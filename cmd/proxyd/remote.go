package main

// 「远程连接」周边模块的 CLI：status/on/off/token/serve/remotes/forwards
// 作为运行中实例的 API 客户端实现；pipe 与 ssh 是纯客户端命令，不依赖守护进程。

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"

	"proxyd/internal/config"
	"proxyd/internal/remote"
)

// remoteStatusJSON 对应 GET /api/remote 的响应（token 为打码摘要）。
type remoteStatusJSON struct {
	Enabled  bool   `json:"enabled"`
	Running  bool   `json:"running"`
	Error    string `json:"error,omitempty"`
	Token    string `json:"token,omitempty"`
	Region   string `json:"region,omitempty"`
	Serve    []int  `json:"serve"`
	Forwards []struct {
		Name       string `json:"name"`
		Listen     string `json:"listen"`
		Remote     string `json:"remote"`
		RemotePort int    `json:"remote_port"`
		Enabled    bool   `json:"enabled"`
		Running    bool   `json:"running"`
		Active     int64  `json:"active"`
		LastError  string `json:"last_error,omitempty"`
	} `json:"forwards"`
}

// cmdRemote 是 remote 命令组入口；pipe 子命令不经过 API，可独立运行。
func cmdRemote(args []string) error {
	cfgFile, rest, err := parseCFlag("remote", args)
	if err != nil {
		return err
	}
	sub := "status"
	if len(rest) > 0 {
		sub = rest[0]
	}
	// pipe 是 ProxyCommand 用的纯客户端管道，无需守护进程。
	if sub == "pipe" {
		return cmdRemotePipe(rest[1:])
	}

	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	switch sub {
	case "status":
		return remotePrintStatus(c)
	case "on", "off":
		enabled := sub == "on"
		var st remoteStatusJSON
		if err := c.do(http.MethodPost, "/api/remote", map[string]bool{"enabled": enabled}, &st); err != nil {
			return err
		}
		if enabled {
			fmt.Println("远程连接服务端已开启（proxyd remote token 查看连接 token）")
		} else {
			fmt.Println("远程连接服务端已关闭")
		}
		return nil
	case "token":
		var out struct {
			Token string `json:"token"`
		}
		if err := c.do(http.MethodGet, "/api/remote/token", nil, &out); err != nil {
			return err
		}
		if out.Token == "" {
			return fmt.Errorf("服务端未在运行，没有可用 token（先 proxyd remote on）")
		}
		fmt.Println(out.Token)
		return nil
	case "serve":
		return cmdRemoteServe(c, rest[1:])
	case "remotes":
		return cmdRemoteRemotes(c, rest[1:])
	case "forwards":
		return cmdRemoteForwards(c, rest[1:])
	}
	return fmt.Errorf("未知子命令 %q（status|on|off|token|serve|remotes|forwards|pipe）", sub)
}

// remotePrintStatus 打印远程连接状态汇总。
func remotePrintStatus(c *apiClient) error {
	var st remoteStatusJSON
	if err := c.do(http.MethodGet, "/api/remote", nil, &st); err != nil {
		return err
	}
	state := "关闭"
	if st.Running {
		state = "运行中"
	} else if st.Enabled {
		state = "配置已开启，但未在运行"
	}
	fmt.Printf("远程连接：%s\n", state)
	if st.Error != "" {
		fmt.Printf("错误：%s\n", st.Error)
	}
	if st.Token != "" {
		fmt.Printf("本机 token：%s（proxyd remote token 查看完整值）\n", st.Token)
	}
	if st.Region != "" {
		fmt.Printf("DERP 区域：%s\n", st.Region)
	}
	fmt.Printf("暴露端口：%s\n", formatPorts(st.Serve))
	if len(st.Forwards) > 0 {
		fmt.Println("\n本地转发：")
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tLISTEN\tREMOTE\tENABLED\tRUNNING\tACTIVE\tERROR")
		for _, f := range st.Forwards {
			fmt.Fprintf(tw, "%s\t%s\t%s:%d\t%v\t%v\t%d\t%s\n",
				f.Name, f.Listen, f.Remote, f.RemotePort, f.Enabled, f.Running, f.Active, f.LastError)
		}
		tw.Flush()
	}
	return nil
}

// formatPorts 把端口列表格式化为逗号分隔文本。
func formatPorts(ports []int) string {
	if len(ports) == 0 {
		return "（无）"
	}
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ", ")
}

// cmdRemoteServe 查看或设置经隧道暴露的本机端口。
func cmdRemoteServe(c *apiClient, args []string) error {
	if len(args) == 0 {
		var st remoteStatusJSON
		if err := c.do(http.MethodGet, "/api/remote", nil, &st); err != nil {
			return err
		}
		fmt.Printf("暴露端口：%s\n", formatPorts(st.Serve))
		return nil
	}
	if len(args) != 1 {
		return fmt.Errorf("用法: proxyd remote serve [端口,端口...]（空串清空）")
	}
	var ports []int
	if strings.TrimSpace(args[0]) != "" {
		for _, s := range strings.Split(args[0], ",") {
			p, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil {
				return fmt.Errorf("端口 %q 无效", s)
			}
			ports = append(ports, p)
		}
	}
	var st remoteStatusJSON
	if err := c.do(http.MethodPost, "/api/remote/serve", map[string]any{"ports": ports}, &st); err != nil {
		return err
	}
	fmt.Printf("暴露端口已更新：%s\n", formatPorts(st.Serve))
	return nil
}

// cmdRemoteRemotes 管理保存的远端（名称 → token）。
func cmdRemoteRemotes(c *apiClient, args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list":
		var out struct {
			Remotes []struct {
				Name  string `json:"name"`
				Token string `json:"token"`
			} `json:"remotes"`
		}
		if err := c.do(http.MethodGet, "/api/remote/remotes", nil, &out); err != nil {
			return err
		}
		if len(out.Remotes) == 0 {
			fmt.Println("（无保存的远端）")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tTOKEN")
		for _, r := range out.Remotes {
			fmt.Fprintf(tw, "%s\t%s\n", r.Name, r.Token)
		}
		tw.Flush()
		return nil
	case "add":
		if len(args) != 3 {
			return fmt.Errorf("用法: proxyd remote remotes add <名> <token>")
		}
		var entry struct {
			Name string `json:"name"`
		}
		if err := c.do(http.MethodPost, "/api/remote/remotes",
			map[string]string{"name": args[1], "token": args[2]}, &entry); err != nil {
			return err
		}
		fmt.Printf("远端 %q 已保存（proxyd ssh %s 可直接连接）\n", entry.Name, entry.Name)
		return nil
	case "del":
		if len(args) != 2 {
			return fmt.Errorf("用法: proxyd remote remotes del <名>")
		}
		if err := c.do(http.MethodDelete, "/api/remote/remotes/"+args[1], nil, nil); err != nil {
			return err
		}
		fmt.Printf("远端 %q 已删除\n", args[1])
		return nil
	}
	return fmt.Errorf("未知子命令 %q（list|add|del）", sub)
}

// cmdRemoteForwards 管理本地转发。
func cmdRemoteForwards(c *apiClient, args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list":
		return remotePrintStatus(c)
	case "add":
		if len(args) != 5 {
			return fmt.Errorf("用法: proxyd remote forwards add <名> <监听地址> <远端名|token> <远端端口>")
		}
		port, err := strconv.Atoi(args[4])
		if err != nil {
			return fmt.Errorf("远端端口 %q 无效", args[4])
		}
		if err := c.do(http.MethodPost, "/api/remote/forwards", map[string]any{
			"name": args[1], "listen": args[2], "remote": args[3], "remote_port": port,
		}, nil); err != nil {
			return err
		}
		fmt.Printf("转发 %q 已创建并启用\n", args[1])
		return nil
	case "del":
		if len(args) != 2 {
			return fmt.Errorf("用法: proxyd remote forwards del <名>")
		}
		if err := c.do(http.MethodDelete, "/api/remote/forwards/"+args[1], nil, nil); err != nil {
			return err
		}
		fmt.Printf("转发 %q 已删除\n", args[1])
		return nil
	case "on", "off":
		if len(args) != 2 {
			return fmt.Errorf("用法: proxyd remote forwards %s <名>", sub)
		}
		if err := c.do(http.MethodPut, "/api/remote/forwards/"+args[1], map[string]bool{"enabled": sub == "on"}, nil); err != nil {
			return err
		}
		fmt.Printf("转发 %q 已%s\n", args[1], map[string]string{"on": "启用", "off": "停用"}[sub])
		return nil
	}
	return fmt.Errorf("未知子命令 %q（list|add|del|on|off）", sub)
}

// cmdRemotePipe 把 stdio 与远端隧道端口双向管道化（OpenSSH ProxyCommand 用法）。
// 纯客户端命令，不需要守护进程运行。
func cmdRemotePipe(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("用法: proxyd remote pipe <token> <端口>")
	}
	port, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("端口 %q 无效", args[1])
	}
	return remote.Pipe(context.Background(), args[0], port, os.Stdin, os.Stdout)
}

// cmdSCP 经 tailcat 隧道执行系统 scp（镜像 cmdSSH）：远端操作数以 tc... token
// 作为主机名（可带 user@ 前缀），以 `proxyd remote pipe` 作为 ProxyCommand
// 转发到对端 22 端口，token 主机名因此不会进入 DNS 解析。纯客户端命令。
func cmdSCP(args []string) error {
	token, err := findTunnelToken(args)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	scpExe, err := exec.LookPath("scp")
	if err != nil {
		return fmt.Errorf("未找到系统 scp 客户端（%v）", err)
	}

	// ProxyCommand 由 scp/ssh 经 shell 执行，引号规则与 cmdSSH 一致：
	// Windows 下 cmd.exe 不允许 Go %q 的反斜杠转义，用普通双引号包路径。
	quotedExe := fmt.Sprintf("%q", exe)
	if runtime.GOOS == "windows" {
		quotedExe = "\"" + exe + "\""
	}
	argv := []string{"-o", fmt.Sprintf("ProxyCommand=%s remote pipe %s 22", quotedExe, token)}
	argv = append(argv, args...)

	cmd := exec.Command(scpExe, argv...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}
	return nil
}

// findTunnelToken 在 scp 参数中定位隧道远端操作数并返回其 token。
//
// 参数：
//   - args: []string，scp 子命令的原始参数（选项与操作数混合）。
//
// 返回值：
//   - string：唯一出现的 tc... 隧道 token。
//   - error：没有隧道操作数时返回用法错误；源与目标出现两个不同 token 时返回
//     单跳限制错误（隧道不支持 tc→tc 一跳中转）。
//
// 错误情况：只有主机部分（剥掉可选 user@ 前缀与 :path 后缀后）通过
// remote.ValidateToken 的参数才被视为隧道操作数，其余参数原样透传给 scp。
func findTunnelToken(args []string) (string, error) {
	token := ""
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		host := a
		if i := strings.LastIndex(host, "@"); i >= 0 {
			host = host[i+1:]
		}
		if i := strings.Index(host, ":"); i >= 0 {
			host = host[:i]
		}
		if remote.ValidateToken(host) != nil {
			continue
		}
		if token == "" {
			token = host
		} else if token != host {
			return "", fmt.Errorf("源与目标使用了不同的隧道 token；隧道只支持单跳，请分两次传输")
		}
	}
	if token == "" {
		return "", fmt.Errorf("用法: proxyd scp [scp选项...] <源...> <目标>（远端操作数以 tc... token 作为主机名，如 tc-xxxx:/tmp/ 或 root@tc-xxxx:/tmp/）")
	}
	return token, nil
}

// cmdSSH 经 tailcat 隧道执行系统 ssh（等价 tailcat ssh）：解析远端名 → token，
// 以 `proxyd remote pipe` 作为 ProxyCommand 启动系统 ssh。纯客户端命令。
func cmdSSH(args []string) error {
	cfgFile, rest, err := parseCFlag("ssh", args)
	if err != nil {
		return err
	}
	// 解析 -p 端口（默认 22）；首个非 flag 参数为 [user@]<远端名|token>，其余透传给 ssh。
	port := "22"
	var dst string
	var sshArgs []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "-p" && i+1 < len(rest):
			port = rest[i+1]
			i++
		case strings.HasPrefix(a, "-p") && len(a) > 2:
			port = a[2:]
		case dst == "" && !strings.HasPrefix(a, "-"):
			dst = a
		default:
			sshArgs = append(sshArgs, a)
		}
	}
	if dst == "" {
		return fmt.Errorf("用法: proxyd ssh [-c 配置] [-p 端口] [user@]<远端名|token> [ssh 参数/远端命令...]")
	}
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum <= 0 || portNum > 65535 {
		return fmt.Errorf("端口 %q 无效", port)
	}

	user, name, hasUser := strings.Cut(dst, "@")
	if !hasUser {
		name = user
		user = ""
	}

	// 名称 → token：读配置文件的 remotes；直接给 tc... token 时原样使用（无需配置文件）。
	var token string
	if strings.HasPrefix(name, "tc") {
		token = name
	} else {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return fmt.Errorf("读取配置 %s 失败（用于解析远端名）: %w", cfgFile, err)
		}
		if token, err = remote.ResolveToken(cfg.Remote.Remotes, name); err != nil {
			return err
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	sshExe, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("未找到系统 ssh 客户端（%v）；可改用 proxyd remote forwards 做本地转发后自行连接", err)
	}

	// ssh 会把目标主机名代入 ControlPath 等展开，token 太长会撑爆 Unix socket 路径，
	// 因此给 ssh 一个短主机名；真实 token 只出现在 ProxyCommand 参数里。
	sshHost := name
	if strings.HasPrefix(name, "tc") {
		sshHost = "tailcat-" + token[len(token)-8:]
	}
	sshDst := sshHost
	if user != "" {
		sshDst = user + "@" + sshHost
	}
	// ProxyCommand 由 ssh 经 shell 执行：Windows 下 cmd.exe 不允许 Go %q 的反斜杠转义
	// （会吃掉路径里的反斜杠），因此 Windows 用普通双引号包路径。
	quotedExe := fmt.Sprintf("%q", exe)
	if runtime.GOOS == "windows" {
		quotedExe = "\"" + exe + "\""
	}
	proxyCmd := fmt.Sprintf("%s remote pipe %s %d", quotedExe, token, portNum)

	argv := []string{
		"-o", "UpdateHostKeys no",
		"-o", "StrictHostKeyChecking no",
		"-o", "UserKnownHostsFile " + os.DevNull,
		"-o", "LogLevel ERROR",
		"-o", "ProxyCommand=" + proxyCmd,
		sshDst,
	}
	argv = append(argv, sshArgs...)

	cmd := exec.Command(sshExe, argv...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}
	return nil
}
