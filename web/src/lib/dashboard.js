/** 总览展示模型：纯函数将各模块快照投影为统一摘要，不依赖 HTTP、React 或第三方 SDK。 */

export const PHASE_LABELS = { idle: "待命", starting: "正在启动", running: "运行中", retrying: "等待重试", degraded: "部分功能异常", failed: "启动失败", disabled: "已禁用" };
const ATTENTION_PHASES = new Set(["retrying", "degraded", "failed"]);

/**
 * proxySummary 提取代理运行指标与需要检查的节点状态。
 * 参数：context 为包含 overview、data 的 object；返回 {metrics: Array, items: Array}。
 * 错误：缺少快照时返回未知值；节点尚未测速不能据此断言整个守护进程停止。
 */
export function proxySummary({ overview, data }) {
  const nodes = overview?.nodes || [];
  const alive = nodes.filter((node) => node.alive).length;
  return {
    metrics: [
      { label: "代理模式", value: overview ? ({ rule: "规则", global: "全局", direct: "直连" }[overview.mode] || overview.mode) : null },
      { label: "可用节点", value: overview ? `${alive} / ${nodes.length}` : null },
      { label: "活动代理连接", value: data.connections ? (data.connections.connections || []).length : null },
    ],
    items: overview && !alive && overview.mode !== "direct" ? [{ id: "proxy-nodes", title: "代理暂无已确认可用的节点", detail: "检查节点配置或进入运行概览测速。", view: "proxy/overview" }] : [],
  };
}

/**
 * remoteSummary 提取远程服务、设备档案与连接指标，汇总授权和转发待办。
 * 参数：context 为 {data: object, now: number}，now 为当前毫秒时间；返回统一摘要。
 * 错误：未读取到数据时展示未知；设备档案不等同在线设备，入站连接与 SSH 授权计数不重复相加。
 */
export function remoteSummary({ data, now = Date.now() }) {
  const remote = data.remote;
  const items = [];
  if (remote) {
    const keys = (remote.ssh_keys || []).filter((key) => !key.disabled && key.expires_at);
    const expired = keys.filter((key) => key.expired || Date.parse(key.expires_at) <= now).length;
    const expiring = keys.filter((key) => !key.expired && Date.parse(key.expires_at) > now && Date.parse(key.expires_at) <= now + 7 * 86400000).length;
    if (expired || expiring) items.push({ id: "remote-keys", title: "SSH 授权需要检查", detail: `${expired} 个密钥已到期，${expiring} 个将在 7 天内到期。`, view: "remote/access" });
    if ((remote.forwards || []).some((forward) => forward.enabled && forward.last_error)) items.push({ id: "remote-forwards", title: "部分端口转发启动失败", detail: "检查监听地址、端口占用与远端配置。", view: "remote/forwards" });
    if (remote.enabled && !remote.running && remote.error) items.push({ id: "remote-server", title: "远程服务端未启动", detail: remote.error, view: "remote/services" });
  }
  return {
    metrics: [
      { label: "隧道服务端", value: remote ? (remote.running ? "运行中" : remote.enabled ? "未就绪" : "未启用") : null },
      { label: "已保存设备", value: data.devices ? (data.devices.remotes || []).length : null },
      { label: "入站隧道连接", value: remote ? (remote.peers || []).reduce((sum, peer) => sum + (peer.active || 0), 0) : null },
      { label: "运行中转发", value: remote ? (remote.forwards || []).filter((forward) => forward.running).length : null },
      { label: "桌面会话", value: data.desktop ? (data.desktop.sessions || []).length : null },
    ],
    items,
  };
}

/** 模块摘要注册表只声明展示入口与投影函数；新增模块不必改动总览页面结构。 */
const SUMMARY_PROVIDERS = {
  proxy: { view: "proxy/overview", description: "代理流量、节点与连接", summarize: proxySummary },
  remote: { view: "remote/devices", description: "远程服务、设备与会话", summarize: remoteSummary },
};

/**
 * buildDashboard 组合已启用模块摘要、生命周期异常和系统待办。
 * 参数：modules 为 Array<object>；context 为数据快照；返回 {cards: Array, items: Array}。
 * 错误：禁用模块的旧错误与指标一律排除；新模块无专用摘要时仍可展示状态并进入模块管理。
 */
export function buildDashboard(modules, context) {
  const cards = modules.filter((module) => module.enabled).map((module) => {
    const provider = SUMMARY_PROVIDERS[module.id];
    return { ...module, label: PHASE_LABELS[module.phase] || "状态未知", attention: ATTENTION_PHASES.has(module.phase), view: provider?.view || "modules", description: provider?.description || "模块运行状态", ...(provider ? provider.summarize(context) : { metrics: [], items: [] }) };
  });
  const items = cards.flatMap((card) => [
    ...(card.attention || card.error ? [{ id: `${card.id}-lifecycle`, title: `${card.name} · ${card.label}`, detail: card.error || "请进入模块管理检查运行状态。", view: "modules", retryAt: card.next_retry_at }] : []),
    ...card.items,
  ]);
  if (context.data.system?.pending_restart) items.unshift({ id: "system-restart", title: "配置已恢复，等待重启生效", detail: "请检查配置历史并重启守护进程，期间暂停修改设置。", view: "config-history" });
  return { cards, items };
}
