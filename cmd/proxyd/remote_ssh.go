package main

// 本文件提供服务端 SSH 公钥授权的 CLI；读取客户端 .pub 文件并通过管理 API 导入。

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"proxyd/internal/remote"
)

// cmdRemoteSSHKeys 管理附加 SSH 公钥认证与授权集合。
// 参数说明：c 为 *apiClient，已配置认证的管理 API 客户端；args 为 []string，子命令参数。
// 返回值说明：error，操作成功时为 nil。
// 错误情况：参数非法、公钥文件读取失败、API 校验或事务失败时返回错误；
// import 只读取本机公钥文件，不上传私钥；多个公钥在服务端原子合并。
func cmdRemoteSSHKeys(c *apiClient, args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list", "export":
		if (sub == "list" && len(args) > 1) || len(args) > 2 {
			return fmt.Errorf("用法: proxyd remote ssh-keys list | export [文件]")
		}
		var result struct {
			Required bool                `json:"required"`
			Keys     []remote.SSHKeyInfo `json:"keys"`
		}
		if err := c.do(http.MethodGet, "/api/remote/ssh-keys", nil, &result); err != nil {
			return err
		}
		if sub == "export" {
			var content strings.Builder
			for _, entry := range result.Keys {
				content.WriteString(entry.PublicKey)
				if entry.Name != "" {
					content.WriteString(" " + entry.Name)
				}
				content.WriteByte('\n')
			}
			if len(args) == 2 {
				return os.WriteFile(args[1], []byte(content.String()), 0o600)
			}
			_, err := fmt.Fprint(os.Stdout, content.String())
			return err
		}
		if result.Required {
			fmt.Println("SSH 公钥认证：开启（隧道认证后仍需匹配 SSH 私钥）")
		} else {
			fmt.Println("SSH 公钥认证：关闭（保留隧道免密登录）")
		}
		if len(result.Keys) == 0 {
			fmt.Println("暂无 SSH 公钥；开启认证时将拒绝所有 SSH 登录")
			return nil
		}
		writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "NAME\tSTATE\tEXPIRES\tLAST LOGIN\tACTIVE\tFINGERPRINT")
		for _, entry := range result.Keys {
			state := "启用"
			if entry.Expired {
				state = "过期"
			}
			if entry.Disabled {
				state = "禁用"
			}
			expiry, last := "永久", "本次运行暂无"
			if entry.ExpiresAt != nil {
				expiry = entry.ExpiresAt.Format(time.RFC3339)
			}
			if entry.LastUsedAt != nil {
				last = entry.LastUsedAt.Format(time.RFC3339)
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%d\t%s\n", entry.Name, state, expiry, last, entry.Active, entry.Fingerprint)
		}
		return writer.Flush()
	case "on", "off":
		if len(args) != 1 {
			return fmt.Errorf("用法: proxyd remote ssh-keys on|off")
		}
		if err := c.do(http.MethodPost, "/api/remote/ssh-auth", map[string]bool{"required": sub == "on"}, nil); err != nil {
			return err
		}
		fmt.Printf("SSH 公钥认证已%s（已保存的公钥与隧道白名单保留）\n", map[string]string{"on": "开启", "off": "关闭"}[sub])
		return nil
	case "add", "import":
		if len(args) < 2 || len(args) > 3 {
			return fmt.Errorf("用法: proxyd remote ssh-keys add <公钥文本> [名称] | import <公钥文件> [名称]")
		}
		text := args[1]
		if sub == "import" {
			file, err := os.Open(args[1])
			if err != nil {
				return err
			}
			defer file.Close()
			// 多读取一个字节判断越界，避免把截断的 authorized_keys 当作完整文件导入。
			data, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
			if err != nil {
				return err
			}
			if len(data) > 64*1024 {
				return fmt.Errorf("SSH 公钥文件不得超过 64 KiB")
			}
			text = string(data)
		}
		name := ""
		if len(args) == 3 {
			name = args[2]
		}
		// 在网络请求之前复用服务端解析器，误选私钥文件时不会将它发送到服务端。
		if _, err := remote.ParseSSHAuthorizedKeys(text, name); err != nil {
			return err
		}
		if err := c.do(http.MethodPost, "/api/remote/ssh-keys", map[string]string{"public_key": text, "name": name}, nil); err != nil {
			return err
		}
		fmt.Println("SSH 公钥已添加；proxyd remote ssh-keys on 开启附加认证")
		return nil
	case "enable", "disable", "expire":
		if (sub == "expire" && len(args) != 3) || (sub != "expire" && len(args) != 2) {
			return fmt.Errorf("用法: proxyd remote ssh-keys enable|disable <指纹|名称> | expire <指纹|名称> <RFC3339|never>")
		}
		body := map[string]any{"identifier": args[1]}
		if sub == "expire" {
			value := args[2]
			if value == "never" {
				value = ""
			}
			body["expires_at"] = value
		} else {
			body["disabled"] = sub == "disable"
		}
		if err := c.do(http.MethodPatch, "/api/remote/ssh-keys", body, nil); err != nil {
			return err
		}
		fmt.Println("SSH 公钥策略已热更新；已有连接保留，disconnect 可显式断开")
		return nil
	case "disconnect":
		if len(args) != 2 {
			return fmt.Errorf("用法: proxyd remote ssh-keys disconnect <SHA256 指纹>")
		}
		var result struct {
			Disconnected int `json:"disconnected"`
		}
		if err := c.do(http.MethodPost, "/api/remote/ssh-keys/disconnect", map[string]string{"fingerprint": args[1]}, &result); err != nil {
			return err
		}
		fmt.Printf("已请求断开 %d 条 SSH 连接；授权保持原值\n", result.Disconnected)
		return nil
	case "del":
		if len(args) != 2 {
			return fmt.Errorf("用法: proxyd remote ssh-keys del <指纹|唯一名称>")
		}
		if err := c.do(http.MethodDelete, "/api/remote/ssh-keys", map[string]string{"identifier": args[1]}, nil); err != nil {
			return err
		}
		fmt.Println("SSH 公钥已删除，认证开关保持原值")
		return nil
	default:
		return fmt.Errorf("未知 SSH 公钥子命令 %q（list|add|import|del|export|on|off|enable|disable|expire|disconnect）", sub)
	}
}
