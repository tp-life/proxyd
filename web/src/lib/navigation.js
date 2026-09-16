/**
 * 导航注册表统一维护页面归属、URL 与命令菜单，新增任务页只需在本文件登记。
 * 页面组件仍负责自己的业务交互；不在导航模型中嵌入 API 或配置写入规则。
 */
import { Activity, Gauge, Laptop, Layers, Link2, ListFilter, Monitor, Network, Router, Rss, Settings, Shield, Terminal, Server, KeyRound, ArrowLeftRight, History } from "lucide-react";

export const NAV_GROUPS = [
  { id: "overview", label: "总览", icon: Activity },
  { id: "proxy", module: "proxy", label: "代理", icon: Network },
  { id: "gateway", module: "gateway", label: "网关", icon: Router },
  { id: "remote", module: "remote", label: "远程访问", icon: Laptop },
  { id: "system", label: "系统", icon: Settings },
];

export const NAV_ITEMS = [
  { id: "overview", label: "总览", group: "overview", icon: Activity },
  { id: "proxy/overview", label: "运行概览", group: "proxy", section: "运行状态", icon: Activity },
  { id: "nodes", label: "代理节点", group: "proxy", section: "代理资源", icon: Network },
  { id: "subscriptions", label: "订阅管理", group: "proxy", section: "代理资源", icon: Rss },
  { id: "ports", label: "代理入口", group: "proxy", section: "代理资源", icon: Gauge },
  { id: "groups", label: "策略分组", group: "proxy", section: "代理资源", icon: Layers },
  { id: "rules", label: "访问规则", group: "proxy", section: "代理资源", icon: ListFilter },
  { id: "connections", label: "活动连接", group: "proxy", section: "代理资源", icon: Link2 },
  { id: "proxy/tailscale", label: "Tailscale", group: "proxy", section: "VPN 接入", icon: Network, keywords: "Tailnet Headscale tsnet VPN 接入" },
  { id: "proxy/openvpn", label: "OpenVPN", group: "proxy", section: "VPN 接入", icon: Shield, keywords: "OpenVPN VPN 隧道 证书" },
  { id: "proxy/settings", label: "代理设置", group: "proxy", section: "代理配置", icon: Settings, keywords: "DNS TUN 系统代理 端口 固定节点" },
  { id: "gateway", keywords: "旁路由 网关 LAN 设备 分流 helper", label: "LAN 网关", group: "gateway", icon: Router, detail: "登记局域网设备，把它的网关/DNS 指向本机即可分流。" },
  { id: "remote/devices", keywords: "SSH 连接 诊断 diagnose 设备", label: "设备与连接", group: "remote", icon: Laptop, detail: "保存远端设备，连接 SSH 并检查链路。" },
  { id: "remote/services", label: "本机服务", group: "remote", icon: Server, detail: "管理本机隧道、SSH、浏览器终端和暴露端口。" },
  { id: "remote/access", keywords: "SSH 公钥 私钥 密钥 key 授权 白名单 到期", label: "访问授权", group: "remote", icon: KeyRound, detail: "管理 SSH 公钥、客户端白名单及隧道身份。" },
  { id: "remote/forwards", label: "端口转发", group: "remote", icon: ArrowLeftRight, detail: "将本机监听端口转发到远端设备。" },
  { id: "remote/audit", label: "连接审计", group: "remote", icon: History, detail: "查看隧道连接、SSH 认证和会话结束事件。" },
  { id: "desktop", label: "远程桌面", group: "remote", icon: Monitor },
  { id: "modules", label: "模块管理", group: "system", icon: Layers, keywords: "启用 禁用 总开关 功能" },
  { id: "diagnostics", label: "诊断中心", group: "system", icon: Activity, keywords: "DNS DERP SSH 环境 检查 排障" },
  { id: "config-history", label: "配置历史", group: "system", icon: History, keywords: "版本 恢复 回滚 备份" },
  { id: "logs", label: "运行日志", group: "system", icon: Terminal },
  { id: "settings", label: "通用设置", group: "system", icon: Shield, keywords: "系统设置 开机自启 版本检查 导入 导出 备份 重启" },
];

/**
 * normalizeView 将 URL 或旧页面标识解析为已注册任务页。
 * 参数说明：value 为 string，页面 id 或 hash 路径。
 * 返回值说明：string，保证对应唯一注册页面。
 * 错误情况：未知值回到总览；旧 remote 入口兼容到设备与连接。
 */
export function normalizeView(value) {
  const id = String(value || "").replace(/^#\/?/, "");
  if (id === "remote") return "remote/devices";
  return NAV_ITEMS.some((item) => item.id === id) ? id : "overview";
}

/**
 * isRemoteView 判断是否属于 remote 数据页面，避免切换子页时误停数据加载。
 * 参数说明：view 为 string，规范化页面 id。
 * 返回值说明：boolean。
 * 错误情况：未知或空页面返回 false。
 */
export function isRemoteView(view) {
  return typeof view === "string" && view.startsWith("remote/");
}

/**
 * visibleNavigation 按模块状态生成统一可见导航，供顶部、侧边栏与命令菜单共用。
 * 参数：modules 为 Array<object>，后端模块快照；返回 { groups: Array, items: Array }。
 * 错误情况：尚未取得模块状态时暂不展示模块入口，总览和系统始终保留，避免禁用入口闪现。
 */
export function visibleNavigation(modules) {
  const groups = NAV_GROUPS.filter((group) => !group.module || modules.some((module) => module.id === group.module && module.enabled));
  const items = NAV_ITEMS.filter((item) => groups.some((group) => group.id === item.group));
  return { groups, items };
}

/**
 * resolveModuleView 将禁用模块的地址引导到模块管理，覆盖直接链接和浏览器前进后退。
 * 参数：value 为 string 页面地址，modules 为 Array<object> 模块快照；返回合法页面 id。
 * 错误情况：状态尚未知时保留原地址，避免首次加载丢失书签；只有明确禁用才替换目标。
 */
export function resolveModuleView(value, modules) {
  const view = normalizeView(value);
  const item = NAV_ITEMS.find((entry) => entry.id === view);
  const group = NAV_GROUPS.find((entry) => entry.id === item.group);
  return group.module && modules.some((module) => module.id === group.module && module.enabled === false) ? "modules" : view;
}
