package tunhelper

// helper 协议的跨平台单测（net.Pipe + fake 执行能力）。

import (
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
)

// fakeHelperOps 记录白名单指令调用，并可注入失败。
type fakeHelperOps struct {
	created    []CreateTUNParams
	fd         int    // CreateTUN 返回的 fd（-1 表示不回传）
	ifName     string // CreateTUN 返回的接口名
	failPing   error
	failCreate error
}

func newFakeHelperOps() *fakeHelperOps {
	return &fakeHelperOps{fd: -1, ifName: "utun9"}
}

func (f *fakeHelperOps) Ping() error { return f.failPing }

func (f *fakeHelperOps) CreateTUN(params CreateTUNParams) (int, string, error) {
	if f.failCreate != nil {
		return -1, "", f.failCreate
	}
	f.created = append(f.created, params)
	return f.fd, f.ifName, nil
}

// helperRoundTrip 在 net.Pipe 上跑一次「握手 + 单指令」并返回应答。
func helperRoundTrip(t *testing.T, ops helperOps, clientVersion int, req helperRequest) (helperResponse, helperResponse) {
	t.Helper()
	client, server := net.Pipe()
	go serveHelperConn(server, ops)

	encoder := json.NewEncoder(client)
	decoder := json.NewDecoder(client)
	handshake := helperRequest{Version: clientVersion, Op: helperOpPing}
	if err := encoder.Encode(handshake); err != nil {
		t.Fatal(err)
	}
	var ack helperResponse
	if err := decoder.Decode(&ack); err != nil {
		t.Fatal(err)
	}
	if req.Op != "" {
		if err := encoder.Encode(req); err != nil {
			t.Fatal(err)
		}
		var resp helperResponse
		if err := decoder.Decode(&resp); err != nil {
			t.Fatal(err)
		}
		_ = client.Close()
		return ack, resp
	}
	_ = client.Close()
	return ack, helperResponse{}
}

func TestHelperProtocolHandshakeAndDispatch(t *testing.T) {
	ops := newFakeHelperOps()
	ack, resp := helperRoundTrip(t, ops, helperProtocolVersion, helperRequest{
		Version: helperProtocolVersion,
		Op:      helperOpTunCreate,
		Params:  json.RawMessage(`{"mtu":1500,"inet4_address":"198.18.0.1/30"}`),
	})
	if !ack.OK || ack.Result != "pong" || ack.Version != helperProtocolVersion {
		t.Fatalf("握手应答 = %+v", ack)
	}
	if !resp.OK || resp.Result != "utun9" {
		t.Fatalf("tun.create 应答 = %+v", resp)
	}
	if len(ops.created) != 1 || ops.created[0].MTU != 1500 {
		t.Fatalf("ops.created = %v", ops.created)
	}
}

func TestHelperProtocolRejectsVersionMismatch(t *testing.T) {
	ops := newFakeHelperOps()
	ack, _ := helperRoundTrip(t, ops, helperProtocolVersion+1, helperRequest{})
	if ack.OK {
		t.Fatal("版本失配应拒绝")
	}
	if !strings.Contains(ack.Error, "版本") {
		t.Errorf("错误应说明版本不兼容: %q", ack.Error)
	}
	if ack.Version != helperProtocolVersion {
		t.Errorf("应答应携带 helper 主版本: %+v", ack)
	}
}

func TestHelperProtocolRejectsUnknownOpAndFields(t *testing.T) {
	ops := newFakeHelperOps()
	cases := []struct {
		name string
		req  helperRequest
		want string
	}{
		{"未知指令", helperRequest{Version: helperProtocolVersion, Op: "tun.destroy"}, "白名单"},
		{"tun.create 未知字段", helperRequest{Version: helperProtocolVersion, Op: helperOpTunCreate, Params: json.RawMessage(`{"mtu":1500,"inet4_address":"198.18.0.1/30","cmd":"rm -rf /"}`)}, "params 非法"},
		{"tun.create 缺 params", helperRequest{Version: helperProtocolVersion, Op: helperOpTunCreate}, "缺少 params"},
		{"tun.create 参数类型错", helperRequest{Version: helperProtocolVersion, Op: helperOpTunCreate, Params: json.RawMessage(`{"mtu":"1500"}`)}, "params 非法"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, resp := helperRoundTrip(t, ops, helperProtocolVersion, tc.req)
			if resp.OK {
				t.Fatalf("应拒绝: %+v", resp)
			}
			if !strings.Contains(resp.Error, tc.want) {
				t.Errorf("错误 %q 不含 %q", resp.Error, tc.want)
			}
		})
	}
	if len(ops.created) != 0 {
		t.Errorf("被拒绝的请求不应触达执行层: %v", ops.created)
	}
}

func TestHelperProtocolRejectsInvalidParams(t *testing.T) {
	ops := newFakeHelperOps()
	cases := []struct {
		name   string
		params string
		want   string
	}{
		{"mtu 为零", `{"mtu":0,"inet4_address":"198.18.0.1/30"}`, "mtu"},
		{"mtu 超上限", `{"mtu":70000,"inet4_address":"198.18.0.1/30"}`, "mtu"},
		{"inet4 非 CIDR", `{"mtu":1500,"inet4_address":"198.18.0.1"}`, "inet4_address"},
		{"inet4 用 v6", `{"mtu":1500,"inet4_address":"fd00::1/64"}`, "inet4_address"},
		{"inet6 用 v4", `{"mtu":1500,"inet4_address":"198.18.0.1/30","inet6_address":"198.18.0.1/30"}`, "inet6_address"},
		{"auto_route 空段", `{"mtu":1500,"inet4_address":"198.18.0.1/30","auto_route":true}`, "auto_route_ranges"},
		{"路由段非 v4", `{"mtu":1500,"inet4_address":"198.18.0.1/30","auto_route":true,"auto_route_ranges":["fd00::/8"]}`, "auto_route_ranges"},
		{"排除段非法", `{"mtu":1500,"inet4_address":"198.18.0.1/30","exclude_ranges":["abc"]}`, "exclude_ranges"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, resp := helperRoundTrip(t, ops, helperProtocolVersion, helperRequest{
				Version: helperProtocolVersion, Op: helperOpTunCreate, Params: json.RawMessage(tc.params),
			})
			if resp.OK {
				t.Fatalf("应拒绝: %+v", resp)
			}
			if !strings.Contains(resp.Error, tc.want) {
				t.Errorf("错误 %q 不含 %q", resp.Error, tc.want)
			}
		})
	}
	if len(ops.created) != 0 {
		t.Errorf("非法参数不应触达执行层: %v", ops.created)
	}
}

func TestHelperProtocolCreateFailurePropagates(t *testing.T) {
	ops := newFakeHelperOps()
	ops.failCreate = errors.New("内核拒绝")
	_, resp := helperRoundTrip(t, ops, helperProtocolVersion, helperRequest{
		Version: helperProtocolVersion,
		Op:      helperOpTunCreate,
		Params:  json.RawMessage(`{"mtu":1500,"inet4_address":"198.18.0.1/30"}`),
	})
	if resp.OK || !strings.Contains(resp.Error, "内核拒绝") {
		t.Fatalf("执行失败应收敛为 OK=false: %+v", resp)
	}
}

func TestCreateTUNParamsParseExcludeFilter(t *testing.T) {
	cfg, err := CreateTUNParams{
		MTU:             1500,
		Inet4Address:    "198.18.0.1/30",
		Inet6Address:    "fd00::1/64",
		AutoRoute:       true,
		AutoRouteRanges: []string{"0.0.0.0/1", "128.0.0.0/1"},
		ExcludeRanges:   []string{"128.0.0.0/1"},
	}.parse()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cfg.inet6.IsValid() || cfg.inet6.String() != "fd00::1/64" {
		t.Errorf("inet6 = %v", cfg.inet6)
	}
	if len(cfg.routes) != 1 || cfg.routes[0].String() != "0.0.0.0/1" {
		t.Errorf("剔除后路由 = %v", cfg.routes)
	}

	// 全覆盖排除：0.0.0.0/0 吞掉全部子段。
	cfg, err = CreateTUNParams{
		MTU:             1500,
		Inet4Address:    "198.18.0.1/30",
		AutoRoute:       true,
		AutoRouteRanges: []string{"1.0.0.0/8", "2.0.0.0/8"},
		ExcludeRanges:   []string{"0.0.0.0/0"},
	}.parse()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.routes) != 0 {
		t.Errorf("全覆盖剔除后应为空: %v", cfg.routes)
	}

	// 不覆盖的排除段不影响路由段。
	cfg, err = CreateTUNParams{
		MTU:             1500,
		Inet4Address:    "198.18.0.1/30",
		AutoRoute:       true,
		AutoRouteRanges: []string{"1.0.0.0/8"},
		ExcludeRanges:   []string{"10.0.0.0/8"},
	}.parse()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.routes) != 1 {
		t.Errorf("不覆盖的排除段不应剔除: %v", cfg.routes)
	}

	// auto_route 关闭时路由段不参与解析。
	if _, err := (CreateTUNParams{MTU: 1500, Inet4Address: "198.18.0.1/30"}).parse(); err != nil {
		t.Errorf("最小参数集应通过: %v", err)
	}
}
