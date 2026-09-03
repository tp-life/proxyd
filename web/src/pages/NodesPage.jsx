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
import { Field } from "@/components/Field";
import { PageHeader } from "@/components/PageHeader";
import { PanelTitle } from "@/components/PanelTitle";
import { StatusBadge } from "@/components/StatusBadge";
import { classNames, delayClass, formatDelay, sortByDelay, tableViewportHeight } from "@/lib/format";

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
   * submitManualNode 新增一个手动节点并在成功后复位对话框。
   *
   * 参数说明：
   * - event: React.FormEvent<HTMLFormElement>，表单提交事件。
   *
   * 返回值说明：
   * 返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 地址为空或重复提交时直接返回；后端错误由 onPost 展示且对话框保持打开。
   */
  async function submitManualNode(event) {
    event.preventDefault();
    const url = forms.manualURL.trim();
    if (!url || adding) return;
    setAdding(true);
    try {
      const name = forms.manualName.trim();
      const saved = await onPost("/api/manual-nodes", name ? { url, name } : { url }, "节点已添加，后台刷新中");
      if (saved) {
        onForm("manualURL", "");
        onForm("manualName", "");
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
        onMainNode={onMainNode}
      />

      {(overview.manual_nodes || []).length > 0 && (
        <section className="panel compact-panel">
          <PanelTitle title="手动节点源" detail="这里管理原始链接；健康状态统一在上方节点表查看。" />
          <ul className="item-list compact-list">
            {overview.manual_nodes.map((node) => (
              <li key={node.index}>
                <b>{node.name || "未命名节点"}</b><code>{node.url}</code>
                <Button aria-label={`删除手动节点 ${node.name || node.index}`} size="icon" variant="destructive-ghost" type="button" onClick={() => onDelete(`/api/manual-nodes/${node.index}`, "节点已删除，刷新中", `手动节点 ${node.name || "未命名节点"}`)}><Trash2 size={16} aria-hidden="true" /></Button>
              </li>
            ))}
          </ul>
        </section>
      )}

      <Dialog open={addOpen} onOpenChange={setAddOpen}>
        <DialogContent>
          <DialogClose className="dialog-close" aria-label="关闭添加节点对话框"><X size={16} aria-hidden="true" /></DialogClose>
          <form onSubmit={submitManualNode}>
            <DialogHeader>
              <DialogTitle>添加手动节点</DialogTitle>
              <DialogDescription>粘贴完整节点链接；名称留空时使用节点自带名称。</DialogDescription>
            </DialogHeader>
            <div className="dialog-form">
              <Field label="节点地址" hint="支持 socks5://、ss:// 等完整链接"><Input autoFocus value={forms.manualURL} placeholder="socks5://user:pass@host:1080" onChange={(event) => onForm("manualURL", event.target.value)} /></Field>
              <Field label="显示名称" hint="可选"><Input value={forms.manualName} placeholder="例如：东京备用" onChange={(event) => onForm("manualName", event.target.value)} /></Field>
            </div>
            <DialogFooter>
              <Button variant="outline" type="button" onClick={() => setAddOpen(false)}>取消</Button>
              <Button disabled={!forms.manualURL.trim()} loading={adding} type="submit">添加节点</Button>
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
 * - onMainNode: Function，设置主端口节点回调。
 *
 * 返回值说明：
 * 返回表格 React 元素。
 *
 * 可能的异常/错误情况：
 * 无；节点不可用时按钮禁用。
 */
function NodeTable({ nodes, assignmentMap = new Map(), mainNode, mappingEnabled = true, onMainNode }) {
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
        cell: (node) => <span className={delayClass(node)}>{formatDelay(node)}</span>,
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
    [assignmentMap, mainNode, mappingEnabled, onMainNode],
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
