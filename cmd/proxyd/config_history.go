package main

// 配置历史 CLI 复用 Web 的预检与摘要校验，不直接解密服务器历史文件。
import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"proxyd/internal/app"
	"proxyd/internal/configversion"
	"time"
)

// cmdConfigHistory 列表、脱敏导出、预检或恢复历史；参数 cfgFile/args 为配置路径和子参数。
// 返回 error；restore 只有显式 --yes 才写入，未确认时仅输出影响，网络与冲突错误向上传递。
func cmdConfigHistory(cfgFile string, args []string) error {
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	if len(args) == 0 || len(args) == 1 && args[0] == "list" {
		var result struct {
			Versions []configversion.Version `json:"versions"`
			Pending  bool                    `json:"pending_restart"`
		}
		if err = c.do(http.MethodGet, "/api/config/history", nil, &result); err != nil {
			return err
		}
		for _, version := range result.Versions {
			fmt.Printf("%s\t%s\t%s\t%v\n", version.ID, version.CreatedAt.Local().Format(time.RFC3339), version.Reason, version.Sections)
		}
		if result.Pending {
			fmt.Println("配置等待重启生效")
		}
		return nil
	}
	if len(args) < 2 || len(args) > 3 {
		return fmt.Errorf("用法: proxyd config history list|export <ID>|preview <ID>|restore <ID> [--yes]")
	}
	endpoint := "/api/config/history/" + url.PathEscape(args[1])
	if args[0] == "export" && len(args) == 2 {
		data, err := c.raw(http.MethodGet, endpoint+"/export", nil, nil, 30*time.Second)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(data)
		return err
	}
	if args[0] != "preview" && args[0] != "restore" || len(args) == 3 && (args[0] != "restore" || args[2] != "--yes") {
		return fmt.Errorf("无效的历史操作或参数")
	}
	var preview app.HistoryPreview
	if err = c.do(http.MethodPost, endpoint+"/preview", nil, &preview); err != nil {
		return err
	}
	if args[0] == "preview" || len(args) != 3 {
		data, _ := json.MarshalIndent(preview, "", "  ")
		fmt.Println(string(data))
		if args[0] == "restore" {
			fmt.Println("仅预检；确认影响后加 --yes 恢复，重启后生效")
		}
		return nil
	}
	if err = c.do(http.MethodPost, endpoint+"/restore", map[string]string{"digest": preview.Digest, "base_digest": preview.BaseDigest}, nil); err != nil {
		return err
	}
	fmt.Println("配置已恢复，请重启守护进程后再修改设置")
	return nil
}
