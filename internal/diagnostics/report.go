// Package diagnostics 定义不含凭据的诊断报告，网络与 SSH 实现由适配器负责。
package diagnostics

import "time"

// Step 是单一检查结果；Detail 只能使用固定提示，不能存放外部原始错误或环境输出。
type Step struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Detail     string `json:"detail"`
	DurationMS int64  `json:"duration_ms"`
}

// Report 是一次诊断的有界结果，不保存设备 token、主机环境或认证材料。
type Report struct {
	CreatedAt time.Time `json:"created_at"`
	Scope     string    `json:"scope"`
	Steps     []Step    `json:"steps"`
}
