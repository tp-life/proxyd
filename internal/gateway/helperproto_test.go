package gateway

// helper 协议与看门狗的跨平台单测（net.Pipe + fake 执行能力）。

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHelperOps 记录白名单指令调用，并可注入失败。
type fakeHelperOps struct {
	applied    []string
	cleared    int
	forward    bool
	getForward bool
	failOn     string // op 名前缀匹配时返回错误
}

func (f *fakeHelperOps) ApplyPF(anchor string) error {
	if f.failOn == helperOpPFApply {
		return errors.New("pfctl boom")
	}
	f.applied = append(f.applied, anchor)
	return nil
}

func (f *fakeHelperOps) ClearPF() error {
	f.cleared++
	return nil
}

func (f *fakeHelperOps) GetForward() (bool, error) { return f.getForward, nil }

func (f *fakeHelperOps) SetForward(enable bool) error {
	f.forward = enable
	return nil
}

// helperRoundTrip 在 net.Pipe 上跑一次「握手 + 单指令」并返回应答。
func helperRoundTrip(t *testing.T, ops helperOps, clientVersion int, req helperRequest) (helperResponse, helperResponse) {
	t.Helper()
	client, server := net.Pipe()
	var beats atomic.Int32
	go serveHelperConn(server, ops, func() { beats.Add(1) })

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
	ops := &fakeHelperOps{}
	ack, resp := helperRoundTrip(t, ops, helperProtocolVersion, helperRequest{
		Version: helperProtocolVersion,
		Op:      helperOpPFApply,
		Params:  json.RawMessage(`{"anchor":"rdr pass inet proto tcp from any to any -> 127.0.0.1 port 17892"}`),
	})
	if !ack.OK || ack.Version != helperProtocolVersion {
		t.Fatalf("握手应答 = %+v", ack)
	}
	if !resp.OK {
		t.Fatalf("pf.apply 应答 = %+v", resp)
	}
	if len(ops.applied) != 1 || !strings.Contains(ops.applied[0], "rdr pass") {
		t.Fatalf("ops.applied = %v", ops.applied)
	}
}

func TestHelperProtocolRejectsVersionMismatch(t *testing.T) {
	ops := &fakeHelperOps{}
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
	ops := &fakeHelperOps{}
	cases := []struct {
		name string
		req  helperRequest
		want string
	}{
		{"未知指令", helperRequest{Version: helperProtocolVersion, Op: "shell.exec"}, "白名单"},
		{"pf.apply 未知字段", helperRequest{Version: helperProtocolVersion, Op: helperOpPFApply, Params: json.RawMessage(`{"anchor":"x","cmd":"rm -rf /"}`)}, "params 非法"},
		{"pf.apply 空 anchor", helperRequest{Version: helperProtocolVersion, Op: helperOpPFApply, Params: json.RawMessage(`{"anchor":"  "}`)}, "不能为空"},
		{"pf.apply 缺 params", helperRequest{Version: helperProtocolVersion, Op: helperOpPFApply}, "缺少 params"},
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
	if len(ops.applied) != 0 {
		t.Errorf("被拒绝的请求不应触达执行层: %v", ops.applied)
	}
}

func TestHelperProtocolForwardOps(t *testing.T) {
	ops := &fakeHelperOps{getForward: true}
	_, resp := helperRoundTrip(t, ops, helperProtocolVersion, helperRequest{Version: helperProtocolVersion, Op: helperOpForwardGet})
	if !resp.OK || resp.Result != "on" {
		t.Fatalf("forward.get 应答 = %+v", resp)
	}
	_, resp = helperRoundTrip(t, ops, helperProtocolVersion, helperRequest{
		Version: helperProtocolVersion, Op: helperOpForwardSet, Params: json.RawMessage(`{"enable":false}`),
	})
	if !resp.OK || ops.forward {
		t.Fatalf("forward.set 应答 = %+v, forward=%v", resp, ops.forward)
	}
}

func TestHelperProtocolActivityBeats(t *testing.T) {
	ops := &fakeHelperOps{}
	client, server := net.Pipe()
	var beats atomic.Int32
	done := make(chan struct{})
	go func() {
		serveHelperConn(server, ops, func() { beats.Add(1) })
		close(done)
	}()
	encoder := json.NewEncoder(client)
	decoder := json.NewDecoder(client)
	for i := 0; i < 3; i++ {
		if err := encoder.Encode(helperRequest{Version: helperProtocolVersion, Op: helperOpPing}); err != nil {
			t.Fatal(err)
		}
		var resp helperResponse
		if err := decoder.Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if !resp.OK {
			t.Fatalf("ping 应答 = %+v", resp)
		}
	}
	_ = client.Close()
	<-done
	// 每次成功指令记一次心跳（看门狗存活信号）。
	if got := beats.Load(); got != 3 {
		t.Errorf("心跳次数 = %d, want 3", got)
	}
}

// fakeClock 是可在测试协程间安全推进的假时钟。
type fakeClock struct{ nanos atomic.Int64 }

func (c *fakeClock) now() time.Time { return time.Unix(0, c.nanos.Load()) }

func (c *fakeClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

func TestWatchdogExpiry(t *testing.T) {
	clock := &fakeClock{}
	clock.advance(time.Hour) // 离开零值时刻
	w := newHelperWatchdog(clock.now, 90*time.Second)
	if w.expired() {
		t.Fatal("刚创建不应超时")
	}
	clock.advance(91 * time.Second)
	if !w.expired() {
		t.Fatal("超过阈值应超时")
	}
	w.beat()
	if w.expired() {
		t.Fatal("心跳后不应超时")
	}
}

func TestWatchdogRunExpiresOnce(t *testing.T) {
	clock := &fakeClock{}
	clock.advance(time.Hour)
	w := newHelperWatchdog(clock.now, 90*time.Second)
	var appliedFlag atomic.Bool
	appliedFlag.Store(true)
	var clears atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runHelperWatchdog(ctx, w, 5*time.Millisecond, appliedFlag.Load, func() {
			clears.Add(1)
			appliedFlag.Store(false) // 清理后规则不再应用
		})
	}()
	clock.advance(91 * time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for clears.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if clears.Load() != 1 {
		t.Fatalf("超时清理次数 = %d, want 1", clears.Load())
	}
	// 清理后不再重复触发。
	time.Sleep(30 * time.Millisecond)
	if clears.Load() != 1 {
		t.Fatalf("清理后不应重复触发, clears=%d", clears.Load())
	}
	cancel()
	<-done
}
