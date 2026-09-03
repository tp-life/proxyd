//go:build darwin

package sysproxy

import (
	"strings"
	"testing"
)

// stubRun 替换包级 run，记录所有调用并按命令前缀返回预设输出。
func stubRun(t *testing.T, outputs map[string]string) (calls *[]string) {
	t.Helper()
	recorded := []string{}
	orig := run
	run = func(name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		recorded = append(recorded, call)
		for prefix, out := range outputs {
			if strings.Contains(call, prefix) {
				return out, nil
			}
		}
		return "", nil
	}
	t.Cleanup(func() { run = orig })
	return &recorded
}

const servicesList = `An asterisk (*) denotes that a network service is disabled.
Wi-Fi
*Ethernet (disabled)
USB LAN
`

func TestOnCommands(t *testing.T) {
	calls := stubRun(t, map[string]string{"-listallnetworkservices": servicesList})
	if err := On("127.0.0.1", 41999); err != nil {
		t.Fatal(err)
	}
	// 每个活动服务（Wi-Fi、USB LAN）× web/secureweb/socks 三条
	want := 2 * 3
	got := len(*calls) - 1 // 减去 list 调用
	if got != want {
		t.Fatalf("set 命令数 = %d, want %d: %v", got, want, *calls)
	}
	for _, c := range (*calls)[1:] {
		if !strings.Contains(c, "127.0.0.1 41999") {
			t.Errorf("命令缺少代理地址: %s", c)
		}
		if strings.Contains(c, "Ethernet") {
			t.Errorf("停用服务不应被设置: %s", c)
		}
	}
}

func TestOffCommands(t *testing.T) {
	calls := stubRun(t, map[string]string{"-listallnetworkservices": servicesList})
	if err := Off(); err != nil {
		t.Fatal(err)
	}
	for _, c := range (*calls)[1:] {
		if !strings.HasSuffix(c, " off") {
			t.Errorf("off 命令异常: %s", c)
		}
	}
	if len(*calls) != 1+2*3 {
		t.Errorf("命令数 = %d: %v", len(*calls), *calls)
	}
}

func TestStatusAndSnapshot(t *testing.T) {
	calls := stubRun(t, map[string]string{
		"-listallnetworkservices": servicesList,
		"-getwebproxy":            "Enabled: Yes\nServer: 127.0.0.1\nPort: 41999\nAuthenticated Proxy Enabled: 0\n",
		"-getsecurewebproxy":      "Enabled: No\nServer: \nPort: 0\n",
		"-getsocksfirewallproxy":  "Enabled: No\nServer: \nPort: 0\n",
	})
	on, err := Status("127.0.0.1", 41999)
	if err != nil || !on {
		t.Errorf("Status = %v, %v", on, err)
	}
	on, _ = Status("127.0.0.1", 1080)
	if on {
		t.Error("端口不匹配时应为 false")
	}

	snap, err := Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 2*3 {
		t.Fatalf("Snapshot 条数 = %d, want 6", len(snap))
	}
	if !snap[0].Enabled || snap[0].Server != "127.0.0.1" || snap[0].Port != 41999 {
		t.Errorf("Snapshot[0] = %+v", snap[0])
	}

	// Restore：enabled 的先 set 再 on，disabled 的直接 off
	*calls = nil
	if err := Restore(snap); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2*2+4 { // 2 个 enabled（每服务 web）×2 条 + 4 个 disabled ×1 条
		t.Errorf("Restore 命令数 = %d: %v", len(*calls), *calls)
	}
}
