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
	if len(*commands) != 5 {
		t.Fatalf("命令数 = %d", len(*commands))
	}
	for index, want := range []string{
		"ip -4 route replace local 0.0.0.0/0 dev lo table 20260",
		"ip -4 rule del priority 12026 fwmark 0x7078/0xffff lookup 20260",
		"ip -4 rule add priority 12026 fwmark 0x7078/0xffff lookup 20260",
	} {
		call := (*commands)[index]
		if got := call.name + " " + strings.Join(call.args, " "); got != want {
			t.Errorf("命令[%d] = %q, 期望 %q", index, got, want)
		}
	}
	clearCall := (*commands)[3]
	if clearCall.name != "nft" || strings.Join(clearCall.args, " ") != "delete table inet proxyd-gw" {
		t.Errorf("替换前清理调用 = %s %v", clearCall.name, clearCall.args)
	}
	call := (*commands)[4]
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

// TestNFTRunnerApplyFailureCleansDataplane 验证新 ruleset 加载失败后会删除可能残留的
// nft table 与策略路由，避免进程启动失败时留下仍截获流量的旧规则。
//
// 参数：t 由 testing 注入，用于替换命令执行器并检查失败后的清理顺序。
// 返回值：无。
// 错误情况：Apply 未返回错误，或失败后没有执行 nft/rule/route 清理时测试失败。
func TestNFTRunnerApplyFailureCleansDataplane(t *testing.T) {
	commands, _ := fakeExec(t)
	nftLoads := 0
	runCommand = func(input, name string, args ...string) (string, error) {
		*commands = append(*commands, recordedCommand{input, name, args})
		if name == "nft" && strings.Join(args, " ") == "-f -" {
			nftLoads++
			return "syntax error", errors.New("exit status 1")
		}
		return "", nil
	}

	err := (nftRunner{}).Apply("invalid ruleset")
	if err == nil || !strings.Contains(err.Error(), "应用 nftables 规则失败") {
		t.Fatalf("Apply 错误 = %v", err)
	}
	if nftLoads != 1 {
		t.Fatalf("ruleset 加载次数 = %d, 期望 1", nftLoads)
	}
	if len(*commands) != 8 {
		t.Fatalf("调用数 = %d, 期望准备 3 + 替换前删除 1 + 加载 1 + 失败清理 3", len(*commands))
	}
	last := (*commands)[len(*commands)-3:]
	if last[0].name != "nft" || last[1].name != "ip" || last[2].name != "ip" {
		t.Fatalf("失败清理顺序 = %+v", last)
	}
}

func TestNFTRunnerClearIdempotent(t *testing.T) {
	commands, _ := fakeExec(t)
	runner := nftRunner{}
	if err := runner.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if len(*commands) != 3 {
		t.Fatalf("清理命令数 = %d, 期望 nft + rule + route 共 3 条", len(*commands))
	}
	call := (*commands)[0]
	if call.name != "nft" || strings.Join(call.args, " ") != "delete table inet proxyd-gw" {
		t.Errorf("调用 = %s %v", call.name, call.args)
	}
	if got := (*commands)[1].name + " " + strings.Join((*commands)[1].args, " "); got != "ip -4 rule del priority 12026 fwmark 0x7078/0xffff lookup 20260" {
		t.Errorf("策略规则清理命令 = %q", got)
	}
	if got := (*commands)[2].name + " " + strings.Join((*commands)[2].args, " "); got != "ip -4 route del local 0.0.0.0/0 dev lo table 20260" {
		t.Errorf("本地路由清理命令 = %q", got)
	}
	// 表不存在时的 nft 报错应视为清除成功。
	origRun := runCommand
	runCommand = func(input, name string, args ...string) (string, error) {
		return "Error: target does not exist: No such file or directory", fmt.Errorf("exit status 1")
	}
	defer func() { runCommand = origRun }()
	if err := runner.Clear(); err != nil {
		t.Fatalf("表不存在时 Clear 应成功: %v", err)
	}
}

// TestGatewayCommandPropagatesNetAdmin 验证外部 nft/ip 子进程声明 ambient
// CAP_NET_ADMIN，防止 setcap 只在父进程预检成功、子进程实际执行仍无权限。
//
// 参数：t 由 testing 注入，用于报告 SysProcAttr 断言失败。
// 返回值：无。
// 错误情况：未设置 SysProcAttr 或 capability 列表不精确时测试失败。
func TestGatewayCommandPropagatesNetAdmin(t *testing.T) {
	cmd := newGatewayCommand("nft", "list", "tables")
	if cmd.SysProcAttr == nil {
		t.Fatal("网关子进程缺少 SysProcAttr")
	}
	if len(cmd.SysProcAttr.AmbientCaps) != 1 || cmd.SysProcAttr.AmbientCaps[0] != capNetAdmin {
		t.Fatalf("AmbientCaps = %v, 期望仅 CAP_NET_ADMIN", cmd.SysProcAttr.AmbientCaps)
	}
}

// TestApplyPolicyRoutingFailureCleansPartialState 验证策略规则安装失败时会清除已写入的
// 本地路由，且不会继续启用 nftables，避免启动失败后留下半套数据面。
//
// 参数：t 由 testing 注入，用于安装 fake 命令执行器并检查调用序列。
// 返回值：无。
// 错误情况：预期失败未返回、仍调用 nft 或未执行 rule/route 清理时测试失败。
func TestApplyPolicyRoutingFailureCleansPartialState(t *testing.T) {
	commands, _ := fakeExec(t)
	runCommand = func(input, name string, args ...string) (string, error) {
		*commands = append(*commands, recordedCommand{input, name, args})
		joined := strings.Join(args, " ")
		if name == "ip" && strings.HasPrefix(joined, "-4 rule add") {
			return "RTNETLINK answers: Operation not permitted", errors.New("exit status 2")
		}
		return "", nil
	}

	err := (nftRunner{}).Apply("table inet proxyd-gw {}")
	if err == nil || !strings.Contains(err.Error(), "安装 TPROXY 策略规则失败") {
		t.Fatalf("Apply 错误 = %v", err)
	}
	for _, call := range *commands {
		if call.name == "nft" && strings.Join(call.args, " ") == "-f -" {
			t.Fatalf("策略路由失败后不应加载 nftables ruleset: %+v", *commands)
		}
	}
	if len(*commands) != 6 {
		t.Fatalf("调用数 = %d, 期望 route replace、rule del/add 及执行层完整清理 3 条", len(*commands))
	}
	last := (*commands)[len(*commands)-3:]
	if last[0].name != "nft" || last[1].name != "ip" || last[2].name != "ip" {
		t.Fatalf("策略路由失败后的完整清理顺序 = %+v", last)
	}
}

// TestPolicyRoutingPresentRequiresRuleAndRoute 验证执行层状态只有在 fwmark 规则与
// lo 本地路由同时存在时才报告就绪，避免控制台把半配置误报为可用。
//
// 参数：t 由 testing 注入，用于替换只读命令输出。
// 返回值：无。
// 错误情况：完整状态未识别，或缺少任一对象仍返回 true 时测试失败。
func TestPolicyRoutingPresentRequiresRuleAndRoute(t *testing.T) {
	_, _ = fakeExec(t)
	runCommand = func(input, name string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "rule show"):
			return "12026: from all fwmark 0x7078/0xffff lookup 20260", nil
		case strings.Contains(joined, "route show"):
			return "local default dev lo scope host", nil
		default:
			return "", nil
		}
	}
	if !policyRoutingPresent() {
		t.Fatal("完整策略规则与本地路由应判定为 present")
	}
	runCommand = func(input, name string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "rule show") {
			return "12026: from all fwmark 0x7078/0xffff lookup 20260", nil
		}
		return "", errors.New("route missing")
	}
	if policyRoutingPresent() {
		t.Fatal("缺少本地路由时不应判定为 present")
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
