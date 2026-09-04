package main

// 本文件负责管理面 api-secret 的首次交互引导。
// 它属于 CLI 适配层：配置模型只表达和校验口令，是否向人类终端提问由进程入口决定。

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"

	"proxyd/internal/config"
)

const (
	// minimumPromptedAPISecretRunes 是交互创建口令的最低字符数。
	// 已有配置保持向后兼容；首次引导接受至少 6 个字符，在便于输入的同时
	// 仍拒绝明显为空或过短的口令。
	minimumPromptedAPISecretRunes = 6
	// maximumAPISecretPromptAttempts 限制空值、过短或两次输入不一致的重试次数，
	// 防止脚本误进入交互分支后无限等待。
	maximumAPISecretPromptAttempts = 3
)

// apiSecretPromptIO 封装密码终端交互所需的最小能力，便于测试成功、取消和无终端分支。
type apiSecretPromptIO struct {
	inputFD      int
	output       io.Writer
	isTerminal   func(fd int) bool
	readPassword func(fd int) ([]byte, error)
}

// defaultAPISecretPromptIO 返回连接当前进程 stdin/stderr 的生产交互实现。
//
// 参数说明：无。
//
// 返回值说明：apiSecretPromptIO；密码从 stdin 读取且不回显，提示写到 stderr，
// 避免污染命令可能输出到 stdout 的结构化结果。
//
// 错误情况：构造阶段不执行 I/O，不返回错误；终端检测和读取错误由调用阶段处理。
func defaultAPISecretPromptIO() apiSecretPromptIO {
	return apiSecretPromptIO{
		inputFD:      int(os.Stdin.Fd()),
		output:       os.Stderr,
		isTerminal:   term.IsTerminal,
		readPassword: term.ReadPassword,
	}
}

// ensureAPISecret 在首次交互启动时补录并持久化管理面口令。
//
// 参数说明：
//   - cfg: *config.Config，已经完成首次启动宽松加载、尚未启动网络监听的配置。
//   - cfgPath: string，口令需要写回的配置文件路径。
//
// 返回值说明：error，口令已存在、回环地址在无终端环境安全跳过，或新口令保存成功时为 nil。
//
// 错误情况：非回环监听在无交互终端时拒绝继续；密码读取、确认、完整配置校验或原子保存
// 失败时返回错误。失败前会恢复内存中的旧值，避免调用方误以为口令已经持久化。
func ensureAPISecret(cfg *config.Config, cfgPath string) error {
	return ensureAPISecretWithPrompt(cfg, cfgPath, defaultAPISecretPromptIO())
}

// ensureAPISecretWithPrompt 执行可测试的 api-secret 首次录入流程。
//
// 参数说明：
//   - cfg: *config.Config，待补录口令的配置。
//   - cfgPath: string，原子保存目标路径。
//   - prompt: apiSecretPromptIO，终端检测、隐藏输入和提示输出适配器。
//
// 返回值说明：error，配置可安全继续启动且所需口令已落盘时为 nil。
//
// 错误情况：配置或适配器无效、无终端非回环暴露、连续三次输入不合格、读取失败、
// 完整校验失败或保存失败时返回错误；任何错误都不会保留未落盘的新口令。
func ensureAPISecretWithPrompt(cfg *config.Config, cfgPath string, prompt apiSecretPromptIO) error {
	if cfg == nil {
		return fmt.Errorf("无法初始化 api-secret：配置为空")
	}
	if strings.TrimSpace(cfg.APISecret) != "" {
		return nil
	}
	if prompt.output == nil || prompt.isTerminal == nil || prompt.readPassword == nil {
		return fmt.Errorf("无法初始化 api-secret：终端交互适配器不完整")
	}
	if !prompt.isTerminal(prompt.inputFD) {
		if !config.IsLoopbackAPIListen(cfg.APIListen) {
			return fmt.Errorf("非回环 api-listen %q 尚未配置 api-secret，且当前没有交互终端；请先在配置文件中设置 api-secret", cfg.APIListen)
		}
		// LaunchDaemon、后台 serve 与自动重启没有可交互 stdin。回环监听没有远程暴露，
		// 因此允许旧配置继续运行；用户下次从终端启动时仍会再次进入首次引导。
		return nil
	}
	if strings.TrimSpace(cfgPath) == "" {
		return fmt.Errorf("无法保存 api-secret：配置文件路径为空")
	}

	fmt.Fprintln(prompt.output, "首次运行检测到尚未配置 api-secret；该口令用于保护 Web 控制台、管理 API 和 Web Terminal。")
	for attempt := 1; attempt <= maximumAPISecretPromptAttempts; attempt++ {
		secret, err := readPromptedAPISecret(prompt, fmt.Sprintf("请输入 api-secret（至少 %d 个字符，输入不回显）: ", minimumPromptedAPISecretRunes))
		if err != nil {
			return fmt.Errorf("读取 api-secret 失败: %w", err)
		}
		if utf8.RuneCountInString(secret) < minimumPromptedAPISecretRunes {
			fmt.Fprintf(prompt.output, "api-secret 至少需要 %d 个字符，请重新输入。\n", minimumPromptedAPISecretRunes)
			continue
		}
		confirmed, err := readPromptedAPISecret(prompt, "请再次输入 api-secret: ")
		if err != nil {
			return fmt.Errorf("确认 api-secret 失败: %w", err)
		}
		if secret != confirmed {
			fmt.Fprintln(prompt.output, "两次输入不一致，请重新输入。")
			continue
		}

		oldSecret := cfg.APISecret
		cfg.APISecret = secret
		if err := cfg.Validate(); err != nil {
			cfg.APISecret = oldSecret
			return fmt.Errorf("保存 api-secret 前配置校验失败: %w", err)
		}
		if err := cfg.Save(cfgPath); err != nil {
			cfg.APISecret = oldSecret
			return fmt.Errorf("保存 api-secret 到 %s 失败: %w", cfgPath, err)
		}
		fmt.Fprintf(prompt.output, "api-secret 已保存到 %s，后续启动不会再次提示。\n", cfgPath)
		return nil
	}
	return fmt.Errorf("连续 %d 次未输入有效且一致的 api-secret", maximumAPISecretPromptAttempts)
}

// readPromptedAPISecret 输出一次提示并读取不回显的密码文本。
//
// 参数说明：
//   - prompt: apiSecretPromptIO，提供输出目标、终端 fd 和隐藏读取函数。
//   - message: string，本次输入前显示的提示文字。
//
// 返回值说明：string 为去除首尾空白后的口令，error 为底层终端读取错误。
//
// 错误情况：读取失败时仍补写换行，保持后续错误信息不会粘在密码提示行末；
// 空白口令返回空字符串，由上层统一按长度规则处理。
func readPromptedAPISecret(prompt apiSecretPromptIO, message string) (string, error) {
	fmt.Fprint(prompt.output, message)
	value, err := prompt.readPassword(prompt.inputFD)
	fmt.Fprintln(prompt.output)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(value)), nil
}
