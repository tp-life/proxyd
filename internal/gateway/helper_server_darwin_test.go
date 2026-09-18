//go:build darwin

package gateway

// helper 服务端 darwin 执行能力与安装链路单测（fake runCommand/文件 IO）。

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// newFakeDarwinOps 构造带 fake 命令/文件读写的执行能力，返回记录器。
func newFakeDarwinOps(t *testing.T) (*darwinHelperOps, *[]string, *map[string]string) {
	t.Helper()
	commands := &[]string{}
	files := &map[string]string{}
	ops := newDarwinHelperOps(nil)
	ops.run = func(name string, args ...string) (string, error) {
		*commands = append(*commands, name+" "+strings.Join(args, " "))
		if name == "/usr/sbin/sysctl" && len(args) >= 2 && args[0] == "-n" {
			if v, ok := (*files)["sysctl:"+args[1]]; ok {
				return v + "\n", nil
			}
			return "0\n", nil
		}
		return "", nil
	}
	ops.writeFile = func(path, content string) error {
		(*files)[path] = content
		return nil
	}
	ops.readFile = func(path string) (string, error) {
		if v, ok := (*files)[path]; ok {
			return v, nil
		}
		return "", errors.New("no such file")
	}
	return ops, commands, files
}

func TestBuildGatewayPFConfInsertsAfterAppleAnchor(t *testing.T) {
	base := "scrub-anchor \"com.apple/*\"\nnat-anchor \"com.apple/*\"\nrdr-anchor \"com.apple/*\"\ndummynet-anchor \"com.apple/*\"\nanchor \"com.apple/*\"\nload anchor \"com.apple\" from \"/etc/pf.anchors/com.apple\"\n"
	combined := buildGatewayPFConf(base)
	if !strings.Contains(combined, "rdr-anchor \"com.proxyd.gateway\"") {
		t.Fatal("缺少 rdr-anchor 引用")
	}
	if !strings.Contains(combined, "load anchor \"com.proxyd.gateway\" from \"/etc/pf.anchors/com.proxyd.gateway\"") {
		t.Fatal("缺少 load anchor")
	}
	// 插入位置在 com.apple rdr-anchor 之后、filter 段之前。
	rdrApple := strings.Index(combined, "rdr-anchor \"com.apple/*\"")
	rdrOurs := strings.Index(combined, "rdr-anchor \"com.proxyd.gateway\"")
	if !(rdrOurs > rdrApple) {
		t.Errorf("插入位置错误:\n%s", combined)
	}
	// 幂等：重复构建不重复追加。
	if again := buildGatewayPFConf(combined); again != combined {
		t.Error("重复构建不幂等")
	}
	// 无 com.apple 段的自定义配置：追加到末尾。
	custom := buildGatewayPFConf("pass all\n")
	if !strings.HasSuffix(custom, "load anchor \"com.proxyd.gateway\" from \""+gatewayAnchorFile+"\"\n") {
		t.Errorf("自定义配置应追加到末尾:\n%s", custom)
	}
}

func TestDarwinOpsApplyPFSequence(t *testing.T) {
	ops, commands, files := newFakeDarwinOps(t)
	(*files)["/etc/pf.conf"] = "rdr-anchor \"com.apple/*\"\nanchor \"com.apple/*\"\n"
	if err := ops.ApplyPF("rdr pass inet proto tcp from { 192.168.1.10 } to any -> 127.0.0.1 port 17892\n"); err != nil {
		t.Fatalf("ApplyPF: %v", err)
	}
	if !ops.Applied() {
		t.Fatal("Applied 应为 true")
	}
	anchor := (*files)[gatewayAnchorFile]
	if !strings.Contains(anchor, "192.168.1.10") {
		t.Errorf("anchor 文件内容异常: %q", anchor)
	}
	combined := (*files)[gatewayCombinedPFConf]
	if !strings.Contains(combined, "com.proxyd.gateway") {
		t.Errorf("合并配置缺 anchor 引用: %q", combined)
	}
	want := []string{
		"/sbin/pfctl -e",
		"/sbin/pfctl -f " + gatewayCombinedPFConf,
		"/sbin/pfctl -a com.proxyd.gateway -f " + gatewayAnchorFile,
	}
	if len(*commands) != len(want) {
		t.Fatalf("命令序列 = %v", *commands)
	}
	for i := range want {
		if (*commands)[i] != want[i] {
			t.Errorf("commands[%d] = %q, want %q", i, (*commands)[i], want[i])
		}
	}
}

func TestDarwinOpsApplyPFToleratesAlreadyEnabled(t *testing.T) {
	ops, _, files := newFakeDarwinOps(t)
	(*files)["/etc/pf.conf"] = "rdr-anchor \"com.apple/*\"\n"
	origRun := ops.run
	ops.run = func(name string, args ...string) (string, error) {
		if name == "/sbin/pfctl" && len(args) == 1 && args[0] == "-e" {
			return "pfctl: pf already enabled", fmt.Errorf("exit status 1")
		}
		return origRun(name, args...)
	}
	if err := ops.ApplyPF("rdr pass inet proto tcp from any to any -> 127.0.0.1 port 17892\n"); err != nil {
		t.Fatalf("pf 已启用不应失败: %v", err)
	}
}

func TestDarwinOpsForwardMemoryAndRestore(t *testing.T) {
	ops, commands, files := newFakeDarwinOps(t)
	// 原值 0：首次开启记录并设置。
	if err := ops.SetForward(true); err != nil {
		t.Fatalf("SetForward(true): %v", err)
	}
	if (*files)[helperForwardOrigPath] != "0" {
		t.Errorf("原值记录 = %q", (*files)[helperForwardOrigPath])
	}
	// ClearPF 恢复原值 0。
	if err := ops.ClearPF(); err != nil {
		t.Fatalf("ClearPF: %v", err)
	}
	var sysctlWrites []string
	for _, cmd := range *commands {
		if strings.Contains(cmd, "sysctl -w") {
			sysctlWrites = append(sysctlWrites, cmd)
		}
	}
	if len(sysctlWrites) != 2 || !strings.HasSuffix(sysctlWrites[0], "=1") || !strings.HasSuffix(sysctlWrites[1], "=0") {
		t.Errorf("sysctl 写序列 = %v", sysctlWrites)
	}
	if ops.Applied() {
		t.Error("ClearPF 后 Applied 应为 false")
	}

	// 原值 1（用户已自行开启）：恢复回 1 而非 0。
	ops2, _, files2 := newFakeDarwinOps(t)
	(*files2)["sysctl:net.inet.ip.forwarding"] = "1"
	if err := ops2.SetForward(true); err != nil {
		t.Fatalf("SetForward(true): %v", err)
	}
	if (*files2)[helperForwardOrigPath] != "1" {
		t.Errorf("原值记录 = %q", (*files2)[helperForwardOrigPath])
	}
}

func TestHelperUIDAuthorization(t *testing.T) {
	if !authorizedHelperUID(0, 501) {
		t.Error("root 应放行")
	}
	if !authorizedHelperUID(501, 501) {
		t.Error("属主应放行")
	}
	if authorizedHelperUID(502, 501) {
		t.Error("其他用户应拒绝")
	}
}

// TestHelperUninstallCleanupCommands 验证注册到统一助手的卸载清理链：
// 清 pf anchor、删除 socket/anchor/状态文件。
// 参数：t 为 *testing.T。返回：无。错误：清理项缺失时失败。
func TestHelperUninstallCleanupCommands(t *testing.T) {
	joined := fmt.Sprint(helperUninstallCleanupCommands())
	for _, want := range []string{"pfctl", helperSocketPath, helperForwardOrigPath, gatewayAnchorFile, gatewayCombinedPFConf} {
		if !strings.Contains(joined, want) {
			t.Errorf("卸载清理缺 %q: %s", want, joined)
		}
	}
}
