package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"proxyd/internal/config"
)

// newAPITestConfig 创建带最小有效订阅和默认值的配置，供首次口令引导测试使用。
//
// 参数说明：
//   - t: *testing.T，构造失败时立即终止当前测试。
//
// 返回值说明：*config.Config，默认仅监听回环地址且 api-secret 为空。
//
// 错误情况：config.Quick 意外失败时通过 t.Fatalf 报告，不向调用方返回错误。
func newAPITestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Quick([]string{"https://example.com/sub"}, "")
	if err != nil {
		t.Fatalf("Quick: %v", err)
	}
	return cfg
}

// scriptedAPISecretPrompt 构造确定性的隐藏输入适配器，并返回提示输出和读取次数。
//
// 参数说明：
//   - terminal: bool，模拟 stdin 是否为交互终端。
//   - values: ...string，每次 ReadPassword 应返回的文本。
//
// 返回值说明：apiSecretPromptIO、*bytes.Buffer 和 *int，分别用于执行、检查提示和调用次数。
//
// 错误情况：读取次数超过 values 时返回错误，帮助测试发现意外的重复提示。
func scriptedAPISecretPrompt(terminal bool, values ...string) (apiSecretPromptIO, *bytes.Buffer, *int) {
	output := &bytes.Buffer{}
	reads := 0
	prompt := apiSecretPromptIO{
		inputFD: 0,
		output:  output,
		isTerminal: func(int) bool {
			return terminal
		},
		readPassword: func(int) ([]byte, error) {
			if reads >= len(values) {
				return nil, fmt.Errorf("没有更多脚本输入")
			}
			value := values[reads]
			reads++
			return []byte(value), nil
		},
	}
	return prompt, output, &reads
}

// TestEnsureAPISecretPromptsConfirmsAndPersists 验证首次交互会拒绝过短和不一致输入，
// 最终把确认后的口令保存到配置，并在下一次启动时完全跳过提示。
//
// 参数说明：
//   - t: *testing.T，提供临时目录与断言报告。
//
// 返回值说明：无。
//
// 错误情况：重试、隐藏输入次数、磁盘持久化或后续免提示语义不符合预期时测试失败。
func TestEnsureAPISecretPromptsConfirmsAndPersists(t *testing.T) {
	cfg := newAPITestConfig(t)
	cfg.APIListen = "0.0.0.0:19091"
	path := filepath.Join(t.TempDir(), "config.yaml")
	prompt, output, reads := scriptedAPISecretPrompt(true,
		"short",
		"first-valid-secret", "different-secret",
		"abc123", "abc123",
	)
	if err := ensureAPISecretWithPrompt(cfg, path, prompt); err != nil {
		t.Fatalf("ensureAPISecretWithPrompt: %v", err)
	}
	if cfg.APISecret != "abc123" || *reads != 5 {
		t.Fatalf("首次引导结果 secret=%q reads=%d", cfg.APISecret, *reads)
	}
	if !strings.Contains(output.String(), "至少") || !strings.Contains(output.String(), "两次输入不一致") || !strings.Contains(output.String(), "后续启动不会再次提示") {
		t.Fatalf("首次引导缺少必要反馈:\n%s", output.String())
	}
	reloaded, err := config.Load(path)
	if err != nil || reloaded.APISecret != "abc123" {
		t.Fatalf("保存后的配置无法严格加载: secret=%q err=%v", reloaded.APISecret, err)
	}

	secondPrompt, _, secondReads := scriptedAPISecretPrompt(true)
	if err := ensureAPISecretWithPrompt(reloaded, path, secondPrompt); err != nil {
		t.Fatalf("已有口令不应再次提示: %v", err)
	}
	if *secondReads != 0 {
		t.Fatalf("已有口令仍触发隐藏输入 %d 次", *secondReads)
	}
}

// TestEnsureAPISecretHandlesNonInteractiveStartup 验证后台启动不会等待密码输入：
// 回环监听安全兼容旧配置，非回环监听则要求管理员先显式保存口令。
//
// 参数说明：
//   - t: *testing.T，负责构造两种监听范围并检查结果。
//
// 返回值说明：无。
//
// 错误情况：无终端回环配置被阻断、非回环配置被放行或发生密码读取时测试失败。
func TestEnsureAPISecretHandlesNonInteractiveStartup(t *testing.T) {
	loopback := newAPITestConfig(t)
	prompt, _, reads := scriptedAPISecretPrompt(false)
	if err := ensureAPISecretWithPrompt(loopback, filepath.Join(t.TempDir(), "config.yaml"), prompt); err != nil {
		t.Fatalf("无终端回环启动应安全跳过: %v", err)
	}
	if *reads != 0 {
		t.Fatalf("无终端分支不应读取密码，got %d", *reads)
	}

	nonLoopback := newAPITestConfig(t)
	nonLoopback.APIListen = "0.0.0.0:19091"
	prompt, _, reads = scriptedAPISecretPrompt(false)
	err := ensureAPISecretWithPrompt(nonLoopback, filepath.Join(t.TempDir(), "config.yaml"), prompt)
	if err == nil || !strings.Contains(err.Error(), "没有交互终端") {
		t.Fatalf("无终端非回环启动应明确失败，got %v", err)
	}
	if *reads != 0 {
		t.Fatalf("无终端失败不应尝试读取密码，got %d", *reads)
	}
}

// TestEnsureAPISecretRestoresMemoryWhenSaveFails 验证磁盘保存失败时不会把仅存在于
// 内存的新口令留给后续启动流程，避免健康检查误用尚未持久化的凭据。
//
// 参数说明：
//   - t: *testing.T，创建一个阻塞父目录并检查回滚结果。
//
// 返回值说明：无。
//
// 错误情况：保存失败未返回错误或 cfg.APISecret 未恢复为空时测试失败。
func TestEnsureAPISecretRestoresMemoryWhenSaveFails(t *testing.T) {
	cfg := newAPITestConfig(t)
	blockedParent := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blockedParent, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	prompt, _, _ := scriptedAPISecretPrompt(true, "final-valid-secret", "final-valid-secret")
	err := ensureAPISecretWithPrompt(cfg, filepath.Join(blockedParent, "config.yaml"), prompt)
	if err == nil || !strings.Contains(err.Error(), "保存 api-secret") {
		t.Fatalf("保存失败应返回明确错误，got %v", err)
	}
	if cfg.APISecret != "" {
		t.Fatalf("保存失败后内存口令未回滚: %q", cfg.APISecret)
	}
}
