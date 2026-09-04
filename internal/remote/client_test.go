package remote

// 本文件验证一次性远端管道在对端断开和 context 取消时的收尾语义。

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// TestPipeConnectionUnblocksStdinWhenRemoteCloses 验证远端先断开时，
// Pipe 会关闭阻塞的 stdin 并等待输入拷贝协程退出。
//
// 参数说明：
//   - t: *testing.T，提供测试上下文、超时与断言报告。
//
// 返回值说明：无。
//
// 错误情况：如果连接关闭后函数仍卡在 stdin.Read，测试会在一秒后失败。
func TestPipeConnectionUnblocksStdinWhenRemoteCloses(t *testing.T) {
	client, server := net.Pipe()
	inputReader, inputWriter := io.Pipe()
	result := make(chan error, 1)
	go func() {
		result <- pipeConnection(t.Context(), client, inputReader, io.Discard)
	}()

	if err := server.Close(); err != nil {
		t.Fatalf("关闭模拟远端失败: %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("远端正常关闭应收尾成功，got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("远端关闭后 Pipe 仍阻塞在 stdin")
	}
	_ = inputWriter.Close()
}

// TestPipeConnectionStopsWhenContextCancelled 验证双向都没有流量时，
// context 取消仍能同时解开网络读与 stdin 读。
//
// 参数说明：
//   - t: *testing.T，提供超时与断言报告。
//
// 返回值说明：无。
//
// 错误情况：取消不能在一秒内使管道返回时测试失败。
func TestPipeConnectionStopsWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	client, server := net.Pipe()
	defer server.Close()
	inputReader, inputWriter := io.Pipe()
	defer inputWriter.Close()
	result := make(chan error, 1)
	go func() {
		result <- pipeConnection(ctx, client, inputReader, io.Discard)
	}()

	cancel()
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("context 取消后 Pipe 未退出")
	}
}
