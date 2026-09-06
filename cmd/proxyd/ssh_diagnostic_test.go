package main

// 本文件验证诊断进度只能由完整协议标记推进，启动脚本输出不会伪装成成功。

import (
	"bytes"
	"strings"
	"testing"
)

// TestDiagnosticOutputMarkers 验证分片、PTY 换行、脚本回显以及白名单输出。
// 参数说明：t 为 *testing.T；返回值说明：无。
// 错误情况：回显触发成功、未选择的环境字段泄露或分片丢失时失败。
func TestDiagnosticOutputMarkers(t *testing.T) {
	var out bytes.Buffer
	d := &diagnosticOutput{out: &out}
	d.Write([]byte("printf 'PROXYD_DIAG_READY'; printf 'PROXYD_DIAG_DONE'\r\n"))
	if d.ready || d.done {
		t.Fatal("脚本回显被识别为执行完成")
	}
	d.Write([]byte("PROXYD_DIAG_AUTHENT"))
	d.Write([]byte("ICATED\r\nPROXYD_DIAG_READY\r\nUSER=test\r\nSECRET=hidden\r\nPATH=/bin\r\nPROXYD_DIAG_DONE\r\n"))
	if !d.authenticated || !d.ready || !d.done || !strings.Contains(out.String(), "USER=test") || strings.Contains(out.String(), "hidden") {
		t.Fatal("阶段标记或环境白名单错误")
	}
	d.Write([]byte(strings.Repeat("x", 70000)))
	if len(d.pending) > 65536 {
		t.Fatal("诊断缓存没有限制")
	}
}
