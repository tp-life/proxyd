//go:build linux

package autostart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLinuxRootUsesSystemScope 验证 root 运行时写系统级 unit 且 systemctl 不带 --user。
func TestLinuxRootUsesSystemScope(t *testing.T) {
	origScope, origDir, origRun := systemScope, systemUnitDir, run
	t.Cleanup(func() { systemScope, systemUnitDir, run = origScope, origDir, origRun })

	systemScope = func() bool { return true }
	systemUnitDir = t.TempDir()
	var calls []string
	run = func(name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return "", nil
	}

	if err := on(Options{Exe: "/usr/local/bin/proxyd", ConfigPath: "/root/.config/proxyd/config.yaml"}); err != nil {
		t.Fatalf("on: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(systemUnitDir, unitName))
	if err != nil {
		t.Fatalf("读取 unit: %v", err)
	}
	if !strings.Contains(string(data), "WantedBy=multi-user.target") {
		t.Errorf("应写入系统级 unit:\n%s", data)
	}
	if enabled, _ := status(); !enabled {
		t.Error("status 应为已开启")
	}
	if err := off(); err != nil {
		t.Fatalf("off: %v", err)
	}
	for _, c := range calls {
		if strings.Contains(c, "--user") {
			t.Errorf("root 场景不应调用 --user: %q", c)
		}
	}
	want := []string{"systemctl daemon-reload", "systemctl enable --now proxyd.service", "systemctl disable proxyd.service", "systemctl daemon-reload"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Errorf("systemctl 调用不符:\n got %q\nwant %q", calls, want)
	}
}

// TestLinuxUserScopeFillsRuntimeDir 验证用户级缺少 XDG_RUNTIME_DIR 时经 env 补上运行目录。
func TestLinuxUserScopeFillsRuntimeDir(t *testing.T) {
	origScope, origDir, origRun := systemScope, userRuntimeDir, run
	t.Cleanup(func() { systemScope, userRuntimeDir, run = origScope, origDir, origRun })

	systemScope = func() bool { return false }
	var got string
	run = func(name string, args ...string) (string, error) {
		got = name + " " + strings.Join(args, " ")
		return "", nil
	}

	userRuntimeDir = func() string { return "/run/user/1000" }
	_, _ = systemctl("daemon-reload")
	if want := "env XDG_RUNTIME_DIR=/run/user/1000 systemctl --user daemon-reload"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	userRuntimeDir = func() string { return "" }
	_, _ = systemctl("daemon-reload")
	if want := "systemctl --user daemon-reload"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
