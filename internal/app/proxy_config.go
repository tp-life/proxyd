package app

// 代理域：配置导出与导入（预检 + 确认）用例。

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"

	"proxyd/internal/config"
)

// ConfigCountChange 描述配置导入前后某类对象数量的变化，不包含 URL、secret 等敏感值。
type ConfigCountChange struct {
	Before int `json:"before"`
	After  int `json:"after"`
}

// ConfigImportPreview 是配置导入预检结果。
//
// Digest 绑定用户确认时看到的原始字节；Counts 和 ChangedFields 只提供影响摘要，
// 不回显订阅 URL、节点凭据、controller secret 等敏感内容。
type ConfigImportPreview struct {
	Digest          string                       `json:"digest"`
	RestartRequired bool                         `json:"restart_required"`
	Counts          map[string]ConfigCountChange `json:"counts"`
	ChangedFields   []string                     `json:"changed_fields"`
	Warnings        []string                     `json:"warnings"`
}

// ExportConfig 导出当前运行配置的 YAML，可选择对凭据打码。
//
// 参数：
//   - maskTokens: bool，true 时隐藏 secret、URL 用户信息和敏感查询参数。
//
// 返回值：
//   - []byte：YAML 配置内容。
//   - error：YAML 序列化失败时返回错误。
//
// 错误情况：导出期间持有配置读锁以获得一致快照；不会读取订阅缓存或修改运行状态。
func (a *App) ExportConfig(maskTokens bool) ([]byte, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.ExportYAML(maskTokens)
}

// PreviewImport 解析并完整校验待导入配置，返回不含凭据的影响摘要和内容摘要，且绝不写盘。
//
// 参数：
//   - raw: []byte，用户选择的完整 YAML 原始字节；确认阶段必须提交完全相同的内容。
//
// 返回值：
//   - ConfigImportPreview，包含 SHA-256 digest、对象数量变化、关键字段变化与安全警告。
//   - error，YAML 或配置结构/业务约束校验失败时返回。
//
// 错误情况：预检只调用 config.Parse 和内存比较，不修改 a.cfg、运行态、缓存或配置文件；
// 因此任何错误都可以安全返回给 UI 修正后重试。
func (a *App) PreviewImport(raw []byte) (ConfigImportPreview, error) {
	next, err := config.Parse(raw)
	if err != nil {
		return ConfigImportPreview{}, fmt.Errorf("导入配置预检失败: %w", err)
	}
	a.mu.RLock()
	current := a.cfg
	preview := ConfigImportPreview{
		Digest:          configImportDigest(raw),
		RestartRequired: true,
		Counts: map[string]ConfigCountChange{
			"subscriptions": {Before: len(current.Subscriptions), After: len(next.Subscriptions)},
			"manual_nodes":  {Before: len(current.ManualNodes), After: len(next.ManualNodes)},
			"groups":        {Before: len(current.Groups), After: len(next.Groups)},
			"custom_rules":  {Before: len(current.CustomRules), After: len(next.CustomRules)},
			"rule_urls":     {Before: len(current.RuleURLs), After: len(next.RuleURLs)},
		},
	}
	if current.Listen != next.Listen {
		preview.ChangedFields = append(preview.ChangedFields, "节点监听地址")
	}
	if current.MixedPort != next.MixedPort {
		preview.ChangedFields = append(preview.ChangedFields, "主代理端口")
	}
	if current.PortRange != next.PortRange {
		preview.ChangedFields = append(preview.ChangedFields, "节点端口区间")
	}
	if current.APIListen != next.APIListen {
		preview.ChangedFields = append(preview.ChangedFields, "管理 API 地址")
	}
	if current.StateDir != next.StateDir {
		preview.ChangedFields = append(preview.ChangedFields, "状态目录")
	}
	if current.ExternalController != next.ExternalController {
		preview.ChangedFields = append(preview.ChangedFields, "mihomo 控制器地址")
	}
	if current.TUN.Enable != next.TUN.Enable {
		preview.ChangedFields = append(preview.ChangedFields, "TUN 开关")
	}
	if current.PortMappingEnabled() != next.PortMappingEnabled() {
		preview.ChangedFields = append(preview.ChangedFields, "节点端口映射开关")
	}
	a.mu.RUnlock()
	if next.Listen != "127.0.0.1" && next.Listen != "::1" && next.Listen != "localhost" {
		preview.Warnings = append(preview.Warnings, "节点监听地址不是回环地址，请确认局域网暴露风险")
	}
	if next.TUN.Enable {
		preview.Warnings = append(preview.Warnings, "导入后将启用 TUN，重启进程需要相应系统权限")
	}
	if len(preview.ChangedFields) == 0 {
		preview.ChangedFields = []string{}
	}
	if len(preview.Warnings) == 0 {
		preview.Warnings = []string{}
	}
	return preview, nil
}

// ImportConfigConfirmed 校验确认摘要并原子写入配置，确保用户确认的内容与最终提交字节一致。
//
// 参数：
//   - raw: []byte，本次要写入的 YAML 原始字节。
//   - expectedDigest: string，PreviewImport 返回的十六进制 SHA-256 摘要。
//
// 返回值：error，摘要缺失/不匹配、配置校验或写盘失败时返回；成功返回 nil。
//
// 错误情况：摘要使用常量时间比较，避免确认后文件内容被替换或浏览器状态过期时误写；
// 摘要通过后仍重新解析校验，不能把预检结果当作绕过最终边界校验的许可证。
func (a *App) ImportConfigConfirmed(raw []byte, expectedDigest string) error {
	expectedDigest = strings.ToLower(strings.TrimSpace(expectedDigest))
	actualDigest := configImportDigest(raw)
	if len(expectedDigest) != len(actualDigest) || subtle.ConstantTimeCompare([]byte(expectedDigest), []byte(actualDigest)) != 1 {
		return fmt.Errorf("配置内容已变化或尚未预检，请重新预览后确认导入")
	}
	return a.ImportConfig(raw)
}

// configImportDigest 计算配置确认协议使用的稳定 SHA-256 十六进制摘要。
//
// 参数：
//   - raw: []byte，未经规范化的 YAML 原始字节。
//
// 返回值：string，64 个小写十六进制字符；换行或空白变化也会产生不同摘要。
//
// 错误情况：无；sha256.Sum256 对任意字节输入都有确定结果。
func configImportDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ImportConfig 校验并原子写入一份完整配置，等待进程重启后生效。
//
// 参数：
//   - raw: []byte，用户上传的完整 YAML 配置，调用方应限制请求体大小。
//
// 返回值：
//   - error：YAML/配置校验失败、当前实例无配置路径或原子写入失败时返回错误。
//
// 错误情况：导入不会直接替换 a.cfg。配置可能改变 api-listen、state-dir 和系统监听
// 端口，在当前 HTTP 请求中热替换会让磁盘地址与运行地址分裂，因此只写磁盘并要求重启。
func (a *App) ImportConfig(raw []byte) error {
	next, err := config.Parse(raw)
	if err != nil {
		return fmt.Errorf("导入配置校验失败: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfgPath == "" {
		return fmt.Errorf("当前实例没有配置文件路径，无法导入；请先使用 proxyd serve -c <配置文件> 启动")
	}
	// 导入与设置页的其他持久化操作共用同一把配置锁，避免两个 Save 同时使用
	// `<config>.tmp`，也避免较早开始的普通设置在导入完成后覆盖整份备份。
	if err := next.Save(a.cfgPath); err != nil {
		return fmt.Errorf("写入导入配置失败: %w", err)
	}
	return nil
}
