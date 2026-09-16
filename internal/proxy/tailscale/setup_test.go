package tailscale

import "testing"

// TestSetupNormalize 验证一体化接入值对象会补齐默认值、区分两种认证方式并生成
// 精确的 IPv4/IPv6 TUN 规则。
//
// 参数说明：t 是 Go 测试上下文。
//
// 返回值说明：无；断言失败时由 testing 标记用例失败。
//
// 错误情况：默认组名漂移、审批模式接受密钥、TUN 接管范围未规范化或 IPv6 规则
// 类型错误时失败。
func TestSetupNormalize(t *testing.T) {
	approval, err := (Setup{
		Name:       " campone ",
		ControlURL: " https://hs.campone.cc ",
		AuthMode:   AuthModeApproval,
		AccessMode: AccessModeBoth,
		Target:     "100.64.0.1",
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if approval.Name != "campone" || approval.Hostname != "campone" || approval.GroupName != "campone-access" {
		t.Fatalf("默认名称未正确补齐: %+v", approval)
	}
	if approval.Target != "100.64.0.1/32" || approval.RouteRule() != "IP-CIDR,100.64.0.1/32,campone-access,no-resolve" {
		t.Fatalf("IPv4 目标或规则异常: target=%q rule=%q", approval.Target, approval.RouteRule())
	}

	ipv6, err := (Setup{Name: "v6", AuthMode: AuthModeKey, AuthKey: "secret", AccessMode: AccessModeTUN, Target: "fd7a:115c:a1e0::1"}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if ipv6.RouteRule() != "IP-CIDR6,fd7a:115c:a1e0::1/128,v6-access,no-resolve" {
		t.Fatalf("IPv6 规则异常: %q", ipv6.RouteRule())
	}

	if _, err := (Setup{Name: "bad", AuthMode: AuthModeApproval, AuthKey: "unexpected", AccessMode: AccessModeProxy}).Normalize(); err == nil {
		t.Fatal("管理员审批模式携带 auth-key 时应拒绝")
	}
	if _, err := (Setup{Name: "bad", AuthMode: AuthModeKey, AccessMode: AccessModeProxy}).Normalize(); err == nil {
		t.Fatal("Auth Key 模式缺少密钥时应拒绝")
	}
	if _, err := (Setup{Name: "bad", AuthMode: AuthModeApproval, AccessMode: AccessModeTUN}).Normalize(); err == nil {
		t.Fatal("TUN 模式缺少目标范围时应拒绝")
	}
}

// TestIsManagedRouteForGroup 验证终止接入只识别 Setup.RouteRule 生成的严格 IP 路由，
// 不会把同组的域名规则、地址族不匹配规则或其它组规则一并删除。
//
// 参数说明：t 是 Go 测试上下文。
//
// 返回值说明：无；通过正反例断言表达受管规则边界。
//
// 错误情况：合法受管路由未识别或用户自定义规则被误判时测试失败。
func TestIsManagedRouteForGroup(t *testing.T) {
	if !IsManagedRouteForGroup("IP-CIDR,100.64.0.1/32,campone-access,no-resolve", "campone-access") {
		t.Fatal("标准 Tailscale IPv4 路由应被识别")
	}
	if !IsManagedRouteForGroup("IP-CIDR6,fd7a:115c:a1e0::/64,campone-access,no-resolve", "campone-access") {
		t.Fatal("标准 Tailscale IPv6 路由应被识别")
	}
	for _, rule := range []string{
		"DOMAIN,example.com,campone-access",
		"IP-CIDR,100.64.0.1/32,other-access,no-resolve",
		"IP-CIDR6,100.64.0.1/32,campone-access,no-resolve",
		"IP-CIDR,not-a-prefix,campone-access,no-resolve",
	} {
		if IsManagedRouteForGroup(rule, "campone-access") {
			t.Fatalf("非受管规则不应被终止流程删除: %q", rule)
		}
	}
}
