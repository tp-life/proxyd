import { useEffect, useMemo, useState } from "react";
import { CircleAlert, Gauge, Plus, Search, Trash2, X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Table } from "@/components/ui/data-table";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { SegmentedControl } from "@/components/ui/segmented-control";
import { Field } from "@/components/Field";
import { PageHeader } from "@/components/PageHeader";
import { PanelTitle } from "@/components/PanelTitle";
import { StatusBadge } from "@/components/StatusBadge";
import { classNames, delayClass, formatDelay, sortByDelay, tableViewportHeight } from "@/lib/format";

/** VPN_TYPE_OPTIONS 是手动节点支持录入的隧道类（VPN）出站类型，与后端白名单一致。 */
const VPN_TYPE_OPTIONS = [
  { value: "tailscale", label: "Tailscale" },
  { value: "openvpn", label: "OpenVPN" },
  { value: "zerotier", label: "ZeroTier" },
  { value: "wireguard", label: "WireGuard" },
  { value: "ssh", label: "SSH" },
];

/** ADVANCED_VPN_TYPES 走通用 JSON 透传的隧道类型（参数较多，面向高级用户）。 */
const ADVANCED_VPN_TYPES = ["zerotier", "wireguard", "ssh"];

/** EMPTY_VPN_FORM 是 VPN 节点表单的初始草稿。 */
const EMPTY_VPN_FORM = {
  type: "tailscale",
  name: "",
  authKey: "",
  controlURL: "",
  hostname: "",
  server: "",
  port: "",
  proto: "udp",
  ca: "",
  cert: "",
  key: "",
  tlsAuth: "",
  username: "",
  password: "",
  json: "",
};

/** VPN_TEXTAREA_CLASS 复用远程模块的等宽多行输入样式，承载证书材料与 JSON。 */
const VPN_TEXTAREA_CLASS = "mono-input min-h-24 w-full rounded-md border bg-background p-2 text-xs";

/**
 * NodesPage 渲染跨来源聚合的节点工作台。
 *
 * 功能说明：
 * 节点与订阅拆分后，本页只承担节点搜索、来源/协议/状态筛选、测速和主节点选择。
 * 添加手动节点使用 Radix Dialog，避免常驻大表单挤压列表首屏。
 *
 * 参数说明：
 * - busy: string，全局后台操作标识；非空时禁用测速按钮，等于“测速”时显示加载态。
 * - forms: object，全局受控表单状态。
 * - initialSource: string，从订阅页跳转时指定的初始来源。
 * - overview: object，概览、节点和稳定端口分配数据。
 * - onDelete/onForm/onMainNode/onPost/onSourceChange/onTest: Function，页面动作回调。
 *
 * 返回值说明：
 * 返回节点工作台 React 元素。
 *
 * 可能的异常/错误情况：
 * 空地址会在前端拦截；后端解析、热更新或持久化失败由 onPost 统一 toast。
 */
export function NodesPage({ busy, forms, initialSource, overview, onDelete, onForm, onMainNode, onPost, onSourceChange, onTest }) {
  const [addOpen, setAddOpen] = useState(false);
  const [adding, setAdding] = useState(false);
  const [addMode, setAddMode] = useState("url");
  const [addError, setAddError] = useState("");
  const [vpn, setVpn] = useState(EMPTY_VPN_FORM);
  const [query, setQuery] = useState("");
  const [source, setSource] = useState(initialSource || "all");
  const [protocol, setProtocol] = useState("all");
  const [status, setStatus] = useState("all");
  const [sort, setSort] = useState("default");

  useEffect(() => {
    setSource(initialSource || "all");
  }, [initialSource]);

  const sourceOptions = useMemo(
    () => ["manual", ...(overview.subscriptions || []).map((subscription) => subscription.name)],
    [overview.subscriptions],
  );
  const protocolOptions = useMemo(
    () => [...new Set((overview.nodes || []).map((node) => node.type).filter(Boolean))].sort(),
    [overview.nodes],
  );
  const assignmentMap = useMemo(
    () => new Map((overview.port_assignments || []).map((entry) => [`${entry.subscription}:${entry.node}`, entry.port])),
    [overview.port_assignments],
  );
  // 手动触发后 busy 立即为「测速」；后台检测期间由 overview.testing 接力，
  // 避免延迟列在新结果出来前继续展示容易被误读为已完成的上轮延迟。
  const testing = busy === "测速" || Boolean(overview.testing);
  const visibleNodes = useMemo(() => {
    const normalizedQuery = query.trim().toLowerCase();
    const filtered = (overview.nodes || []).filter((node) => {
      if (source !== "all" && node.subscription !== source) return false;
      if (protocol !== "all" && node.type !== protocol) return false;
      if (status === "alive" && !node.alive) return false;
      if (status === "failed" && node.alive) return false;
      if (normalizedQuery && !`${node.name} ${node.subscription} ${node.type}`.toLowerCase().includes(normalizedQuery)) return false;
      return true;
    });
    return sortByDelay(filtered, sort);
  }, [overview.nodes, protocol, query, sort, source, status]);

  /**
   * updateVPN 更新 VPN 节点表单的单个字段。
   *
   * 参数说明：
   * - key: string，EMPTY_VPN_FORM 中的字段名。
   * - value: string，输入值。
   *
   * 返回值说明：无。
   *
   * 可能的异常/错误情况：无；字段合法性在提交时统一校验。
   */
  function updateVPN(key, value) {
    setVpn((current) => ({ ...current, [key]: value }));
    setAddError("");
  }

  /**
   * buildVPNProxy 把 VPN 表单草稿组装成 mihomo 出站映射。
   *
   * 参数说明：无；读取当前 vpn 状态。
   *
   * 返回值说明：
   * 返回 `{ proxy }` 或 `{ error }`；proxy 的键与后端 POST /api/manual-nodes 的
   * proxy 字段一致（name/type/auth-key/server/port/ca 等），空可选字段不提交。
   *
   * 可能的异常/错误情况：
   * 必填项缺失或 JSON 解析失败时返回 error 文案，由对话框内联展示。
   */
  function buildVPNProxy() {
    const name = vpn.name.trim();
    if (!name) return { error: "请填写节点名称" };
    if (vpn.type === "tailscale") {
      if (!vpn.authKey.trim()) return { error: "Tailscale 节点必须提供 auth-key" };
      const proxy = { name, type: "tailscale", "auth-key": vpn.authKey.trim() };
      if (vpn.controlURL.trim()) proxy["control-url"] = vpn.controlURL.trim();
      if (vpn.hostname.trim()) proxy.hostname = vpn.hostname.trim();
      return { proxy };
    }
    if (vpn.type === "openvpn") {
      const port = Number.parseInt(vpn.port, 10);
      if (!vpn.server.trim()) return { error: "OpenVPN 节点必须填写服务器地址" };
      if (!port || port <= 0 || port > 65535) return { error: "OpenVPN 节点必须填写有效端口（1-65535）" };
      if (!vpn.ca.trim()) return { error: "OpenVPN 节点必须提供 CA 证书内容" };
      const proxy = { name, type: "openvpn", server: vpn.server.trim(), port, ca: vpn.ca };
      if (vpn.proto) proxy.proto = vpn.proto;
      if (vpn.cert.trim()) proxy.cert = vpn.cert;
      if (vpn.key.trim()) proxy.key = vpn.key;
      if (vpn.tlsAuth.trim()) proxy["tls-auth"] = vpn.tlsAuth;
      if (vpn.username.trim()) proxy.username = vpn.username.trim();
      if (vpn.password) proxy.password = vpn.password;
      return { proxy };
    }
    let extra;
    try {
      extra = JSON.parse(vpn.json);
    } catch {
      return { error: "出站 JSON 解析失败，请检查格式" };
    }
    if (!extra || typeof extra !== "object" || Array.isArray(extra)) return { error: "出站 JSON 必须是对象" };
    return { proxy: { ...extra, name, type: vpn.type } };
  }

  /**
   * submitManualNode 新增一个手动节点并在成功后复位对话框。
   *
   * 功能说明：
   * 链接模式提交 `{url, name?}`；VPN 模式提交 `{proxy}`（结构化隧道出站映射），
   * 两者对应后端 POST /api/manual-nodes 的二选一入口。
   *
   * 参数说明：
   * - event: React.FormEvent<HTMLFormElement>，表单提交事件。
   *
   * 返回值说明：
   * 返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 地址为空或重复提交时直接返回；VPN 表单校验失败在对话框内联提示；
   * 后端错误由 onPost 展示且对话框保持打开。
   */
  async function submitManualNode(event) {
    event.preventDefault();
    if (adding) return;
    let body;
    if (addMode === "vpn") {
      const { proxy, error } = buildVPNProxy();
      if (error) {
        setAddError(error);
        return;
      }
      body = { proxy };
    } else {
      const url = forms.manualURL.trim();
      if (!url) return;
      const name = forms.manualName.trim();
      body = name ? { url, name } : { url };
    }
    setAdding(true);
    try {
      const saved = await onPost("/api/manual-nodes", body, "节点已添加，后台刷新中");
      if (saved) {
        onForm("manualURL", "");
        onForm("manualName", "");
        setVpn(EMPTY_VPN_FORM);
        setAddMode("url");
        setAddError("");
        setAddOpen(false);
      }
    } finally {
      setAdding(false);
    }
  }

  /**
   * changeSource 同步本页与 App 的来源筛选。
   *
   * 参数说明：
   * - nextSource: string，目标来源名或 `all`。
   *
   * 返回值说明：无。
   *
   * 可能的异常/错误情况：无；不存在的来源只会得到空列表。
   */
  function changeSource(nextSource) {
    setSource(nextSource);
    onSourceChange(nextSource);
  }

  return (
    <div className="stack">
      <PageHeader eyebrow="代理资源" title="代理节点" detail="跨订阅查看健康状态、延迟与稳定端口分配。">
        <div className="page-actions">
          <Button disabled={Boolean(busy)} loading={busy === "测速"} variant="outline" type="button" onClick={onTest}>{busy !== "测速" && <Gauge size={16} aria-hidden="true" />}{busy === "测速" ? "测速中…" : "测试全部"}</Button>
          <Button type="button" onClick={() => setAddOpen(true)}><Plus size={16} aria-hidden="true" />添加节点</Button>
        </div>
      </PageHeader>

      <section className="panel compact-panel">
        <div className="filter-grid node-filters">
          <Field compact label="搜索节点">
            <div className="input-with-icon"><Search size={15} aria-hidden="true" /><Input value={query} placeholder="名称 / 来源 / 协议" onChange={(event) => setQuery(event.target.value)} /></div>
          </Field>
          <Field compact label="来源">
            <Select
              ariaLabel="筛选节点来源"
              value={source}
              onValueChange={changeSource}
              options={[
                { value: "all", label: "全部来源" },
                ...sourceOptions.map((name) => ({ value: name, label: name === "manual" ? "手动节点" : name })),
              ]}
            />
          </Field>
          <Field compact label="协议">
            <Select
              ariaLabel="筛选节点协议"
              value={protocol}
              onValueChange={setProtocol}
              options={[
                { value: "all", label: "全部协议" },
                ...protocolOptions.map((name) => ({ value: name, label: name.toUpperCase() })),
              ]}
            />
          </Field>
          <Field compact label="状态">
            <Select
              ariaLabel="筛选节点状态"
              value={status}
              onValueChange={setStatus}
              options={[{ value: "all", label: "全部状态" }, { value: "alive", label: "可用" }, { value: "failed", label: "异常" }]}
            />
          </Field>
          <Field compact label="排序">
            <Select
              ariaLabel="节点排序方式"
              value={sort}
              onValueChange={setSort}
              options={[{ value: "default", label: "默认顺序" }, { value: "asc", label: "延迟升序" }, { value: "desc", label: "延迟降序" }]}
            />
          </Field>
        </div>
      </section>

      {!overview.port_mapping_enabled && (
        <div className="notice-row"><CircleAlert size={16} aria-hidden="true" /><span>节点端口映射已关闭。稳定分配仍保留，但这些端口当前不监听。</span></div>
      )}
      <NodeTable
        assignmentMap={assignmentMap}
        mainNode={overview.main_node}
        mappingEnabled={overview.port_mapping_enabled}
        nodes={visibleNodes}
        testing={testing}
        onMainNode={onMainNode}
      />

      {(overview.manual_nodes || []).length > 0 && (
        <section className="panel compact-panel">
          <PanelTitle title="手动节点源" detail="这里管理原始链接与 VPN 隧道出站；健康状态统一在上方节点表查看。" />
          <ul className="item-list compact-list">
            {overview.manual_nodes.map((node) => (
              <li key={node.index}>
                <b>{node.name || "未命名节点"}</b>
                {node.type ? (
                  <>
                    <Badge variant="secondary">隧道</Badge>
                    <code>{manualProxySummary(node)}</code>
                  </>
                ) : (
                  <code>{node.url}</code>
                )}
                <Button aria-label={`删除手动节点 ${node.name || node.index}`} size="icon" variant="destructive-ghost" type="button" onClick={() => onDelete(`/api/manual-nodes/${node.index}`, "节点已删除，刷新中", `手动节点 ${node.name || "未命名节点"}`)}><Trash2 size={16} aria-hidden="true" /></Button>
              </li>
            ))}
          </ul>
        </section>
      )}

      <Dialog open={addOpen} onOpenChange={(open) => { setAddOpen(open); if (!open) setAddError(""); }}>
        <DialogContent>
          <DialogClose className="dialog-close" aria-label="关闭添加节点对话框"><X size={16} aria-hidden="true" /></DialogClose>
          <form onSubmit={submitManualNode}>
            <DialogHeader>
              <DialogTitle>添加手动节点</DialogTitle>
              <DialogDescription>
                {addMode === "vpn"
                  ? "填写隧道类（VPN）出站参数。隧道节点不分配独立端口，经策略分组出口使用。"
                  : "粘贴完整节点链接；名称留空时使用节点自带名称。"}
              </DialogDescription>
            </DialogHeader>
            <div className="dialog-form">
              <SegmentedControl
                ariaLabel="节点录入方式"
                value={addMode}
                onValueChange={setAddMode}
                options={[{ value: "url", label: "链接" }, { value: "vpn", label: "VPN 节点" }]}
              />
              {addMode === "url" ? (
                <>
                  <Field label="节点地址" hint="支持 socks5://、ss:// 等完整链接"><Input autoFocus value={forms.manualURL} placeholder="socks5://user:pass@host:1080" onChange={(event) => onForm("manualURL", event.target.value)} /></Field>
                  <Field label="显示名称" hint="可选"><Input value={forms.manualName} placeholder="例如：东京备用" onChange={(event) => onForm("manualName", event.target.value)} /></Field>
                </>
              ) : (
                <>
                  <Field label="隧道类型">
                    <Select
                      ariaLabel="VPN 节点隧道类型"
                      value={vpn.type}
                      onValueChange={(value) => updateVPN("type", value)}
                      options={VPN_TYPE_OPTIONS}
                    />
                  </Field>
                  <Field label="节点名称" hint="必填，作为分组出口选项显示"><Input value={vpn.name} placeholder="例如：ts-exit" onChange={(event) => updateVPN("name", event.target.value)} /></Field>
                  {vpn.type === "tailscale" && (
                    <>
                      <Field label="Auth Key" hint="必填，tskey- 开头的认证密钥"><Input autoComplete="off" type="password" value={vpn.authKey} placeholder="tskey-…" onChange={(event) => updateVPN("authKey", event.target.value)} /></Field>
                      <Field label="控制服务地址" hint="可选，自建 Headscale 时填写"><Input value={vpn.controlURL} placeholder="https://headscale.example.com" onChange={(event) => updateVPN("controlURL", event.target.value)} /></Field>
                      <Field label="主机名" hint="可选，在 Tailnet 中显示的名称"><Input value={vpn.hostname} placeholder="proxyd-exit" onChange={(event) => updateVPN("hostname", event.target.value)} /></Field>
                    </>
                  )}
                  {vpn.type === "openvpn" && (
                    <>
                      <Field label="服务器地址" hint="必填"><Input value={vpn.server} placeholder="vpn.example.com" onChange={(event) => updateVPN("server", event.target.value)} /></Field>
                      <div className="vpn-field-row">
                        <Field label="端口" hint="必填"><Input min="1" max="65535" type="number" value={vpn.port} placeholder="1194" onChange={(event) => updateVPN("port", event.target.value)} /></Field>
                        <Field label="传输协议">
                          <Select
                            ariaLabel="OpenVPN 传输协议"
                            value={vpn.proto}
                            onValueChange={(value) => updateVPN("proto", value)}
                            options={[{ value: "udp", label: "UDP" }, { value: "tcp", label: "TCP" }]}
                          />
                        </Field>
                      </div>
                      <Field label="CA 证书" hint="必填，粘贴 PEM 全文"><textarea aria-label="OpenVPN CA 证书" className={VPN_TEXTAREA_CLASS} value={vpn.ca} placeholder="-----BEGIN CERTIFICATE-----…" onChange={(event) => updateVPN("ca", event.target.value)} /></Field>
                      <Field label="客户端证书" hint="可选，PEM 全文"><textarea aria-label="OpenVPN 客户端证书" className={VPN_TEXTAREA_CLASS} value={vpn.cert} onChange={(event) => updateVPN("cert", event.target.value)} /></Field>
                      <Field label="客户端私钥" hint="可选，PEM 全文；提交后仅服务端保存"><textarea aria-label="OpenVPN 客户端私钥" className={VPN_TEXTAREA_CLASS} value={vpn.key} onChange={(event) => updateVPN("key", event.target.value)} /></Field>
                      <Field label="TLS-Auth 密钥" hint="可选"><textarea aria-label="OpenVPN TLS-Auth 密钥" className={VPN_TEXTAREA_CLASS} value={vpn.tlsAuth} onChange={(event) => updateVPN("tlsAuth", event.target.value)} /></Field>
                      <div className="vpn-field-row">
                        <Field label="用户名" hint="可选，auth-user-pass 认证"><Input autoComplete="off" value={vpn.username} onChange={(event) => updateVPN("username", event.target.value)} /></Field>
                        <Field label="密码" hint="可选"><Input autoComplete="off" type="password" value={vpn.password} onChange={(event) => updateVPN("password", event.target.value)} /></Field>
                      </div>
                    </>
                  )}
                  {ADVANCED_VPN_TYPES.includes(vpn.type) && (
                    <Field label="出站参数" hint="JSON 对象，字段原样透传给 mihomo；name 与 type 由上方表单生成">
                      <textarea
                        aria-label={`${vpn.type} 出站参数 JSON`}
                        className={VPN_TEXTAREA_CLASS}
                        value={vpn.json}
                        placeholder={vpn.type === "wireguard" ? '{"server":"…","port":51820,"private-key":"…","public-key":"…"}' : "{}"}
                        onChange={(event) => updateVPN("json", event.target.value)}
                      />
                    </Field>
                  )}
                  {addError && <p className="dialog-error" role="alert">{addError}</p>}
                </>
              )}
            </div>
            <DialogFooter>
              <Button variant="outline" type="button" onClick={() => setAddOpen(false)}>取消</Button>
              <Button disabled={addMode === "url" ? !forms.manualURL.trim() : !vpn.name.trim()} loading={adding} type="submit">添加节点</Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </div>
  );
}

/**
 * NodeTable 渲染节点列表表格。
 *
 * 参数说明：
 * - nodes: Array<object>，节点数据。
 * - assignmentMap: Map<string, number>，按来源与节点名索引的稳定端口分配。
 * - mainNode: string，当前主端口固定节点 key。
 * - mappingEnabled: boolean，节点一对一 listener 是否启用。
 * - testing: boolean，节点健康检测进行中；延迟列显示「测速中…」而不是上一轮结果。
 * - onMainNode: Function，设置主端口节点回调。
 *
 * 返回值说明：
 * 返回表格 React 元素。
 *
 * 可能的异常/错误情况：
 * 无；节点不可用时按钮禁用。
 */
function NodeTable({ nodes, assignmentMap = new Map(), mainNode, mappingEnabled = true, testing = false, onMainNode }) {
  const columns = useMemo(
    () => [
      {
        key: "name",
        header: "节点",
        sortable: true,
        width: "34%",
        cell: (node) => (
          <span className={classNames("node-name", !node.alive && "text-muted-foreground")} title={node.name}>
            {node.name}
            {node.tunnel && <Badge variant="secondary">隧道</Badge>}
            {mainNode && node.key === mainNode && <Badge variant="outline">主端口</Badge>}
          </span>
        ),
      },
      { key: "type", header: "类型", sortable: true, width: "100px", cell: (node) => node.type || "-" },
      { key: "subscription", header: "来源", sortable: true, width: "18%", cell: (node) => node.subscription === "manual" ? "手动节点" : node.subscription },
      {
        key: "port",
        header: "稳定端口",
        sortable: true,
        width: "118px",
        cell: (node) => {
          // 隧道节点不参与端口映射（port 恒为 0）是正常状态，展示占位符而非缺失。
          if (node.tunnel) return <span className="text-muted-foreground" title="隧道节点经分组出口使用，不分配独立端口">—</span>;
          const assignedPort = node.port || assignmentMap.get(`${node.subscription}:${node.name}`);
          return assignedPort ? <span className={classNames(!mappingEnabled && "text-muted-foreground")}>{assignedPort}{!mappingEnabled && <small className="block">未监听</small>}</span> : "-";
        },
        sortValue: (node) => node.port || assignmentMap.get(`${node.subscription}:${node.name}`) || Number.POSITIVE_INFINITY,
      },
      {
        key: "delay",
        header: "延迟",
        sortable: true,
        width: "105px",
        cell: (node) => testing
          ? <span className="delay-muted">测速中…</span>
          : <span className={delayClass(node)}>{formatDelay(node)}</span>,
        sortValue: (node) => node.alive && node.delay > 0 ? node.delay : Number.POSITIVE_INFINITY,
      },
      {
        key: "alive",
        header: "状态",
        sortable: true,
        width: "110px",
        cell: (node) => <StatusBadge ok={node.alive} text={node.alive ? "可用" : "失效"} />,
        sortValue: (node) => node.alive ? 1 : 0,
      },
      {
        key: "action",
        header: "操作",
        width: "156px",
        cell: (node) => (
          <Button size="sm" variant="outline" disabled={!node.alive || mainNode === node.key} type="button" onClick={() => onMainNode(node.key)}>
            {mainNode === node.key ? "当前主端口" : "设为主端口"}
          </Button>
        ),
      },
    ],
    [assignmentMap, mainNode, mappingEnabled, onMainNode, testing],
  );

  return (
    <Table
      className="data-table"
      columns={columns}
      data={nodes}
      emptyState="暂无节点"
      getRowClassName={(node) => (mainNode && node.key === mainNode ? "main-node-row" : "")}
      getRowId={(node, index) => node.key || `${node.subscription}:${node.name}:${index}`}
      height={tableViewportHeight(nodes.length, 880)}
      minColumnWidth={84}
      resizable
    />
  );
}

/**
 * manualProxySummary 为结构化（VPN）手动节点生成紧凑摘要。
 *
 * 参数说明：
 * - entry: object，overview.manual_nodes 条目，proxy 字段的凭据已被后端打码。
 *
 * 返回值说明：
 * 返回「类型 · 目标」文本；取不到地址时只显示类型，不泄露凭据字段。
 *
 * 可能的异常/错误情况：
 * 无；缺失字段按空串处理。
 */
function manualProxySummary(entry) {
  const proxy = entry.proxy || {};
  const target = proxy.server
    ? `${proxy.server}${proxy.port ? `:${proxy.port}` : ""}`
    : proxy["control-url"] || "";
  return target ? `${entry.type} · ${target}` : `${entry.type} 隧道出站`;
}
