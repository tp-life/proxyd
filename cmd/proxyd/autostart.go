package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"proxyd/internal/autostart"
	"proxyd/internal/config"
)

// cmdAutostart 开关/查看开机自启（直接操作 OS 自启项，无需实例运行）。
//
// 参数说明：
//   - args: []string，包含可选 -c 配置路径及 on、off、status 操作。
//
// 返回值说明：error，参数、配置解析和系统服务操作全部成功时为 nil。
//
// 错误情况：参数非法、配置不可读、首次 api-secret 引导、路径解析或平台自启项操作
// 失败时返回错误；macOS 安装/移除 LaunchDaemon 时系统会请求管理员授权。
func cmdAutostart(args []string) error {
	fs := flag.NewFlagSet("autostart", flag.ExitOnError)
	cfgFile := fs.String("c", config.DefaultPath(), "配置文件路径")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("用法: proxyd autostart [-c 配置文件] on|off|status")
	}
	switch fs.Arg(0) {
	case "on":
		// LaunchDaemon 启动后没有交互 stdin，因此必须在当前前台命令中完成
		// api-secret 首次录入和落盘，重启后服务只读取已保存值。
		cfg, err := config.LoadForAPISecretBootstrap(*cfgFile)
		if err != nil {
			return err
		}
		if err := ensureAPISecret(cfg, *cfgFile); err != nil {
			return err
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if exe, err = filepath.Abs(exe); err != nil {
			return err
		}
		cfgAbs, err := filepath.Abs(*cfgFile)
		if err != nil {
			return err
		}
		if err := autostart.On(autostart.Options{Exe: exe, ConfigPath: cfgAbs, StateDir: cfg.StateDir}); err != nil {
			return err
		}
		fmt.Println("开机自启已开启")
	case "off":
		if err := autostart.Off(); err != nil {
			return err
		}
		fmt.Println("开机自启已关闭")
	case "status":
		on, err := autostart.Status()
		if err != nil {
			return err
		}
		if on {
			fmt.Println("开机自启：开启")
		} else {
			fmt.Println("开机自启：关闭")
		}
		fmt.Println("服务状态：" + autostart.Inspect().Message)
	default:
		return fmt.Errorf("未知操作 %q，用法: proxyd autostart [-c 配置文件] on|off|status", fs.Arg(0))
	}
	return nil
}
