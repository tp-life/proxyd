package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"proxyd/internal/config"
)

// 本文件处理“曾用 sudo/其他用户运行导致 state-dir、配置文件属主异常”的启动期修复：
// 探测到权限不足（EACCES）且 stdin 是终端时，交互确认后执行一次 sudo chown -R
// 把属主归还给当前用户，随后继续以普通用户运行——避免整个代理核心长期持有 root
// 权限，也避免 root 运行再次产生 root 所有的文件形成循环。

// isPermission 判断错误链中是否包含权限不足（EACCES/EPERM）。
func isPermission(err error) bool {
	return errors.Is(err, fs.ErrPermission)
}

// isTerminal 探测 stdin 是否为可交互终端；声明为变量以便测试替换。
var isTerminal = func() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// sudoRunner 执行提权命令，标准流直接挂到终端，密码提示由 sudo 自己完成；测试中可替换。
var sudoRunner = func(args ...string) error {
	cmd := exec.Command("sudo", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// confirmRepair 向用户确认是否执行属主修复；声明为变量以便测试替换。
var confirmRepair = func(paths []string) bool {
	fmt.Println("检测到以下路径权限不足（通常因曾用 sudo 或其他用户运行 proxyd）：")
	for _, p := range paths {
		fmt.Printf("  - %s\n", p)
	}
	fmt.Printf("将执行 sudo chown -R %d:%d 把属主归还给当前用户（需要输入登录密码），现在修复？[Y/n] ", os.Getuid(), os.Getgid())
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "" || ans == "y" || ans == "yes"
}

// probeWritable 探测路径可写性：目录尝试创建临时文件；已存在的文件尝试读写打开；
// 不存在的文件探测父目录（原子替换写盘依赖父目录可写）。
func probeWritable(path string) error {
	st, err := os.Stat(path)
	switch {
	case err == nil && st.IsDir():
		f, err := os.CreateTemp(path, ".proxyd-write-probe-*")
		if err != nil {
			return err
		}
		_ = f.Close()
		return os.Remove(f.Name())
	case err == nil:
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		return f.Close()
	case os.IsNotExist(err):
		return probeWritable(filepath.Dir(path))
	default:
		return err
	}
}

// nearestExistingDir 返回路径向上第一个已存在的目录，用于 state-dir 父链本身
// 属主异常时确定 chown 目标。
func nearestExistingDir(path string) string {
	for {
		if st, err := os.Stat(path); err == nil && st.IsDir() {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

// offerOwnershipRepair 检查 paths 的可写性，存在权限不足时交互确认并用 sudo chown
// 一次性修复属主，修复后重新探测确认。
//
// 参数：
//   - paths: ...string，需要当前用户可写的目录或文件；空串忽略，重复自动去重。
//
// 返回值：
//   - error：全部可写返回 nil；非权限类探测错误、非终端环境、用户取消、sudo 失败
//     或修复后仍不可写时返回错误，文本中附带可手动执行的 chown 命令。
//
// 错误情况：开机自启等非终端场景无法交互读密码，直接返回带指引的错误，由日志/启动器呈现。
func offerOwnershipRepair(paths ...string) error {
	blocked := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		err := probeWritable(p)
		if err == nil {
			continue
		}
		if !isPermission(err) {
			return fmt.Errorf("检查 %s 可写性失败: %w", p, err)
		}
		blocked = append(blocked, p)
	}
	if len(blocked) == 0 {
		return nil
	}

	ownership := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	fixCmd := fmt.Sprintf("sudo chown -R %s %s", ownership, strings.Join(blocked, " "))
	if !isTerminal() {
		return fmt.Errorf("权限不足：%s 不可写；请执行 %s 后重试", strings.Join(blocked, "、"), fixCmd)
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return fmt.Errorf("权限不足：%s 不可写，且未找到 sudo；请用其他方式修复属主后重试", strings.Join(blocked, "、"))
	}
	if !confirmRepair(blocked) {
		return fmt.Errorf("已取消属主修复；请手动执行 %s 后重试", fixCmd)
	}
	args := append([]string{"chown", "-R", ownership}, blocked...)
	if err := sudoRunner(args...); err != nil {
		return fmt.Errorf("sudo 修复属主失败: %w", err)
	}
	for _, p := range blocked {
		if err := probeWritable(p); err != nil {
			return fmt.Errorf("属主修复后 %s 仍不可写: %w", p, err)
		}
	}
	log.Printf("[proxyd] 已修复路径属主: %s", strings.Join(blocked, "、"))
	return nil
}

// configPathFromArgs 单独解析 -c 标志，供 loadConfig 因权限失败时定位配置文件。
// flag 解析遇位置参数停止，与 loadConfig 的解析行为保持一致。
func configPathFromArgs(args []string) string {
	cfgPath := config.DefaultPath()
	for i, a := range args {
		if a == "-c" && i+1 < len(args) {
			return args[i+1]
		}
		if !strings.HasPrefix(a, "-") {
			break
		}
	}
	return cfgPath
}

// offerConfigRepair 在配置加载因权限失败时，修复配置文件及其所在目录的属主。
func offerConfigRepair(args []string) error {
	cfgPath := configPathFromArgs(args)
	return offerOwnershipRepair(nearestExistingDir(filepath.Dir(cfgPath)), cfgPath)
}

// offerStateDirRepair 探测 state-dir 可写性，权限不足时修复最近已存在的祖先目录。
// 非权限类问题（如磁盘错误）不拦截，保持原有的宽容启动行为。
func offerStateDirRepair(cfg *config.Config) error {
	if cfg.StateDir == "" {
		return nil
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		if !isPermission(err) {
			return nil
		}
		return offerOwnershipRepair(nearestExistingDir(cfg.StateDir))
	}
	if err := probeWritable(cfg.StateDir); err != nil {
		if !isPermission(err) {
			return nil
		}
		return offerOwnershipRepair(cfg.StateDir)
	}
	// 目录可写不代表其中的既有文件可写：曾用 sudo 运行时 proxyd.log、proxyd.pid、
	// geo 数据等会以 root 属主残留，目录本身（尤其 777）探测会通过，
	// 但追加写日志/换 pid 文件仍会被拒。逐个探测顶层条目，修复所有不可写项。
	blocked, err := unwritableEntries(cfg.StateDir)
	if err != nil || len(blocked) == 0 {
		return nil
	}
	return offerOwnershipRepair(blocked...)
}

// unwritableEntries 返回 dir 顶层当前用户不可写的条目（文件或子目录）；全部可写返回空。
func unwritableEntries(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var blocked []string
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if perr := probeWritable(p); perr != nil && isPermission(perr) {
			blocked = append(blocked, p)
		}
	}
	return blocked, nil
}

// loadConfigOrRepair 包装 loadConfig：配置读取因权限不足失败时，先尝试交互修复
// 属主再重试一次；修复未执行（非终端/用户取消）时返回原始错误，保留完整上下文。
// persist 模式下还会探测配置文件可写性——可读但不可写时后续持久化只会静默失败，
// 因此同样提前修复；权限确认后执行 api-secret 首次交互引导并原子保存。
//
// 参数说明：
//   - args: []string，serve/start 的配置与订阅参数。
//   - persist: bool，是否允许配置合并、权限修复和首次口令保存。
//
// 返回值说明：*config.Config、配置路径和 error；成功配置可直接交给启动用例。
//
// 错误情况：配置读取、权限修复、可写性探测或 api-secret 引导失败时返回错误；
// 无交互终端的非回环配置会被拒绝，避免后台进程启动后才发现管理面无法安全监听。
func loadConfigOrRepair(args []string, persist bool) (*config.Config, string, error) {
	cfg, cfgPath, err := loadConfig(args, persist)
	if err != nil {
		if !isPermission(err) {
			return nil, "", err
		}
		if rerr := offerConfigRepair(args); rerr != nil {
			return nil, "", err
		}
		cfg, cfgPath, err = loadConfig(args, persist)
		if err != nil {
			return nil, "", err
		}
	}
	if persist && cfgPath != "" {
		if perr := probeWritable(cfgPath); perr != nil && isPermission(perr) {
			if rerr := offerConfigRepair(args); rerr != nil {
				return nil, "", rerr
			}
		}
	}
	if persist {
		if err := ensureAPISecret(cfg, cfgPath); err != nil {
			return nil, "", err
		}
	}
	return cfg, cfgPath, nil
}
