//go:build linux || darwin || windows

package remote

// 本文件验证网页文件链路所依赖的 SSH/SFTP 协议边界。测试使用 net.Pipe，
// 不访问公网 DERP；tailcat 拨号已由现有 client 测试覆盖。

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"
)

// TestFileClientRoundTrip 验证内嵌 SSH 的标准 SFTP 子系统可以列目录、原子上传和下载。
//
// 参数说明：t 为 *testing.T，提供隔离目录、失败报告和清理。
// 返回值说明：无；上传内容、列表元数据与下载内容全部一致时测试通过。
// 错误情况：SSH/SFTP 握手、文件权限、临时文件清理或流内容任一步异常都会立即失败。
func TestFileClientRoundTrip(t *testing.T) {
	stateDir := t.TempDir()
	filesDir := t.TempDir()
	handler, err := configuredShellSSHHandler(stateDir, false, nil, "")
	if err != nil {
		t.Fatalf("configuredShellSSHHandler: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clientConn, err := openAuthenticatedLoopback(ctx, handler)
	if err != nil {
		t.Fatalf("openAuthenticatedLoopback: %v", err)
	}
	client, err := openFileClientOnConn(ctx, clientConn, FileCredentials{Username: "proxyd-test"})
	if err != nil {
		t.Fatalf("openFileClientOnConn: %v", err)
	}
	target := filepath.Join(filesDir, "hello.txt")
	written, err := client.Upload(target, bytes.NewBufferString("hello over sftp"))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if written != int64(len("hello over sftp")) {
		t.Fatalf("written=%d", written)
	}

	listing, err := client.List(ctx, filesDir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listing.Entries) != 1 || listing.Entries[0].Name != "hello.txt" || listing.Entries[0].IsDir {
		t.Fatalf("entries=%+v", listing.Entries)
	}

	download, err := client.OpenDownload(target)
	if err != nil {
		t.Fatalf("OpenDownload: %v", err)
	}
	data, err := io.ReadAll(download.Reader)
	_ = download.Reader.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "hello over sftp" {
		t.Fatalf("download=%q", data)
	}
	_ = client.Close()
}
