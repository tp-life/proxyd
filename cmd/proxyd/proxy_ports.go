package main

// 代理域子命令：代理模式与端口相关设置（mode/port-range/port-mapping/auto-port/main-*）。

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"proxyd/internal/api"
)

func cmdMode(args []string) error {
	cfgFile, rest, err := parseCFlag("mode", args)
	if err != nil {
		return err
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		ov, err := c.overview()
		if err != nil {
			return err
		}
		fmt.Printf("当前模式: %s（主端口 %d）\n", ov.Mode, ov.MixedPort)
		return nil
	}
	if len(rest) != 1 {
		return fmt.Errorf("用法: proxyd mode [-c 配置] [rule|global|direct]")
	}
	if err := c.do(http.MethodPost, "/api/mode", map[string]string{"mode": rest[0]}, nil); err != nil {
		return err
	}
	fmt.Printf("已切换到 %s 模式\n", rest[0])
	return nil
}

func cmdPortRange(args []string) error {
	cfgFile, rest, err := parseCFlag("port-range", args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("用法: proxyd port-range [-c 配置] <起-止>，如 42000-42100")
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	if err := c.do(http.MethodPost, "/api/port-range", map[string]string{"range": rest[0]}, nil); err != nil {
		return err
	}
	fmt.Printf("端口区间已更新为 %s（已重新分配端口）\n", rest[0])
	return nil
}

// parseAutoPortArg 解析 auto-port 参数："off"/"0" 表示关闭，否则为端口号。
func parseAutoPortArg(s string) (int, error) {
	if strings.EqualFold(s, "off") {
		return 0, nil
	}
	p, err := strconv.Atoi(s)
	if err != nil || p < 0 || p > 65535 {
		return 0, fmt.Errorf("无效端口 %q（1-65535，或 off 关闭）", s)
	}
	return p, nil
}

func cmdAutoPort(args []string) error {
	cfgFile, rest, err := parseCFlag("auto-port", args)
	if err != nil {
		return err
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		ov, err := c.overview()
		if err != nil {
			return err
		}
		if ov.AutoPort == 0 {
			fmt.Println("自动选优端口: 关闭")
		} else {
			fmt.Printf("自动选优端口: %d\n", ov.AutoPort)
		}
		return nil
	}
	if len(rest) != 1 {
		return fmt.Errorf("用法: proxyd auto-port [-c 配置] <端口|off>")
	}
	port, err := parseAutoPortArg(rest[0])
	if err != nil {
		return err
	}
	if err := c.do(http.MethodPost, "/api/auto-port", map[string]int{"port": port}, nil); err != nil {
		return err
	}
	if port == 0 {
		fmt.Println("自动选优端口已关闭")
	} else {
		fmt.Printf("自动选优端口已设置为 %d\n", port)
	}
	return nil
}

// cmdPortMapping 通过运行中实例的 API 查看或切换健康节点一对一端口映射。
//
// 参数：
//   - args: []string，支持 `-c <配置文件>` 和一个可选的 `on|off|status` 位置参数；
//     未提供位置参数时等价于 status。
//
// 返回值：
//   - error：参数无效、实例未运行、mihomo 热更新或配置持久化失败时返回。
//
// 错误情况：服务端事务回滚失败会作为组合错误原样输出，提醒用户检查配置与实际监听。
func cmdPortMapping(args []string) error {
	cfgFile, rest, err := parseCFlag("port-mapping", args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return fmt.Errorf("用法: proxyd port-mapping [-c 配置] [on|off|status]")
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	if len(rest) == 0 || strings.EqualFold(rest[0], "status") {
		ov, err := c.overview()
		if err != nil {
			return err
		}
		state := "关闭"
		if ov.PortMappingEnabled {
			state = "开启"
		}
		fmt.Printf("节点端口映射: %s（稳定分配 %d 个，当前监听 %d 个）\n", state, len(ov.PortAssignments), len(ov.Ports))
		return nil
	}
	enabled, err := parseOnOff(rest[0])
	if err != nil {
		return err
	}
	if err := c.do(http.MethodPost, "/api/port-mapping", map[string]bool{"enabled": enabled}, nil); err != nil {
		return err
	}
	if enabled {
		fmt.Println("节点端口映射已开启，稳定分配的一对一监听已恢复")
	} else {
		fmt.Println("节点端口映射已关闭；主端口、自动端口、分组端口和稳定分配快照保持不变")
	}
	return nil
}

func cmdMainAuto(args []string) error {
	cfgFile, rest, err := parseCFlag("main-auto", args)
	if err != nil {
		return err
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		ov, err := c.overview()
		if err != nil {
			return err
		}
		if ov.MainAuto {
			fmt.Printf("主端口最优节点: 开启（主端口 %d 跳过规则、直连最优节点）\n", ov.MixedPort)
		} else {
			fmt.Printf("主端口最优节点: 关闭（主端口 %d 走规则模式）\n", ov.MixedPort)
		}
		return nil
	}
	if len(rest) != 1 {
		return fmt.Errorf("用法: proxyd main-auto [-c 配置] [on|off]")
	}
	on, err := parseOnOff(rest[0])
	if err != nil {
		return err
	}
	if err := c.do(http.MethodPost, "/api/main-auto", map[string]bool{"enabled": on}, nil); err != nil {
		return err
	}
	if on {
		fmt.Println("主端口已切换为最优节点模式（跳过规则，与 auto-port 互不影响）")
	} else {
		fmt.Println("主端口已恢复规则模式")
	}
	return nil
}

// cmdMainNode 查看/设置主端口固定节点：无参显示当前设置；
// `off` 清除（恢复规则模式）；其余参数视为节点 key（overview 里每个节点的 key 字段）。
func cmdMainNode(args []string) error {
	cfgFile, rest, err := parseCFlag("main-node", args)
	if err != nil {
		return err
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		ov, err := c.overview()
		if err != nil {
			return err
		}
		if ov.MainNode == "" {
			fmt.Printf("主端口固定节点: 未设置（主端口 %d 走规则模式）\n", ov.MixedPort)
			return nil
		}
		name := ""
		for _, n := range ov.Nodes {
			if n.Key == ov.MainNode {
				name = n.Name
				break
			}
		}
		state := "生效中（主端口跳过规则、直达该节点）"
		switch {
		case ov.MainAuto:
			state = "main-auto 已开启，当前被忽略（auto 优先）"
		case !ov.MainNodeUp:
			state = "节点当前不可用，已回退规则模式（恢复后自动再生效）"
		}
		if name == "" {
			name = "（节点已不在列表中）"
		}
		fmt.Printf("主端口固定节点: %s\n  key: %s\n  状态: %s\n", name, ov.MainNode, state)
		return nil
	}
	if len(rest) != 1 {
		return fmt.Errorf("用法: proxyd main-node [-c 配置] [节点名|节点key|off]")
	}
	key := rest[0]
	if strings.ToLower(key) == "off" {
		key = ""
	} else {
		// 允许直接给节点名称；key 精确匹配优先，名称需唯一
		ov, err := c.overview()
		if err != nil {
			return err
		}
		key, err = resolveNodeKey(ov, key)
		if err != nil {
			return err
		}
	}
	if err := c.do(http.MethodPost, "/api/main-node", map[string]string{"node": key}, nil); err != nil {
		return err
	}
	if key == "" {
		fmt.Println("主端口已恢复规则模式（main-node 已清除）")
	} else {
		fmt.Println("主端口已固定到指定节点（跳过规则）；main-auto 开启时该设置不生效")
	}
	return nil
}

func cmdMainPort(args []string) error {
	cfgFile, rest, err := parseCFlag("main-port", args)
	if err != nil {
		return err
	}
	c, err := newAPIClient(cfgFile)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		ov, err := c.overview()
		if err != nil {
			return err
		}
		fmt.Printf("主端口: %d\n", ov.MixedPort)
		return nil
	}
	if len(rest) != 1 {
		return fmt.Errorf("用法: proxyd main-port [-c 配置] <端口>")
	}
	port, err := strconv.Atoi(rest[0])
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("无效端口 %q（1-65535）", rest[0])
	}
	if err := c.do(http.MethodPost, "/api/main-port", map[string]int{"port": port}, nil); err != nil {
		return err
	}
	fmt.Printf("主端口已改为 %d（已热更新；系统代理开启时已自动重绑）\n", port)
	return nil
}

// resolveNodeKey 把用户输入的节点 key 或名称解析为节点 key。
// key 精确匹配优先；名称匹配要求唯一，重名时列出候选引导用户改用 key。
func resolveNodeKey(ov *api.Overview, target string) (string, error) {
	for _, n := range ov.Nodes {
		if n.Key == target {
			return n.Key, nil
		}
	}
	var matches []api.NodeEntry
	for _, n := range ov.Nodes {
		if n.Name == target {
			matches = append(matches, n)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("节点 %q 不存在（可用 proxyd nodes 查看节点列表）", target)
	case 1:
		return matches[0].Key, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "存在 %d 个同名节点 %q，请改用 key 指定：", len(matches), target)
	for _, n := range matches {
		fmt.Fprintf(&b, "\n  %s（订阅 %s，端口 %d）", n.Key, n.Subscription, n.Port)
	}
	return "", fmt.Errorf("%s", b.String())
}
