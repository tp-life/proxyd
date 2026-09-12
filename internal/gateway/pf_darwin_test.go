//go:build darwin

package gateway

import (
	"errors"
	"net"
	"strings"
	"testing"
)

func TestHelperCallDialFailure(t *testing.T) {
	orig := helperDial
	helperDial = func() (net.Conn, error) {
		return nil, errors.New("dial unix: no such file or directory")
	}
	defer func() { helperDial = orig }()

	_, err := helperCall(helperOpPing, nil)
	if err == nil {
		t.Fatal("拨号失败应返回错误")
	}
	if !strings.Contains(err.Error(), "helper 未安装") || !strings.Contains(err.Error(), helperSocketPath) {
		t.Errorf("错误应含安装指引与 socket 路径: %v", err)
	}
	// Runner 各操作同样以指引错误失败。
	if err := (helperRunner{}).Apply("anchor text"); err == nil || !strings.Contains(err.Error(), "helper 未安装") {
		t.Errorf("Apply: %v", err)
	}
	if err := (helperRunner{}).Clear(); err == nil || !strings.Contains(err.Error(), "helper 未安装") {
		t.Errorf("Clear: %v", err)
	}
	if err := (helperRunner{}).Forwarding(true); err == nil || !strings.Contains(err.Error(), "helper 未安装") {
		t.Errorf("Forwarding: %v", err)
	}
	if s := (helperRunner{}).Status(); !strings.Contains(s, "helper 未安装") {
		t.Errorf("Status: %q", s)
	}
}
