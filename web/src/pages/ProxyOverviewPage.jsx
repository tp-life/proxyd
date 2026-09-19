/** 代理运行概览模块，仅呈现代理上下文的详细状态与控制操作。 */
import { useRef, useState } from "react";
import {
  ArrowRight,
  ArrowRightLeft,
  CheckCircle2,
  CircleAlert,
  CircleHelp,
  Copy,
  ExternalLink,
  Gauge,
  Globe2,
  Laptop,
  ListFilter,
  Menu,
  RefreshCw,
  Search,
  Settings,
  Shield,
  ShieldCheck,
  Sparkles,
  Target,
  Terminal,
} from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { SegmentedControl } from "@/components/ui/segmented-control";
import { Switch as UISwitch } from "@/components/ui/switch";
import { PanelTitle } from "@/components/PanelTitle";
import { StatusBadge } from "@/components/StatusBadge";
import { FixedNodeDialog } from "@/components/FixedNodeDialog";
import { MODE_LABELS } from "@/lib/constants";
import { classNames, delayClass, formatBytes, formatDelay } from "@/lib/format";

/**
 * MODE_HELP 描述 mihomo 三种运行模式的真实选路语义。
 *
 * 功能说明：
 * 这组说明不仅是界面文案，也是防止误操作的业务边界提示。mode 决定主入口是否走
 * 访问规则：规则模式下未命中规则的流量落到内置 PROXY 组的「默认出口」；全局模式
 * 交给内置 GLOBAL 组；直连模式全部直走。节点专属端口、自动选优端口和策略分组端口
 * 都有独立出口，不读取这里的模式。
 */
const MODE_HELP = {
  rule: {
    title: "按规则决定出口",
    detail: "依次匹配自定义规则、远程规则源和内置规则；首条命中立即生效，未命中的流量走默认出口。",
  },
  global: {
    title: "全部交给代理组",
    detail: "主入口流量跳过访问规则，统一进入 GLOBAL 选择组；默认出口仅在规则模式下生效。",
  },
  direct: {
    title: "全部直接连接",
    detail: "主入口流量跳过访问规则与代理节点，直接访问目标；适合临时排查代理链路问题。",
  },
};

/**
 * ProxyOverviewPage 渲染代理运行概览，保留主入口模式、默认出口与节点管理操作。
 *
 * 参数说明：
 * - overview: object，/api/overview 响应（含内置 PROXY 分组的选中项）。
 * - aliveCount: number，可用节点数量。
 * - busy/loading: string | boolean，全局后台操作与同步状态。
 * - traffic: object，实时速率流状态。
 * - onCopy/onCopyEnv/onMenu/onMode/onNavigate/onPalette/onPortMapping/onRefresh/onSystemProxy/onTest/onTun: Function，用户操作回调。
 * - onSelectNode: (value: string) => Promise<boolean>，写入默认出口（节点名 / AUTO / DIRECT）并刷新概览，失败返回 false。
 *
 * 返回值说明：
 * 返回概览页 React 元素。
 *
 * 可能的异常/错误情况：
 * 无；操作失败由父组件 toast。
 */
export function ProxyOverviewPage({
  overview,
  aliveCount,
  busy,
  loading,
  traffic,
  onCopy,
  onCopyEnv,
  onMenu,
  onMode,
  onNavigate,
  onPalette,
  onPortMapping,
  onRefresh,
  onSelectNode,
  onSystemProxy,
  onTest,
  onTun,
}) {
  const [nodePickerOpen, setNodePickerOpen] = useState(false);
  const nodePickerTrigger = useRef(null);
  /** openNodePicker 打开候选弹窗并记录焦点入口；event 为 React.MouseEvent，返回 void，无同步异常。 */
  function openNodePicker(event) {
    nodePickerTrigger.current = event.currentTarget;
    setNodePickerOpen(true);
  }
  const updated = overview.server_time ? new Date(overview.server_time) : new Date();
  const ready = aliveCount > 0 && overview.mixed_port > 0;
  const takenOver = Boolean(overview.system_proxy || overview.tun?.active || overview.tun?.enabled);
  /*
   * 默认出口只由内置 PROXY 组的持久化选中项表达：节点名 / "AUTO" / "DIRECT"。
   * 选中节点失效时生成层会忽略该值并落回成员首位，这里据此标记「已回退」，
   * 不把失效节点误报为真实出口。该出口只在规则模式生效，TUN 与网关的
   * 未命中流量同样经 PROXY 组转发。
   */
  const exit = resolveDefaultExit(overview);
  const canAuto = (overview.nodes || []).some((node) => node.alive && !node.tunnel);
  const modeLabel = MODE_LABELS[overview.mode] || overview.mode || "未知";
  const exitLabel = exit.label;
  const exitDetail = describeExit(exit);
  const exitOk = exit.kind === "direct" || Boolean(exit.node && !exit.fallback);
  const exitStatus = exit.kind === "direct" ? "直连" : exit.fallback ? "已回退" : exit.ok ? "可用" : "不可用";
  const modeHelp = MODE_HELP[overview.mode] || {
    title: "使用后端配置模式",
    detail: `当前后端返回未识别模式“${overview.mode || "未知"}”，界面不会推测其选路行为。`,
  };
  const attentionItems = buildOverviewAttention(overview, traffic, aliveCount, exit);
  const routeSummary = overview.mode === "rule"
    ? `流量按「规则」模式匹配；未命中规则的流量由默认出口「${exitLabel}」转发。`
    : `流量按「${modeLabel}」模式处理；默认出口仅在规则模式生效。`;

  return (
    <section className="overview-shell">
      <aside className="policy-pane" aria-labelledby="policy-pane-title">
        <header className="policy-pane-header">
          <div><span>主代理入口</span><h1 id="policy-pane-title">主入口策略</h1></div>
          <Button aria-label="打开代理设置" size="icon" variant="outline" type="button" onClick={() => onNavigate("proxy/settings")}><Settings size={17} aria-hidden="true" /></Button>
        </header>
        <div className="policy-options" role="list" aria-label="默认出口">
          <PolicyOption active detail={exitDetail} icon={exitIcon(exit)} label="默认出口" tone="indigo" onClick={openNodePicker} />
        </div>
        <section className="policy-mode">
          <PanelTitle title="规则模式" detail="全局与直连模式不使用默认出口" />
          <SegmentedControl ariaLabel="流量处理模式" className="policy-mode-control" onValueChange={onMode} options={Object.entries(MODE_LABELS).map(([value, label]) => ({ value, label }))} value={overview.mode} />
          <div className="policy-mode-note">
            <CircleHelp size={16} aria-hidden="true" />
            <div>
              <strong>{modeHelp.title}</strong>
              <p>{modeHelp.detail}</p>
              <small>仅影响主代理入口；节点端口、自动选优端口和策略分组端口不受影响。默认出口仅规则模式生效，TUN 与网关流量同样走该出口。</small>
            </div>
          </div>
        </section>
        <footer className="policy-pane-footer">
          <span>主入口</span>
          <button className="copy-link" type="button" onClick={() => onCopy(overview.mixed_port)}>127.0.0.1:{overview.mixed_port}<Copy size={14} aria-hidden="true" /></button>
          <button className="copy-link" title="复制 http_proxy/https_proxy/all_proxy 环境变量" type="button" onClick={() => onCopyEnv(overview.mixed_port)}>环境变量<Terminal size={14} aria-hidden="true" /></button>
          <small>{aliveCount}/{overview.nodes.length} 个节点可用</small>
        </footer>
      </aside>

      <div className="overview-detail">
        <header className="overview-hero">
          <div className={classNames("hero-status-icon", ready ? "ready" : "attention")}>
            {ready ? <ShieldCheck size={34} aria-hidden="true" /> : <CircleAlert size={34} aria-hidden="true" />}
          </div>
          <div className="overview-hero-copy">
            <span className="overview-mobile-heading"><Button className="mobile-only" size="icon" variant="outline" type="button" onClick={onMenu} aria-label="打开导航"><Menu size={18} aria-hidden="true" /></Button>运行概览</span>
            <h2>{ready ? (takenOver ? "代理已接管" : "代理入口已就绪") : "代理服务需要检查"}</h2>
            <p>{ready ? (overview.mode === "rule" ? `当前主入口按规则匹配，未命中规则的流量走默认出口「${exitLabel}」` : `当前主入口为${modeLabel}模式`) : "当前没有健康节点，请同步订阅或检查节点配置"}</p>
            <div className="hero-badges">
              <Badge variant={ready ? "success" : "destructive"}>{ready ? "当前生效" : "需要处理"}</Badge>
              {overview.tun?.enabled && <Badge variant="outline">TUN 已开启</Badge>}
              {overview.system_proxy && <Badge variant="outline">系统代理已开启</Badge>}
            </div>
          </div>
          <div className="overview-hero-actions">
            <Button className="command-button" size="icon" variant="outline" type="button" onClick={onPalette} aria-label="打开命令菜单"><Search size={16} aria-hidden="true" /></Button>
            <Button disabled={Boolean(busy)} loading={busy === "测速"} variant="outline" type="button" onClick={onTest}>{busy !== "测速" && <Gauge size={16} aria-hidden="true" />}{busy === "测速" ? "测速中…" : "测速"}</Button>
            <Button disabled={Boolean(busy)} loading={busy === "刷新订阅"} type="button" onClick={onRefresh}>{busy !== "刷新订阅" && <RefreshCw className={classNames((busy || loading) && "animate-spin")} size={16} aria-hidden="true" />}{busy === "刷新订阅" ? "同步中…" : "同步"}</Button>
          </div>
        </header>

        <VersionNotice status={overview.version_check} />

        <section className="effective-route" aria-labelledby="effective-route-title">
          <div className="section-heading-row">
            <div><span>当前生效</span><h2 id="effective-route-title">路由概览</h2></div>
            <small>最后更新 {updated.toLocaleTimeString("zh-CN", { hour12: false })}</small>
          </div>
          <div className="route-flow">
            <RouteStep detail={takenOver ? (overview.tun?.enabled ? "TUN 接管" : "系统代理") : "手动配置入口"} icon={Laptop} label="本机应用" tone="blue" />
            <ArrowRight className="route-arrow" size={20} aria-hidden="true" />
            <RouteStep detail={`127.0.0.1:${overview.mixed_port}`} icon={Shield} label="主代理入口" tone="blue" />
            <ArrowRight className="route-arrow" size={20} aria-hidden="true" />
            <RouteStep detail={`${modeLabel}模式`} icon={ListFilter} label="匹配策略" tone="indigo" />
            <ArrowRight className="route-arrow" size={20} aria-hidden="true" />
            <RouteStep detail={exitLabel} icon={Globe2} label="实际出口" tone="teal" />
          </div>
          <p className="route-summary">{routeSummary}</p>
        </section>

        <div className="overview-lower-grid">
          <section className="exit-summary" aria-labelledby="exit-summary-title">
            <div className="section-heading-row compact"><div><span>规则模式</span><h2 id="exit-summary-title">默认出口</h2></div><div className="exit-summary-actions"><StatusBadge ok={exitOk} text={exitStatus} /><Button size="sm" variant="outline" onClick={openNodePicker}><ArrowRightLeft size={14} aria-hidden="true" />切换出口</Button></div></div>
            <div className="exit-node">
              <div className="exit-node-icon"><Globe2 size={24} aria-hidden="true" /></div>
              <div><strong>{exitLabel}</strong><small>{exit.node?.subscription === "manual" ? "手动节点" : exit.node?.subscription || (exit.kind === "direct" ? "不走代理" : "规则未命中时的兜底出口")}</small></div>
              {exit.node && <span className={delayClass(exit.node)}>{formatDelay(exit.node)}</span>}
            </div>
            <dl className="exit-facts">
              <div><dt>模式</dt><dd>{modeLabel}</dd></div>
              <div><dt>主端口</dt><dd>{overview.mixed_port}</dd></div>
              <div><dt>候选节点</dt><dd>{aliveCount} 个可用</dd></div>
            </dl>
          </section>
          <TrafficPanel traffic={traffic} />
        </div>

        <OverviewAttention items={attentionItems} onNavigate={onNavigate} />

        <section className="overview-quick-settings" aria-label="快速接管设置">
          <UISwitch checked={overview.system_proxy} label="接管系统代理" onCheckedChange={onSystemProxy} />
          <UISwitch checked={Boolean(overview.tun?.enabled)} label="启用 TUN" onCheckedChange={onTun} />
          <UISwitch checked={Boolean(overview.port_mapping_enabled)} label="节点端口映射" onCheckedChange={onPortMapping} />
          <button type="button" onClick={() => onNavigate("ports")}>查看全部代理入口 <ArrowRight size={15} aria-hidden="true" /></button>
        </section>
      </div>
      {nodePickerOpen && <FixedNodeDialog nodes={overview.nodes} currentNode={exit.selected} canAuto={canAuto} triggerElement={nodePickerTrigger.current} onSelect={onSelectNode} onClose={() => setNodePickerOpen(false)} />}
    </section>
  );
}

/** exitIcon 按默认出口类型返回候选入口图标；exit 为 resolveDefaultExit 结果，返回 lucide 组件。 */
function exitIcon(exit) {
  if (exit.kind === "auto") return Sparkles;
  if (exit.kind === "direct") return Globe2;
  return Target;
}

/**
 * resolveDefaultExit 解析内置 PROXY 组当前的默认出口。
 *
 * 功能说明：
 * 选中值来自内置 PROXY 组的 selected 字段（节点名 / "AUTO" / "DIRECT"）。选中节点
 * 已失效时生成层会忽略该值并回退成员首位，因此这里把有效出口落到首个可用节点并
 * 标记 fallback，避免界面把失效选择误报为真实出口；未持久化选择时同样落成员首位。
 *
 * 参数说明：
 * - overview: object，/api/overview 响应（含 groups 与 nodes）。
 *
 * 返回值说明：
 * 返回 { kind, label, node, fallback, ok, selected }；kind 为 node/auto/direct/default。
 *
 * 可能的异常/错误情况：
 * 缺失字段安全回退为空选择与 null 节点，不抛出异常。
 */
function resolveDefaultExit(overview) {
  const nodes = overview?.nodes || [];
  const alive = nodes.filter((node) => node.alive);
  const group = (overview?.groups || []).find((item) => item.name === "PROXY") || null;
  const selected = group?.selected || "";
  if (selected === "AUTO") {
    const fastest = [...alive].sort((left, right) => (left.delay || Number.POSITIVE_INFINITY) - (right.delay || Number.POSITIVE_INFINITY))[0] || null;
    return { kind: "auto", label: fastest ? `自动最快 · ${fastest.name}` : "自动最快", node: fastest, fallback: false, ok: Boolean(fastest), selected };
  }
  if (selected === "DIRECT") {
    return { kind: "direct", label: "直连（DIRECT）", node: null, fallback: false, ok: true, selected };
  }
  if (selected) {
    const node = alive.find((item) => item.name === selected) || null;
    if (node) return { kind: "node", label: node.name, node, fallback: false, ok: true, selected };
    return { kind: "node", label: selected, node: alive[0] || null, fallback: true, ok: false, selected };
  }
  const first = alive[0] || null;
  return { kind: "default", label: first ? `成员首位 · ${first.name}` : "暂无可用节点", node: first, fallback: false, ok: Boolean(first), selected: "" };
}

/** describeExit 生成默认出口入口的副标题文案；exit 为 resolveDefaultExit 结果，返回 string。 */
function describeExit(exit) {
  if (exit.kind === "auto") return exit.node ? `自动选择延迟最低的节点 · 当前 ${exit.node.name}` : "自动选择延迟最低的节点";
  if (exit.kind === "direct") return "直接连接目标，不经过任何节点";
  if (exit.fallback) return `${exit.selected}（不可用，已回退成员首位）`;
  if (exit.node) return `${exit.node.name} · ${formatDelay(exit.node)}`;
  return "暂无可用节点";
}

/**
 * buildOverviewAttention 汇总需要用户关注但不一定阻断代理的状态。
 *
 * 参数说明：
 * - overview: object，完整概览响应。
 * - traffic: object，实时流量连接状态。
 * - aliveCount: number，健康节点数量。
 * - exit: object，resolveDefaultExit 解析出的默认出口。
 *
 * 返回值说明：
 * 返回 Array<{text: string, view: string}>；数组为空表示没有需要主动提醒的状态。
 *
 * 可能的异常/错误情况：
 * 缺失的可选字段会被忽略；函数只生成展示模型，不修改任何配置。
 */
function buildOverviewAttention(overview, traffic, aliveCount, exit) {
  const items = [];
  if (aliveCount === 0) items.push({ text: "当前没有健康节点，请检查订阅或手动节点。", view: "nodes" });
  if (exit?.fallback) items.push({ text: `默认出口「${exit.selected}」当前不可用，已回退到 PROXY 组成员首位。`, view: "nodes" });
  if (overview.tun?.enabled && !overview.tun?.active) items.push({ text: "TUN 已配置但没有实际生效，请检查权限与运行日志。", view: "logs" });
  if (!overview.dns_custom && overview.tun?.enabled && overview.dns_preset === "off") items.push({ text: "TUN 已开启但 DNS 预设关闭，建议评估 Fake IP。", view: "proxy/settings" });
  if (!overview.port_mapping_enabled) items.push({ text: "节点一对一端口当前未监听，稳定分配仍然保留。", view: "ports" });
  if (traffic.error) items.push({ text: "实时流量暂不可用，控制台正在自动重连。", view: "logs" });
  return items;
}

/**
 * PolicyOption 渲染默认出口的入口按钮。
 *
 * 参数说明：
 * - active: boolean，是否处于强调态。
 * - detail/label: string，出口说明与名称。
 * - icon: React.ComponentType，来自现有图标库的线性图标组件。
 * - tone: string，语义色。
 * - onClick: Function，打开出口选择弹窗的回调。
 *
 * 返回值说明：
 * 返回具有按下状态语义的按钮元素。
 *
 * 可能的异常/错误情况：
 * 回调错误由上层统一处理；组件本身不抛出异常。
 */
function PolicyOption({ active, detail, icon: Icon, label, tone, onClick }) {
  return (
    <button aria-pressed={active} className={classNames("policy-option", active && "active", tone)} role="listitem" type="button" onClick={onClick}>
      <span className="policy-option-icon"><Icon size={20} aria-hidden="true" /></span>
      <span><b>{label}</b><small>{detail}</small></span>
      <span className="policy-option-check" aria-hidden="true">{active ? <CheckCircle2 size={18} /> : <i />}</span>
    </button>
  );
}

/**
 * RouteStep 渲染有效流量路径中的单个步骤。
 *
 * 参数说明：
 * - detail/label: string，步骤当前值与名称。
 * - icon: React.ComponentType，步骤图标。
 * - tone: string，步骤语义色。
 *
 * 返回值说明：
 * 返回只读路径节点。
 *
 * 可能的异常/错误情况：
 * 无；长文本会由 CSS 自动换行或省略。
 */
function RouteStep({ detail, icon: Icon, label, tone }) {
  return <div className={classNames("route-step", tone)}><Icon size={22} aria-hidden="true" /><span>{label}<small>{detail}</small></span></div>;
}

/**
 * OverviewAttention 渲染概览页的可操作提醒区。
 *
 * 参数说明：
 * - items: Array<{text: string, view: string}>，提醒与目标页面。
 * - onNavigate: Function，跳转到处理页面的回调。
 *
 * 返回值说明：
 * 有提醒时返回警告区；没有提醒时返回简洁的正常状态条。
 *
 * 可能的异常/错误情况：
 * 无；未知 view 仍交由上层导航处理。
 */
function OverviewAttention({ items, onNavigate }) {
  if (!items.length) {
    return <section className="overview-attention clear"><CheckCircle2 size={19} aria-hidden="true" /><div><h2>当前无异常</h2><p>主入口、节点健康与实时状态均未发现需要处理的问题。</p></div></section>;
  }
  return (
    <section className="overview-attention">
      <CircleAlert size={20} aria-hidden="true" />
      <div><h2>需要注意</h2><ul>{items.slice(0, 3).map((item) => <li key={item.text}><span>{item.text}</span><button type="button" onClick={() => onNavigate(item.view)}>去处理 <ArrowRight size={14} aria-hidden="true" /></button></li>)}</ul></div>
    </section>
  );
}

/**
 * VersionNotice 在发现新稳定版本时显示轻量下载提示。
 *
 * 参数说明：
 * - status: object，overview.version_check 缓存状态。
 *
 * 返回值说明：
 * 有更新时返回全宽链接提示；其余状态返回 null，避免失败状态干扰代理日常操作。
 *
 * 可能的异常/错误情况：
 * 缺少 URL 或 latest 时不渲染链接；版本检查失败信息仍可在设置页查看。
 */
function VersionNotice({ status }) {
  if (status?.state !== "available" || !status.url || !status.latest) return null;
  return (
    <a className="update-notice" href={status.url} rel="noreferrer" target="_blank">
      <span>发现新版本 <b>{status.latest}</b></span>
      <span>查看 Release <ExternalLink size={15} /></span>
    </a>
  );
}

/**
 * TrafficPanel 渲染实时上下行速率条。
 *
 * 参数说明：
 * - traffic: object，包含 up/down 当前速率与 upTotal/downTotal 累计值。
 *
 * 返回值说明：
 * 返回概览页顶部的实时速率 React 元素。
 *
 * 可能的异常/错误情况：
 * 流量流不可用时展示离线状态；组件不主动发起请求。
 */
function TrafficPanel({ traffic }) {
  const peak = Math.max(traffic.up || 0, traffic.down || 0, 1);
  const upWidth = `${Math.max(4, Math.round(((traffic.up || 0) / peak) * 100))}%`;
  const downWidth = `${Math.max(4, Math.round(((traffic.down || 0) / peak) * 100))}%`;
  const chartPeak = Math.max(
    1,
    ...(traffic.history || []).flatMap((sample) => [sample.up || 0, sample.down || 0]),
  );
  const uploadPath = buildTrafficPath(traffic.history || [], "up", chartPeak);
  const downloadPath = buildTrafficPath(traffic.history || [], "down", chartPeak);
  return (
    <section className="panel traffic-panel full">
      <div>
        <PanelTitle title="实时速率" />
        <StatusBadge ok={traffic.connected} text={traffic.connected ? "已连接" : "离线"} />
      </div>
      <div className="traffic-grid">
        <div className="traffic-row">
          <span>下载</span>
          <b>{formatBytes(traffic.down)}/s</b>
          <i><em style={{ width: downWidth }} /></i>
          <small>累计 {formatBytes(traffic.downTotal)}</small>
        </div>
        <div className="traffic-row">
          <span>上传</span>
          <b>{formatBytes(traffic.up)}/s</b>
          <i><em style={{ width: upWidth }} /></i>
          <small>累计 {formatBytes(traffic.upTotal)}</small>
        </div>
      </div>
      <div className="traffic-chart" aria-label="最近 60 个采样点的上下行速率趋势">
        <svg role="img" viewBox="0 0 600 92" preserveAspectRatio="none">
          <path className="traffic-line download" d={downloadPath} fill="none" pathLength="1" />
          <path className="traffic-line upload" d={uploadPath} fill="none" pathLength="1" />
        </svg>
        <span><i className="download" />下载</span><span><i className="upload" />上传</span>
      </div>
      {traffic.error && <p className="traffic-message"><CircleAlert size={14} aria-hidden="true" />{traffic.error}</p>}
    </section>
  );
}

/**
 * buildTrafficPath 把定长流量采样转换成 SVG 折线路径。
 *
 * 功能说明：
 * 趋势图只用于表达最近一分钟的相对变化，因此以当前窗口峰值归一化，避免 Mbps
 * 与 B/s 跨量级时曲线贴底。单个采样点会复制为水平短线，保证刚打开页面也可见。
 *
 * 参数说明：
 * - samples: Array<object>，包含 up/down 的采样数组。
 * - key: string，要绘制的数值字段，取 `up` 或 `down`。
 * - peak: number，当前窗口归一化峰值，必须大于 0。
 *
 * 返回值说明：
 * 返回合法的 SVG path `d` 字符串。
 *
 * 可能的异常/错误情况：
 * 空数组返回贴近底部的水平线；非法值按 0 处理，不向渲染层抛错。
 */
function buildTrafficPath(samples, key, peak) {
  const points = samples.length ? samples : [{ [key]: 0 }, { [key]: 0 }];
  const divisor = Math.max(points.length - 1, 1);
  return points.map((sample, index) => {
    const x = (index / divisor) * 600;
    const value = Math.max(0, Number(sample[key]) || 0);
    const y = 86 - Math.min(1, value / Math.max(peak, 1)) * 76;
    return `${index === 0 ? "M" : "L"}${x.toFixed(2)} ${y.toFixed(2)}`;
  }).join(" ");
}
