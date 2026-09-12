//go:build linux

package gateway

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// recordedCommand 记录一次 fake runCommand 调用。
type recordedCommand struct {
	input string
	name  string
	args  []string
}

// fakeExec 替换 runCommand/writeSysctl，返回记录器与恢复函数。
func fakeExec(t *testing.T) (*[]recordedCommand, *map[string]string) {
	t.Helper()
	commands := &[]recordedCommand{}
	sysctls := &map[string]string{}
	origRun, origWrite := runCommand, writeSysctl
	origCaps := requireGatewayCaps
	requireGatewayCaps = func() error { return nil }
	runCommand = func(input, name string, args ...string) (string, error) {
		*commands = append(*commands, recordedCommand{input, name, args})
		return "", nil
	}
	writeSysctl = func(path, value string) error {
		(*sysctls)[path] = value
		return nil
	}
	t.Cleanup(func() {
		runCommand, writeSysctl = origRun, origWrite
		requireGatewayCaps = origCaps
	})
	return commands, sysctls
}

func TestNFTRunnerApplyCommandSequence(t *testing.T) {
	commands, sysctls := fakeExec(t)
	m := NewManager(t.TempDir(), nil)
	m.runner = nftRunner{}
	if err := m.Apply(testGatewayConfig()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(*commands) != 1 {
		t.Fatalf("命令数 = %d", len(*commands))
	}
	call := (*commands)[0]
	if call.name != "nft" || strings.Join(call.args, " ") != "-f -" {
		t.Errorf("调用 = %s %v", call.name, call.args)
	}
	if !strings.Contains(call.input, "table inet proxyd-gw") {
		t.Errorf("stdin 未携带 ruleset: %q", call.input)
	}
	if (*sysctls)[ipForwardPath] != "1" {
		t.Errorf("ip_forward 写入 = %q", (*sysctls)[ipForwardPath])
	}
	status := m.Status()
	if !status.Applied || !status.Forwarding || !status.Enabled || status.Platform != "linux" {
		t.Errorf("Status = %+v", status)
	}
}

func TestNFTRunnerClearIdempotent(t *testing.T) {
	commands, _ := fakeExec(t)
	runner := nftRunner{}
	if err := runner.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	call := (*commands)[0]
	if call.name != "nft" || strings.Join(call.args, " ") != "delete table inet proxyd-gw" {
		t.Errorf("调用 = %s %v", call.name, call.args)
	}
	// 表不存在时的 nft 报错应视为清除成功。
	origRun := runCommand
	runCommand = func(input, name string, args ...string) (string, error) {
		return "Error: Could not process rule: No such file or directory", fmt.Errorf("exit status 1")
	}
	defer func() { runCommand = origRun }()
	if err := runner.Clear(); err != nil {
		t.Fatalf("表不存在时 Clear 应成功: %v", err)
	}
}

func TestNFTRunnerForwardingOff(t *testing.T) {
	_, sysctls := fakeExec(t)
	if err := (nftRunner{}).Forwarding(false); err != nil {
		t.Fatalf("Forwarding(false): %v", err)
	}
	if (*sysctls)[ipForwardPath] != "0" {
		t.Errorf("ip_forward 写入 = %q", (*sysctls)[ipForwardPath])
	}
}

func TestRequireGatewayCapsMissing(t *testing.T) {
	if !hasCapability("Name:\tproxyd\nCapEff:\t0000000000000000\n", capNetAdmin) {
		t.Log("CapEff=0 正确判缺")
	} else {
		t.Error("CapEff=0 不应判有 cap_net_admin")
	}
	// 12(cap_net_admin)=0x1000，13(cap_net_raw)=0x2000，10(cap_net_bind_service)=0x400
	full := fmt.Sprintf("CapEff:\t%016x\n", 0x1000|0x2000|0x400)
	for _, cap := range []int{capNetAdmin, capNetRaw, capNetBindService} {
		if !hasCapability(full, cap) {
			t.Errorf("完整位图应判有 capability %d", cap)
		}
	}
	if hasCapability("garbage", capNetAdmin) {
		t.Error("损坏输入应保守判缺")
	}
}

func TestManagerApplyErrorRecorded(t *testing.T) {
	commands, _ := fakeExec(t)
	_ = commands
	origRun := runCommand
	runCommand = func(input, name string, args ...string) (string, error) {
		return "boom", errors.New("exit status 1")
	}
	defer func() { runCommand = origRun }()
	m := NewManager(t.TempDir(), nil)
	m.runner = nftRunner{}
	if err := m.Apply(testGatewayConfig()); err == nil {
		t.Fatal("nft 失败应返回错误")
	}
	status := m.Status()
	if status.Err == "" || status.Applied {
		t.Errorf("失败后 Status = %+v", status)
	}
}
