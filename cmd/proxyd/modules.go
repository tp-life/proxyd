package main

// 模块命令通过守护进程 API 执行，保持与 Web 相同的事务与错误语义。
import (
	"fmt"
	"net/http"
	"proxyd/internal/app"
)

// cmdModules 列出模块，或启用/禁用单个模块。
// 参数：args 为 []string，支持 -c、list、proxy|remote|gateway on|off|retry；返回 error，参数或 API 错误原样上报。
func cmdModules(args []string) error {
	path, rest, err := parseCFlag("modules", args)
	if err != nil {
		return err
	}
	c, err := newAPIClient(path)
	if err != nil {
		return err
	}
	var states []app.ModuleState
	if len(rest) == 0 || len(rest) == 1 && rest[0] == "list" {
		err = c.do(http.MethodGet, "/api/modules", nil, &states)
	} else {
		if len(rest) != 2 || (rest[0] != "proxy" && rest[0] != "remote" && rest[0] != "gateway") || (rest[1] != "on" && rest[1] != "off" && rest[1] != "retry") {
			return fmt.Errorf("用法: proxyd modules [-c 配置] list | proxy|remote|gateway on|off|retry")
		}
		if rest[1] == "retry" {
			err = c.do(http.MethodPost, "/api/modules/"+rest[0]+"/retry", nil, &states)
		} else {
			err = c.do(http.MethodPost, "/api/modules/"+rest[0], map[string]bool{"enabled": rest[1] == "on"}, &states)
		}
	}
	if err != nil {
		return err
	}
	for _, state := range states {
		fmt.Printf("%s\t%s\t%s\t%s\t%s\n", state.ID, state.Name, onOffText(state.Enabled), state.Phase, state.Error)
		if state.NextRetryAt != nil {
			fmt.Printf("  下次重试: %s\n", state.NextRetryAt.Local().Format("15:04:05"))
		}
	}
	return nil
}
