package main

// 本文件回归本机 JSON 诊断无设备参数时被共享 flag 解析器拒绝的问题。
import "testing"

// TestDiagnosticCLIArguments 覆盖本机和命名设备的 JSON 输出参数；参数 t 为测试对象；无返回，解析错误时失败。
func TestDiagnosticCLIArguments(t *testing.T) {
	for _, args := range [][]string{{"-c", "test.yaml", "--json"}, {"--json", "-c", "test.yaml"}, {"-c", "test.yaml", "home", "--json"}} {
		path, peer, jsonOutput, err := parseDiagnosticArgs(args)
		if err != nil || path != "test.yaml" || !jsonOutput {
			t.Fatalf("参数未接受: %v %v", args, err)
		}
		if len(args) == 4 && peer != "home" {
			t.Fatal("设备名丢失")
		}
	}
	if _, _, _, err := parseDiagnosticArgs([]string{"home", "extra"}); err == nil {
		t.Fatal("接受了多余参数")
	}
}
