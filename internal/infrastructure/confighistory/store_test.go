package confighistory

// 本文件验证加密、损坏拒绝、路径边界与保留数量，不依赖真实用户状态目录。
import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestEncryptedHistoryRoundTripAndTampering 检查明文隔离与认证加密；参数 t 为测试对象；无返回，泄露或篡改被接受时失败。
func TestEncryptedHistoryRoundTripAndTampering(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	raw := []byte("api-secret: unique-sensitive-secret\n")
	redacted := []byte("api-secret: '***'\n")
	version, err := store.Archive(raw, redacted, "设置变更前", []string{"api-secret"})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(dir, version.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(saved, raw) || bytes.Contains(saved, []byte("unique-sensitive-secret")) {
		t.Fatal("历史包含明文凭据")
	}
	restored, err := New(dir).Read(version.ID)
	if err != nil || !bytes.Equal(restored, raw) {
		t.Fatalf("重启后无法恢复: %v", err)
	}
	exported, err := store.Redacted(version.ID)
	if err != nil || !bytes.Equal(exported, redacted) {
		t.Fatal("脱敏导出错误")
	}
	info, err := os.Stat(filepath.Join(dir, "vault.key"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("密钥权限错误")
	}
	var record record
	if err = json.Unmarshal(saved, &record); err != nil {
		t.Fatal(err)
	}
	record.Sealed[0] ^= 1
	tampered, _ := json.Marshal(record)
	if err = os.WriteFile(filepath.Join(dir, version.ID+".json"), tampered, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Read(version.ID); err == nil {
		t.Fatal("接受了损坏密文")
	}
	if _, err = store.Read("../vault.key"); err == nil {
		t.Fatal("接受了路径穿越")
	}
}

// TestHistoryRetentionAndMissingKey 检查上限和密钥遗失保护；参数 t 为测试对象；无返回，旧密钥被替换时失败。
func TestHistoryRetentionAndMissingKey(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	for range 32 {
		if _, err := store.Archive([]byte("secret"), []byte("***"), "修改", nil); err != nil {
			t.Fatal(err)
		}
	}
	versions, err := store.List()
	if err != nil || len(versions) != 30 {
		t.Fatalf("保留数量不正确: %d %v", len(versions), err)
	}
	if err = os.Remove(filepath.Join(dir, "vault.key")); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Archive([]byte("new"), nil, "修改", nil); err == nil {
		t.Fatal("密钥遗失后仍创建了历史")
	}
	if _, err = store.Read(versions[0].ID); err == nil {
		t.Fatal("密钥遗失后仍能解密")
	}
}
