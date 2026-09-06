package main

// 本文件验证 CLI 公钥文件导入与私钥预检，确保文件内容和管理名称正确发送到 API。

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestRemoteSSHKeysCLIImport 验证文件导入、认证开关和私钥本地拒绝。
// 参数说明：t 为 *testing.T，提供临时公钥文件和本地 HTTP 服务。
// 返回值说明：无。
// 错误情况：错误 API 路由/字段、私钥被上传、无效参数未拒绝时测试失败。
func TestRemoteSSHKeysCLIImport(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	text := string(ssh.MarshalAuthorizedKey(key))
	requests := make(chan map[string]any, 4)
	// 此测试服务只记录公钥管理请求，不模拟 SSH 认证；真实握手由 remote 层测试覆盖。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		body["path"] = r.URL.Path
		requests <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client := &apiClient{base: server.URL}
	path := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdRemoteSSHKeys(client, []string{"import", path, "laptop"}); err != nil {
		t.Fatal(err)
	}
	body := <-requests
	if body["path"] != "/api/remote/ssh-keys" || body["public_key"] != text || body["name"] != "laptop" {
		t.Fatalf("导入请求异常：%+v", body)
	}
	if err := cmdRemoteSSHKeys(client, []string{"on"}); err != nil {
		t.Fatal(err)
	}
	body = <-requests
	if body["path"] != "/api/remote/ssh-auth" || body["required"] != true {
		t.Fatal("认证请求异常")
	}
	if err := os.WriteFile(path, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nprivate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdRemoteSSHKeys(client, []string{"import", path}); err == nil {
		t.Fatal("误上传私钥应在本地拒绝")
	}
	if len(requests) != 0 {
		t.Fatal("私钥不应发送到管理 API")
	}
	if err := cmdRemoteSSHKeys(client, []string{"del"}); err == nil {
		t.Fatal("缺少删除标识应报错")
	}
}
