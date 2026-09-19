/** 出口选择弹窗：仅维护候选选择，配置写入与状态刷新交给应用入口。 */
import { useRef, useState } from "react";
import { CheckCircle2, Globe2, Search, Sparkles } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Select } from "@/components/ui/select";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "@/components/ui/dialog";
import { classNames, delayClass, formatDelay } from "@/lib/format";

/**
 * EXIT_SPECIALS 是内置 PROXY 组的两个保留出口选项，与节点名一起构成默认出口候选。
 * requiresAlive 表示该保留出口依赖至少一个可用节点（AUTO 测速组只在有健康节点时生成）。
 */
export const EXIT_SPECIALS = [
  { value: "AUTO", label: "自动最快", detail: "由 AUTO 测速组选择当前延迟最低的节点", badge: "自动选择", icon: Sparkles, requiresAlive: true },
  { value: "DIRECT", label: "直连", detail: "不经过任何节点，直接访问目标", badge: "不走代理", icon: Globe2 },
];

/**
 * FixedNodeDialog 以关键词和来源联合筛选，选择一个策略出口（节点名或保留出口）。
 *
 * 功能说明：
 * 出口取值是节点名或保留名 AUTO / DIRECT（不是节点 key），因此候选与当前值都按
 * 节点名匹配。默认出口由内置 PROXY 组的持久化选中项表达；策略分组页则把候选裁剪为
 * 该分组当前存活的成员，并通过 specials 决定是否提供 AUTO / DIRECT。确认成功后
 * 弹窗关闭，失败保留选择并提示重试。
 *
 * 参数说明：
 * - nodes: Array<object> 候选节点快照；调用方按场景裁剪（如仅传某分组的存活成员）。
 * - currentNode: string 当前出口值（节点名 / AUTO / DIRECT），空串表示未选择。
 * - canAuto: boolean，是否存在可用节点（决定依赖存活节点的保留出口是否可选）。
 * - specials: Array<{value: string, label: string, detail: string, badge: string, icon: Function, requiresAlive?: boolean}>
 *   保留出口候选；默认使用内置 PROXY 的 AUTO / DIRECT，传空数组即纯节点选择。
 * - title/description/currentBadge/invalidHint: string，按场景覆盖的标题、说明、当前值徽标与无效选择提示。
 * - triggerElement: HTMLElement 打开入口，用于归还焦点。
 * - onSelect: (value: string) => Promise<boolean> 保存回调。
 * - onClose: () => void 关闭回调。
 *
 * 返回值说明：
 * 返回 React 弹窗元素。
 *
 * 可能的异常/错误情况：保存失败保留选择并提示重试；失效或消失的节点禁止提交。
 */
export function FixedNodeDialog({
  nodes,
  currentNode,
  canAuto = true,
  specials = EXIT_SPECIALS,
  title = "选择默认出口",
  description = "规则模式未命中规则的流量出口；TUN 与网关流量同样经过它。",
  currentBadge = "当前默认出口",
  invalidHint = "请选择可用节点、AUTO 或 DIRECT。",
  triggerElement,
  onSelect,
  onClose,
}) {
  const [query, setQuery] = useState("");
  const [source, setSource] = useState("");
  const [selected, setSelected] = useState(currentNode || "");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const submitting = useRef(false);
  /*
   * 保留出口是否可选：AUTO 依赖可用节点（requiresAlive），DIRECT 之类恒可选；
   * 节点候选则要求存活。
   */
  const specialSelectable = (option) => !option.requiresAlive || canAuto;
  const candidateSpecial = specials.find((option) => option.value === selected) || null;
  const candidateNode = nodes.find((node) => node.name === selected && node.alive) || null;
  const candidateLabel = candidateSpecial?.label || candidateNode?.name || "";
  const candidateValid = Boolean(candidateNode) || Boolean(candidateSpecial && specialSelectable(candidateSpecial));
  const unchanged = candidateValid && selected === currentNode;
  const keyword = query.trim().toLocaleLowerCase();
  /*
   * 来源筛选针对节点来源，保留选项（AUTO / DIRECT）不属于任何订阅，只在“全部来源”
   * 下展示；关键词命中标签或说明时同样保留。轮询后来源消失时保留回显，用户仍可切回。
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
  const visibleSpecials = source
    ? []
    : specials.filter((option) => `${option.label} ${option.detail}`.toLocaleLowerCase().includes(keyword));

  /** matchesSource 判断节点是否符合来源筛选；node 为 object 节点快照，返回 boolean；未知来源仅在“全部来源”显示，无异常。 */
  function matchesSource(node) {
    if (!source) return true;
    if (source === "manual") return node.subscription === "manual";
    if (source === "subscriptions") return Boolean(node.subscription) && node.subscription !== "manual";
    return source === `subscription:${node.subscription}`;
  }

  /*
   * 保留失效节点供用户理解当前状态，但把可用节点放在前面；选择按节点名保存，与
   * groupstate 的存储口径一致（mihomo select 组本身也按名字引用成员）。
   */
  const visibleNodes = nodes.filter((node) => matchesSource(node) && `${node.name} ${node.subscription === "manual" ? "手动节点" : node.subscription} ${node.type}`.toLocaleLowerCase().includes(keyword))
    .sort((left, right) => Number(right.alive) - Number(left.alive)
      || Number(right.name === currentNode) - Number(left.name === currentNode)
      || (left.delay || Infinity) - (right.delay || Infinity)
      || left.name.localeCompare(right.name));

  /* 空态文案按候选来源区分：来源筛选、关键词无命中、以及“完全没有候选”三种情形。 */
  const emptyText = source
    ? "没有匹配的节点，请调整关键词或来源筛选。"
    : nodes.length
      ? "没有匹配的出口，请调整关键词。"
      : specials.length
        ? "暂无节点，请先添加节点或同步订阅；也可以直接选择 AUTO 或 DIRECT。"
        : "该分组暂无可用成员节点。";

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
   * confirmSelection 提交仍然有效的候选；无参数，返回 Promise<void>。
   * 错误：回调返回 false 或抛出异常均保留弹窗；同步锁在 React 重绘前阻止连点并发写入。
   */
  async function confirmSelection() {
    if (submitting.current || !candidateValid || unchanged) return;
    submitting.current = true;
    setSaving(true);
    setError("");
    try {
      if (await onSelect(selected)) onClose();
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
        <DialogTitle>{title}</DialogTitle>
        <DialogDescription>{description}</DialogDescription>
      </header>
      <div className="fixed-node-filters">
        <label className="fixed-node-search"><Search size={17} aria-hidden="true" /><input aria-label="搜索出口" placeholder="搜索节点名称、来源或协议" value={query} onChange={(event) => setQuery(event.target.value)} /></label>
        <Select ariaLabel="节点来源" value={source} onValueChange={setSource} options={sourceOptions} />
      </div>
      <div className="fixed-node-list" aria-label="候选出口">
        {visibleSpecials.map((option) => {
          const Icon = option.icon;
          const disabled = saving || !specialSelectable(option);
          return <button key={option.value} type="button" className={classNames("fixed-node-card", selected === option.value && "selected")} aria-pressed={selected === option.value} disabled={disabled} onClick={() => { setSelected(option.value); setError(""); }}>
            <span className="fixed-node-card-heading"><Icon size={18} aria-hidden="true" /><strong>{option.label}</strong>{selected === option.value && <CheckCircle2 size={18} aria-hidden="true" />}</span>
            <span className="fixed-node-source">{option.detail}</span>
            <span className="fixed-node-card-status"><span className="delay-muted">{option.requiresAlive && !canAuto ? "暂无可用节点" : option.badge}</span>{option.value === currentNode && <Badge variant="outline">{currentBadge}</Badge>}</span>
          </button>;
        })}
        {visibleNodes.map((node) => <button key={node.key} type="button" className={classNames("fixed-node-card", selected === node.name && "selected")} aria-pressed={selected === node.name} disabled={saving || !node.alive} onClick={() => { setSelected(node.name); setError(""); }}>
          <span className="fixed-node-card-heading"><Globe2 size={18} aria-hidden="true" /><strong title={node.name}>{node.name}</strong>{selected === node.name && <CheckCircle2 size={18} aria-hidden="true" />}</span>
          <span className="fixed-node-source">{node.subscription === "manual" ? "手动节点" : node.subscription || "未知来源"} · {node.type?.toUpperCase() || "未知协议"}</span>
          <span className="fixed-node-card-status"><span className={delayClass(node)}>{node.alive ? formatDelay(node) : "不可用"}</span>{node.name === currentNode && <Badge variant="outline">{currentBadge}</Badge>}</span>
        </button>)}
        {!visibleSpecials.length && !visibleNodes.length && <p className="fixed-node-empty">{emptyText}</p>}
      </div>
      <div className="fixed-node-feedback" aria-live="polite">
        {error ? <p role="alert">{error}</p> : <p>{candidateValid ? `已选择：${candidateLabel}` : invalidHint}</p>}
      </div>
      <footer className="dialog-footer">
        <Button variant="outline" disabled={saving} onClick={() => changeOpen(false)}>取消</Button>
        <Button disabled={!candidateValid || unchanged} loading={saving} onClick={confirmSelection}>{saving ? "切换中…" : unchanged ? "当前已使用" : "确认切换"}</Button>
      </footer>
    </DialogContent>
  </Dialog>;
}
