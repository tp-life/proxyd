package api

// 本文件从 HTTP 管理入口验证 SSH 公钥导入、模式切换、撤销与输入保护。

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestRemoteSSHKeyAPI 验证 Web/CLI 共用的公钥管理 API 全流程。
// 参数说明：t 为 *testing.T，提供隔离 API 服务与测试公钥。
// 返回值说明：无。
// 错误情况：状态码、模式、公钥列表或持久化结果不符合要求时失败；不启动远端隧道。
func TestRemoteSSHKeyAPI(t *testing.T) {
	_, addr := newRemoteTestServer(t)
	base := "http://" + addr + "/api/remote"
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	text := string(ssh.MarshalAuthorizedKey(key))
	code, state := remoteAPIReq(t, http.MethodGet, base+"/ssh-keys", nil)
	if code != 200 || state["required"] != false || len(state["keys"].([]any)) != 0 {
		t.Fatalf("默认模式异常：%d %+v", code, state)
	}
	code, state = remoteAPIReq(t, http.MethodPost, base+"/ssh-keys", map[string]string{"name": "laptop", "public_key": text})
	if code != 200 || len(state["ssh_keys"].([]any)) != 1 {
		t.Fatalf("添加失败：%d %+v", code, state)
	}
	code, _ = remoteAPIReq(t, http.MethodPost, base+"/ssh-keys", map[string]string{"public_key": text})
	if code != 400 {
		t.Fatal("重复公钥未拒绝")
	}
	code, _ = remoteAPIReq(t, http.MethodPost, base+"/ssh-keys", map[string]string{"public_key": "-----BEGIN OPENSSH PRIVATE KEY-----"})
	if code != 400 {
		t.Fatal("私钥输入未拒绝")
	}
	code, state = remoteAPIReq(t, http.MethodPost, base+"/ssh-auth", map[string]bool{"required": true})
	if code != 200 || state["ssh_auth_required"] != true {
		t.Fatal("开启认证失败")
	}
	// 单项 PATCH 必须保留其他策略字段；过期时间允许写入过去并立刻在状态中显示失效。
	code, state = remoteAPIReq(t, http.MethodPatch, base+"/ssh-keys", map[string]any{"identifier": "laptop", "disabled": true, "expires_at": "2020-01-01T00:00:00Z"})
	if code != 200 {
		t.Fatalf("生命周期更新失败: %d %+v", code, state)
	}
	entry := state["ssh_keys"].([]any)[0].(map[string]any)
	if entry["disabled"] != true || entry["expired"] != true {
		t.Fatal("禁用或到期状态丢失")
	}
	code, state = remoteAPIReq(t, http.MethodPatch, base+"/ssh-keys", map[string]any{"identifier": "laptop", "disabled": false})
	entry = state["ssh_keys"].([]any)[0].(map[string]any)
	if code != 200 || entry["disabled"] != false || entry["expired"] != true {
		t.Fatal("更新禁用状态覆盖了有效期")
	}
	code, state = remoteAPIReq(t, http.MethodPatch, base+"/ssh-keys", map[string]any{"identifier": "laptop", "expires_at": ""})
	if code != 200 || state["ssh_keys"].([]any)[0].(map[string]any)["expired"] != false {
		t.Fatal("恢复永久授权失败")
	}
	code, _ = remoteAPIReq(t, http.MethodPatch, base+"/ssh-keys", map[string]any{"identifier": "laptop", "expires_at": "bad"})
	if code != 400 {
		t.Fatal("无效时间未拒绝")
	}
	code, _ = remoteAPIReq(t, http.MethodPost, base+"/ssh-keys/disconnect", map[string]string{"fingerprint": "laptop"})
	if code != 400 {
		t.Fatal("断开操作应要求完整指纹")
	}
	code, state = remoteAPIReq(t, http.MethodPost, base+"/ssh-keys/disconnect", map[string]string{"fingerprint": ssh.FingerprintSHA256(key)})
	if code != 200 || state["disconnected"] != float64(0) {
		t.Fatal("无活动连接时不应返回错误")
	}
	code, _ = remoteAPIReq(t, http.MethodPost, base+"/ssh-auth", map[string]any{})
	if code != 400 {
		t.Fatal("缺少 required 不应关闭认证")
	}
	code, state = remoteAPIReq(t, http.MethodDelete, base+"/ssh-keys", map[string]string{"identifier": ssh.FingerprintSHA256(key)})
	if code != 200 || state["ssh_auth_required"] != true || len(state["ssh_keys"].([]any)) != 0 {
		t.Fatal("撤销最后一把公钥不应关闭认证")
	}
	code, state = remoteAPIReq(t, http.MethodPost, base+"/ssh-auth", map[string]bool{"required": false})
	if code != 200 || state["ssh_auth_required"] != false {
		t.Fatal("无法显式恢复旧免密模式")
	}
}
