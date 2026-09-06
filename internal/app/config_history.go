package app

// 本文件编排配置历史与恢复，完整快照仅通过仓储解密后交给配置校验器。
import (
	"crypto/md5"
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"proxyd/internal/config"
	"proxyd/internal/configversion"
	"proxyd/internal/remote"
	"reflect"
	"sort"
	"strings"
)

// HistoryPreview 绑定恢复目标和当前磁盘配置，避免预检后发生新变更时误覆盖。
type HistoryPreview struct {
	ID              string   `json:"id"`
	Digest          string   `json:"digest"`
	BaseDigest      string   `json:"base_digest"`
	Sections        []string `json:"sections"`
	RestartRequired bool     `json:"restart_required"`
}

// changedSections 仅输出顶层配置段名，不把 URL、token 或对象值写入摘要。
// 参数 before/after 为配置字节；返回有序字段名；解析失败时返回通用提示，不回显输入。
func changedSections(before, after []byte) []string {
	var old, next map[string]any
	if yaml.Unmarshal(before, &old) != nil || yaml.Unmarshal(after, &next) != nil {
		return []string{"配置内容"}
	}
	keys := map[string]bool{}
	for key := range old {
		keys[key] = true
	}
	for key := range next {
		keys[key] = true
	}
	// 差异摘要仅公开 Config 声明的字段名，未知 YAML 键可能是误粘贴的凭据，统一归类。
	known := map[string]bool{}
	configType := reflect.TypeOf(config.Config{})
	for i := 0; i < configType.NumField(); i++ {
		tag := strings.Split(configType.Field(i).Tag.Get("yaml"), ",")[0]
		if tag != "" && tag != "-" {
			known[tag] = true
		}
	}
	unknownChanged := false
	out := []string{}
	for key := range keys {
		if !reflect.DeepEqual(old[key], next[key]) {
			if known[key] {
				out = append(out, key)
			} else {
				unknownChanged = true
			}
		}
	}
	if unknownChanged {
		out = append(out, "其他配置段")
	}
	sort.Strings(out)
	return out
}

// saveWithHistoryLocked 仅在完整配置内容的 MD5 改变时归档当前磁盘配置并原子保存目标。
// 参数 next 为 *config.Config、reason 为 string 摘要；返回 error；归档或写盘错误向事务传播。
// 调用者持有 a.mu，串行完成比较、归档和保存；MD5 仅用于内容去重，不作为凭据或恢复授权校验。
func (a *App) saveWithHistoryLocked(next *config.Config, reason string) error {
	target, err := next.ExportYAML(false)
	if err != nil {
		return err
	}
	current, err := os.ReadFile(a.cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && md5.Sum(current) == md5.Sum(target) {
		return nil
	}
	if err == nil {
		// 归档保留磁盘原文，包括不满足新规则的旧配置，但不能把未校验字段当作安全公开数据。
		// 只有通过完整校验的配置才能生成脱敏副本；其余原文只加密保存，不阻碍本次修复写入。
		redacted := []byte("# 原配置未通过当前版本校验，仅保留加密快照，无法导出脱敏内容。\n")
		if previous, parseErr := parseHistoryConfig(current); parseErr == nil {
			var redactErr error
			redacted, redactErr = previous.ExportYAML(true)
			if redactErr != nil {
				return redactErr
			}
		}
		// 归档成功但主文件写入失败时，重试可能再次遇到相同快照；仅与最新历史比较。
		// 不扫描所有历史，确保 A→B→A→B 这样的真实往返仍保留每次变更前的回滚点。
		duplicate := false
		if versions, listErr := a.configHistory.List(); listErr == nil && len(versions) > 0 {
			if latest, readErr := a.configHistory.Read(versions[0].ID); readErr == nil {
				duplicate = md5.Sum(latest) == md5.Sum(current)
			}
		}
		// 旧历史无法解密时不能据此判断重复，继续归档当前配置；仓储仍会拒绝缺失密钥等错误。
		if !duplicate {
			if _, archiveErr := a.configHistory.Archive(current, redacted, reason, changedSections(current, target)); archiveErr != nil {
				return fmt.Errorf("归档配置历史失败: %w", archiveErr)
			}
		}
	}
	return next.Save(a.cfgPath)
}

// ConfigHistory 返回变更前快照列表；参数无；返回版本/error，不含完整配置与凭据。
func (a *App) ConfigHistory() ([]configversion.Version, error) { return a.configHistory.List() }

// ExportConfigHistory 导出脱敏历史。参数 id 为版本号；返回 YAML/error，不支持完整值下载。
func (a *App) ExportConfigHistory(id string) ([]byte, error) { return a.configHistory.Redacted(id) }

// PreviewConfigRestore 校验历史配置并生成绑定当前磁盘的预览。
// 参数 id 为版本号；返回 HistoryPreview/error，历史缺失、解密或校验失败时不修改配置。
func (a *App) PreviewConfigRestore(id string) (HistoryPreview, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.previewConfigRestoreLocked(id)
}

// previewConfigRestoreLocked 实现已持锁的恢复预检；参数 id 为版本号；返回预览/error。
// 摘要计算基于完整配置但只返回单向摘要；界面显示的差异始终只有字段名。
func (a *App) previewConfigRestoreLocked(id string) (HistoryPreview, error) {
	var out HistoryPreview
	if a.cfgPath == "" {
		return out, fmt.Errorf("当前实例没有配置文件路径")
	}
	raw, err := a.configHistory.Read(id)
	if err != nil {
		return out, err
	}
	if _, err = parseHistoryConfig(raw); err != nil {
		return out, fmt.Errorf("历史配置校验失败")
	}
	current, err := os.ReadFile(a.cfgPath)
	if err != nil {
		return out, err
	}
	return HistoryPreview{ID: id, Digest: configImportDigest(raw), BaseDigest: configImportDigest(current), Sections: changedSections(current, raw), RestartRequired: true}, nil
}

// RestoreConfigVersion 只恢复经过预检且当前配置未变化的历史，成功后要求重启。
// 参数 id/digest/base 为预检响应；返回 error，冲突时拒绝覆盖，恢复前再次归档当前磁盘。
func (a *App) RestoreConfigVersion(id, digest, base string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	preview, err := a.previewConfigRestoreLocked(id)
	if err != nil {
		return err
	}
	if digest == "" || base == "" || digest != preview.Digest || base != preview.BaseDigest {
		return fmt.Errorf("配置或历史已变化，请重新预检")
	}
	raw, err := a.configHistory.Read(id)
	if err != nil {
		return err
	}
	next, err := parseHistoryConfig(raw)
	if err != nil {
		return err
	}
	if err = a.saveWithHistoryLocked(next, "历史恢复前"); err != nil {
		return err
	}
	a.configPendingRestart = true
	return nil
}

// ConfigRestartPending 返回磁盘配置是否等待重启；参数无；返回 bool，读锁避免与恢复事务竞争。
func (a *App) ConfigRestartPending() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.configPendingRestart
}

// parseHistoryConfig 编排配置结构及 SSH 公钥格式校验，保证恢复与公开副本遵循运行模块的输入边界。
// 参数 raw 为完整配置；返回独立 Config/error；纯解析不读取身份文件、不联网、不调和运行状态。
func parseHistoryConfig(raw []byte) (*config.Config, error) {
	cfg, err := config.Parse(raw)
	if err != nil {
		return nil, err
	}
	if _, err = remote.NormalizeSSHKeys(cfg.Remote.SSHKeys); err != nil {
		return nil, fmt.Errorf("历史配置的 SSH 公钥校验失败")
	}
	for _, peer := range cfg.Remote.Remotes {
		if err = remote.ValidateToken(peer.Token); err != nil {
			return nil, fmt.Errorf("历史配置的远端 token 校验失败")
		}
	}
	return cfg, nil
}
