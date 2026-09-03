import {
  ArrowRight,
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
import { MODE_LABELS } from "@/lib/constants";
import { classNames, delayClass, formatBytes, formatDelay } from "@/lib/format";

/**
 * MODE_HELP 描述 mihomo 三种运行模式的真实选路语义。
 *
 * 功能说明：
 * 这组说明不仅是界面文案，也是防止误操作的业务边界提示。很多代理软件把
 * “规则模式”误解成整个进程的总开关，但 proxyd 的 mode 只在主入口使用规则分流
 * 策略时参与选路；节点专属端口、自动选优端口和策略分组端口都有独立出口，不读取
 * 这里的模式。把每种模式的执行路径写在界面旁边，可以让用户在切换前理解影响范围。
 *
 * 参数说明：
 * 无；对象 key 与后端 `/api/mode` 接受的 rule/global/direct 枚举保持一致。
 *
 * 返回值说明：
 * 每项包含标题与详细说明，供概览页按当前 mode 展示。
 *
 * 可能的异常/错误情况：
 * 如果后端未来新增 mode 而未同步本对象，界面会使用保守兜底文案，不会阻断切换。
 */
const MODE_HELP = {
  rule: {
    title: "按规则决定出口",
    detail: "依次匹配自定义规则、远程规则源和内置规则；首条命中立即生效，后续规则不再继续判断。",
  },
  global: {
    title: "全部交给代理组",
    detail: "主入口流量跳过访问规则，统一进入 PROXY 选择组，再由当前代理组节点负责转发。",
  },
  direct: {
    title: "全部直接连接",
    detail: "主入口流量跳过访问规则与代理节点，直接访问目标；适合临时排查代理链路问题。",
  },
};

/**
 * OverviewPage 渲染概览页。
 *
 * 参数说明：
 * - overview: object，/api/overview 响应。
 * - aliveCount: number，可用节点数量。
 * - busy/loading: string | boolean，全局后台操作与同步状态。
 * - traffic: object，实时速率流状态。
 * - onCopy/onMenu/onMode/onNavigate/onPalette/onPolicy/onPortMapping/onRefresh/onSystemProxy/onTest/onTun: Function，用户操作回调。
 *
 * 返回值说明：
 * 返回概览页 React 元素。
 *
 * 可能的异常/错误情况：
 * 无；操作失败由父组件 toast。
 */
export function OverviewPage({
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
  onPolicy,
  onPortMapping,
  onRefresh,
  onSystemProxy,
  onTest,
  onTun,
}) {
  const updated = overview.server_time ? new Date(overview.server_time) : new Date();
  const ready = aliveCount > 0 && overview.mixed_port > 0;
  const takenOver = Boolean(overview.system_proxy || overview.tun?.active || overview.tun?.enabled);
  const policy = resolveMainPolicy(overview);
  const activeNode = resolveActiveNode(overview, policy);
  const attentionItems = buildOverviewAttention(overview, traffic, aliveCount);
  /*
   * 这里必须同时区分“配置意图”和“运行时有效策略”：
   * 1. main_auto 优先级最高，它开启时 mode 与 main_node 都不参与主端口选路；
   * 2. main_auto 关闭且 main_node 存在时，用户意图是固定节点；
   * 3. 固定节点暂时失效时，后端为了保持主入口可用会临时恢复顶层 mixed-port，
   *    并继续遵循 mode；它不会删除 main_node，节点恢复后仍可回到原配置；
   * 4. 只有规则策略或上述运行时回退发生时，rule/global/direct 才决定主端口路径。
   *
   * 因此界面保留“固定节点”选中态表达持久化意图，同时在路由和出口处明确标记
   * “已回退”，避免把失效节点误报成真实出口。节点专属端口、自动选优端口与分组
   * 端口拥有独立 listener，不经过此判断，也不会被 mode 切换影响。
   */
  const fallbackToMainMode = policy === "fixed" && !activeNode;
  const usesConfiguredMode = policy === "rule" || fallbackToMainMode;
  const modeHelp = MODE_HELP[overview.mode] || {
    title: "使用后端配置模式",
    detail: `当前后端返回未识别模式“${overview.mode || "未知"}”，界面不会推测其选路行为。`,
  };
  const modeInactive = !usesConfiguredMode;
  const policyLabel = policy === "auto"
    ? "自动最快"
    : policy === "fixed"
      ? fallbackToMainMode ? `固定节点（回退至${MODE_LABELS[overview.mode] || overview.mode}）` : "固定节点"
      : MODE_LABELS[overview.mode] === "规则"
        ? "规则分流"
        : `${MODE_LABELS[overview.mode] || overview.mode}模式`;
  const modeExitLabel = overview.mode === "direct"
    ? "直接连接"
    : overview.mode === "global"
      ? "PROXY 选择组"
      : "按规则动态选择";
  const exitLabel = activeNode?.name || (usesConfiguredMode ? modeExitLabel : "暂无可用节点");

  return (
    <section className="overview-shell">
      <aside className="policy-pane" aria-labelledby="policy-pane-title">
        <header className="policy-pane-header">
          <div><span>主代理入口</span><h1 id="policy-pane-title">主入口策略</h1></div>
          <Button aria-label="打开系统设置" size="icon" variant="outline" type="button" onClick={() => onNavigate("settings")}><Settings size={17} aria-hidden="true" /></Button>
        </header>
        <div className="policy-options" role="list" aria-label="主入口策略选项">
          <PolicyOption active={policy === "rule"} detail={`当前为${MODE_LABELS[overview.mode] || overview.mode}模式`} icon={ListFilter} label="规则分流" tone="blue" onClick={() => onPolicy("rule")} />
          <PolicyOption active={policy === "auto"} detail="自动选择延迟最低节点" icon={Sparkles} label="自动最快" tone="teal" onClick={() => onPolicy("auto")} />
          <PolicyOption active={policy === "fixed"} detail={overview.main_node ? (activeNode?.name || "配置节点当前不可用") : "前往设置选择固定节点"} icon={Target} label="固定节点" tone="indigo" onClick={() => onPolicy("fixed")} />
        </div>
        <section className="policy-mode">
          <PanelTitle title="规则模式" detail="仅在规则分流策略下生效" />
          <SegmentedControl ariaLabel="流量处理模式" className="policy-mode-control" onValueChange={onMode} options={Object.entries(MODE_LABELS).map(([value, label]) => ({ value, label }))} value={overview.mode} />
          <div className={classNames("policy-mode-note", modeInactive && "inactive")}>
            <CircleHelp size={16} aria-hidden="true" />
            <div>
              <strong>{modeHelp.title}</strong>
              <p>{modeHelp.detail}</p>
              <small>
                {modeInactive
                  ? `当前主入口使用“${policy === "auto" ? "自动最快" : "固定节点"}”，该模式已保存但暂不参与主端口选路。`
                  : "仅影响主代理入口；节点端口、自动选优端口和策略分组端口不受影响。"}
              </small>
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
            <span className="overview-mobile-heading"><Button className="mobile-only" size="icon" variant="outline" type="button" onClick={onMenu} aria-label="打开导航"><Menu size={18} aria-hidden="true" /></Button>运行概况</span>
            <h2>{ready ? (takenOver ? "代理已接管" : "代理入口已就绪") : "代理服务需要检查"}</h2>
            <p>{ready ? `当前主入口策略：${policyLabel}` : "当前没有健康节点，请同步订阅或检查节点配置"}</p>
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
            <RouteStep detail={policyLabel} icon={ListFilter} label="匹配策略" tone="indigo" />
            <ArrowRight className="route-arrow" size={20} aria-hidden="true" />
            <RouteStep detail={exitLabel} icon={Globe2} label="实际出口" tone="teal" />
          </div>
          <p className="route-summary">当前流量经由主入口进入 {policyLabel}；{activeNode ? `可确认的出口为“${activeNode.name}”。` : "具体出口会根据命中规则和目标地址动态变化。"}</p>
        </section>

        <div className="overview-lower-grid">
          <section className="exit-summary" aria-labelledby="exit-summary-title">
            <div className="section-heading-row compact"><div><span>主入口</span><h2 id="exit-summary-title">实际出口</h2></div><StatusBadge ok={Boolean(activeNode || usesConfiguredMode)} text={activeNode ? "可用" : usesConfiguredMode ? "由模式决定" : "不可用"} /></div>
            <div className="exit-node">
              <div className="exit-node-icon"><Globe2 size={24} aria-hidden="true" /></div>
              <div><strong>{exitLabel}</strong><small>{activeNode?.subscription === "manual" ? "手动节点" : activeNode?.subscription || "由访问规则决定"}</small></div>
              {activeNode && <span className={delayClass(activeNode)}>{formatDelay(activeNode)}</span>}
            </div>
            <dl className="exit-facts">
              <div><dt>策略</dt><dd>{policyLabel}</dd></div>
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
    </section>
  );
}

/**
 * resolveMainPolicy 计算当前真正生效的主入口策略。
 *
 * 参数说明：
 * - overview: object，包含 main_auto 与 main_node 的概览响应。
 *
 * 返回值说明：
 * 返回 "auto"、"fixed" 或 "rule"，顺序与后端优先级保持一致。
 *
 * 可能的异常/错误情况：
 * overview 字段缺失时安全回退为规则策略，不抛出异常。
 */
function resolveMainPolicy(overview) {
  /*
   * 必须先判断 main_auto。后端将其定义为最高优先级，即使配置文件中还残留
   * main_node，实际主入口也会使用自动测速结果。把 main_node 放在前面会让界面
   * 显示固定节点，却与运行时生成的 mihomo 配置不一致。
   */
  if (overview?.main_auto) return "auto";
  /*
   * main_auto 关闭后，非空 main_node 表示持久化的固定节点意图。此处不检查节点
   * 是否健康，因为健康状态只决定是否运行时回退，不应悄悄改写用户选择。
   */
  if (overview?.main_node) return "fixed";
  /*
   * 两个覆盖字段都未启用时才进入规则策略；此时 rule/global/direct 决定主端口
   * 流量是按规则匹配、统一代理还是全部直连。
   */
  return "rule";
}

/**
 * resolveActiveNode 为可确定出口的策略找到节点模型。
 *
 * 参数说明：
 * - overview: object，包含 nodes、main_node 与 main_node_up 的概览响应。
 * - policy: "rule" | "auto" | "fixed"，已解析的主入口策略。
 *
 * 返回值说明：
 * 固定节点可用时返回对应节点；自动最快返回延迟最低的健康节点；规则策略返回 null。
 *
 * 可能的异常/错误情况：
 * 节点列表为空、固定节点失效或延迟字段非法时返回 null，避免界面虚构实际出口。
 */
function resolveActiveNode(overview, policy) {
  const nodes = (overview?.nodes || []).filter((node) => node.alive);
  if (policy === "fixed") {
    if (!overview?.main_node_up) return null;
    return nodes.find((node) => node.key === overview.main_node) || null;
  }
  if (policy === "auto") {
    return [...nodes].sort((left, right) => (left.delay || Number.POSITIVE_INFINITY) - (right.delay || Number.POSITIVE_INFINITY))[0] || null;
  }
  return null;
}

/**
 * buildOverviewAttention 汇总需要用户关注但不一定阻断代理的状态。
 *
 * 参数说明：
 * - overview: object，完整概览响应。
 * - traffic: object，实时流量连接状态。
 * - aliveCount: number，健康节点数量。
 *
 * 返回值说明：
 * 返回 Array<{text: string, view: string}>；数组为空表示没有需要主动提醒的状态。
 *
 * 可能的异常/错误情况：
 * 缺失的可选字段会被忽略；函数只生成展示模型，不修改任何配置。
 */
function buildOverviewAttention(overview, traffic, aliveCount) {
  const items = [];
  if (aliveCount === 0) items.push({ text: "当前没有健康节点，请检查订阅或手动节点。", view: "nodes" });
  if (overview.main_node && !overview.main_node_up && !overview.main_auto) items.push({ text: `固定节点当前不可用，主入口已经临时回退到${MODE_LABELS[overview.mode] || overview.mode}模式。`, view: "nodes" });
  if (overview.tun?.enabled && !overview.tun?.active) items.push({ text: "TUN 已配置但没有实际生效，请检查权限与运行日志。", view: "logs" });
  if (!overview.dns_custom && overview.tun?.enabled && overview.dns_preset === "off") items.push({ text: "TUN 已开启但 DNS 预设关闭，建议评估 Fake IP。", view: "settings" });
  if (!overview.port_mapping_enabled) items.push({ text: "节点一对一端口当前未监听，稳定分配仍然保留。", view: "ports" });
  if (traffic.error) items.push({ text: "实时流量暂不可用，控制台正在自动重连。", view: "logs" });
  return items;
}

/**
 * PolicyOption 渲染一个可切换的主入口策略。
 *
 * 参数说明：
 * - active: boolean，是否为当前策略。
 * - detail/label: string，策略说明与名称。
 * - icon: React.ComponentType，来自现有图标库的线性图标组件。
 * - tone: string，蓝色、青色或靛蓝语义色。
 * - onClick: Function，切换策略的回调。
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
