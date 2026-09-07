/** 固定节点选择弹窗：仅维护候选选择，配置写入与状态刷新交给应用入口。 */
import { useRef, useState } from "react";
import { CheckCircle2, Globe2, Search } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Select } from "@/components/ui/select";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "@/components/ui/dialog";
import { classNames, delayClass, formatDelay } from "@/lib/format";

/**
 * FixedNodeDialog 以关键词和来源联合筛选卡片，选择主端口固定节点，确认成功后关闭。
 * 参数：nodes 为 Array<object> 节点快照；currentNode 为 string 当前固定节点 key；
 * active 为 boolean，当前是否已使用固定策略；triggerElement 为 HTMLElement 打开入口；
 * onSelect 为 (key: string) => Promise<boolean> 保存回调；onClose 为 () => void 关闭回调。
 * 返回：React 弹窗元素。错误：保存失败保留选择并提示重试；消失或失效的节点禁止提交。
 */
export function FixedNodeDialog({ nodes, currentNode, active, triggerElement, onSelect, onClose }) {
  const [query, setQuery] = useState("");
  const [source, setSource] = useState("");
  const [selected, setSelected] = useState(currentNode || "");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const submitting = useRef(false);
  const candidate = nodes.find((node) => node.key === selected && node.alive);
  const unchanged = Boolean(candidate) && active && selected === currentNode;
  const keyword = query.trim().toLocaleLowerCase();
  /*
   * 来源直接取节点快照，手动节点使用后端的 manual 标记；订阅值增加前缀，避免订阅名称
   * 与“全部订阅”等筛选值冲突。轮询后来源消失时保留回显，用户仍可切回全部来源。
   */
  const subscriptions = [...new Set(nodes.map((node) => node.subscription).filter((name) => name && name !== "manual"))].sort((left, right) => left.localeCompare(right));
  const sourceOptions = [
    { value: "", label: "全部来源" },
    { value: "subscriptions", label: "全部订阅" },
    { value: "manual", label: "手动节点" },
    ...subscriptions.map((name) => ({ value: `subscription:${name}`, label: `订阅：${name}` })),
  ];
  if (source && !sourceOptions.some((option) => option.value === source)) {
    sourceOptions.push({ value: source, label: `订阅：${source.slice("subscription:".length)}（暂无节点）`, disabled: true });
  }

  /** matchesSource 判断节点是否符合来源筛选；node 为 object 节点快照，返回 boolean；未知来源仅在“全部来源”显示，无异常。 */
  function matchesSource(node) {
    if (!source) return true;
    if (source === "manual") return node.subscription === "manual";
    if (source === "subscriptions") return Boolean(node.subscription) && node.subscription !== "manual";
    return source === `subscription:${node.subscription}`;
  }

  /*
   * 保留失效节点供用户理解当前状态，但把可用节点放在前面；使用稳定 key 保存选择，
   * 避免同名节点或轮询排序变化选错出口。关键词与来源同时满足才显示，筛选不清除已选候选。
   */
  const visibleNodes = nodes.filter((node) => matchesSource(node) && `${node.name} ${node.subscription === "manual" ? "手动节点" : node.subscription} ${node.type}`.toLocaleLowerCase().includes(keyword))
    .sort((left, right) => Number(right.alive) - Number(left.alive)
      || Number(right.key === currentNode) - Number(left.key === currentNode)
      || (left.delay || Infinity) - (right.delay || Infinity)
      || left.name.localeCompare(right.name));

  /** changeOpen 处理关闭请求；open 为 boolean，返回 void；保存中忽略关闭，避免重复提交或丢失失败反馈。 */
  function changeOpen(open) {
    if (!open && !submitting.current) onClose();
  }

  /** restoreFocus 归还焦点到打开入口；event 为 Event，返回 void；入口已卸载时不操作 DOM。 */
  function restoreFocus(event) {
    if (!triggerElement?.isConnected) return;
    event.preventDefault();
    triggerElement.focus();
  }

  /**
   * confirmSelection 提交仍然健康的候选；无参数，返回 Promise<void>。
   * 错误：回调返回 false 或抛出异常均保留弹窗；同步锁在 React 重绘前阻止连点并发写入。
   */
  async function confirmSelection() {
    if (submitting.current || !candidate || unchanged) return;
    submitting.current = true;
    setSaving(true);
    setError("");
    try {
      if (await onSelect(candidate.key)) onClose();
      else setError("切换未完成，请检查节点状态后重试。");
    } catch {
      setError("切换失败，请稍后重试。");
    } finally {
      submitting.current = false;
      setSaving(false);
    }
  }

  return <Dialog open onOpenChange={changeOpen}>
    <DialogContent className="fixed-node-dialog" showClose={!saving} onCloseAutoFocus={restoreFocus} aria-busy={saving}>
      <header className="dialog-header">
        <DialogTitle>选择固定节点</DialogTitle>
        <DialogDescription>选择一个可用节点作为主入口出口，确认后立即生效。</DialogDescription>
      </header>
      <div className="fixed-node-filters">
        <label className="fixed-node-search"><Search size={17} aria-hidden="true" /><input aria-label="搜索节点" placeholder="搜索节点名称、来源或协议" value={query} onChange={(event) => setQuery(event.target.value)} /></label>
        <Select ariaLabel="节点来源" value={source} onValueChange={setSource} options={sourceOptions} />
      </div>
      <div className="fixed-node-list" aria-label="候选节点">
        {visibleNodes.map((node) => <button key={node.key} type="button" className={classNames("fixed-node-card", selected === node.key && "selected")} aria-pressed={selected === node.key} disabled={saving || !node.alive} onClick={() => { setSelected(node.key); setError(""); }}>
          <span className="fixed-node-card-heading"><Globe2 size={18} aria-hidden="true" /><strong title={node.name}>{node.name}</strong>{selected === node.key && <CheckCircle2 size={18} aria-hidden="true" />}</span>
          <span className="fixed-node-source">{node.subscription === "manual" ? "手动节点" : node.subscription || "未知来源"} · {node.type?.toUpperCase() || "未知协议"}</span>
          <span className="fixed-node-card-status"><span className={delayClass(node)}>{node.alive ? formatDelay(node) : "不可用"}</span>{node.key === currentNode && <Badge variant="outline">{active ? "当前固定" : "已配置"}</Badge>}</span>
        </button>)}
        {!visibleNodes.length && <p className="fixed-node-empty">{nodes.length ? "没有匹配的节点，请调整关键词或来源筛选。" : "暂无节点，请先添加节点或同步订阅。"}</p>}
      </div>
      <div className="fixed-node-feedback" aria-live="polite">
        {error ? <p role="alert">{error}</p> : <p>{candidate ? `已选择：${candidate.name}` : "请选择可用节点；失效节点暂不可选。"}</p>}
      </div>
      <footer className="dialog-footer">
        <Button variant="outline" disabled={saving} onClick={() => changeOpen(false)}>取消</Button>
        <Button disabled={!candidate || unchanged} loading={saving} onClick={confirmSelection}>{saving ? "切换中…" : unchanged ? "当前已使用" : "确认切换"}</Button>
      </footer>
    </DialogContent>
  </Dialog>;
}
