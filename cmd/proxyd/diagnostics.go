package main

// 诊断命令使用守护进程用例，JSON 报告不包含连接凭据和环境原文。
import (
	"encoding/json"
	"fmt"
	"net/http"
	"proxyd/internal/diagnostics"
)

// cmdDiagnose 检查本机或保存的设备；参数 args 支持设备名称及 --json；返回参数、API 或检查失败错误。
// 检查失败仍先输出完整报告，再返回非零状态，便于脚本保留排障证据。
func cmdDiagnose(args []string) error {
	path, peer, asJSON, err := parseDiagnosticArgs(args)
	if err != nil {
		return err
	}
	c, err := newAPIClient(path)
	if err != nil {
		return err
	}
	var report diagnostics.Report
	if err = c.do(http.MethodPost, "/api/diagnostics", map[string]string{"peer": peer}, &report); err != nil {
		return err
	}
	failed := false
	for _, step := range report.Steps {
		failed = failed || step.Status == "failed"
	}
	if asJSON {
		data, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(data))
	} else {
		for _, step := range report.Steps {
			fmt.Printf("%s\t%s\t%dms\t%s\n", step.ID, step.Status, step.DurationMS, step.Detail)
		}
	}
	if failed {
		return fmt.Errorf("诊断发现失败项，请按报告提示处理")
	}
	return nil
}

// parseDiagnosticArgs 先提取输出选项再解析共享配置路径，支持无设备参数的 --json。
// 参数 args 为命令行切片；返回配置路径、设备名、JSON 开关和 error，多余位置参数或未知选项会拒绝。
func parseDiagnosticArgs(args []string) (path, peer string, asJSON bool, err error) {
	filtered := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--json" {
			asJSON = true
		} else {
			filtered = append(filtered, arg)
		}
	}
	path, rest, err := parseCFlag("diagnose", filtered)
	if err != nil {
		return path, "", asJSON, err
	}
	if len(rest) > 1 {
		return path, "", asJSON, fmt.Errorf("用法: proxyd diagnose [-c 配置] [设备名称] [--json]")
	}
	if len(rest) == 1 {
		peer = rest[0]
	}
	return path, peer, asJSON, nil
}
