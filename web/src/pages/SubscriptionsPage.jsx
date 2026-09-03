import { useRef, useState } from "react";
import { Pencil, Plus, RefreshCw, Rss, Trash2, X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
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
import { Switch as UISwitch } from "@/components/ui/switch";
import { EmptyState } from "@/components/EmptyState";
import { Field } from "@/components/Field";
import { PageHeader } from "@/components/PageHeader";
import { StatusBadge } from "@/components/StatusBadge";
import { classNames, formatUserInfo } from "@/lib/format";

/**
 * SubscriptionsPage 渲染订阅资源管理页。
 *
 * 功能说明：
 * 以紧凑卡片展示订阅状态、用量与节点健康度；新增和编辑统一使用 Radix Dialog。
 * 停用操作会提交完整值对象，避免布尔开关覆盖名称、URL 或解析类型。
 *
 * 参数说明：
 * - overview: object，订阅和节点聚合状态。
 * - onDelete/onNavigateNodes/onSubAction/onWrite: Function，删除、跳转、刷新测速与写入回调。
 *
 * 返回值说明：
 * 返回订阅管理页 React 元素。
 *
 * 可能的异常/错误情况：
 * 启用无可用缓存的订阅可能同步失败；失败时弹窗保留或开关回退为服务端状态。
 */
export function SubscriptionsPage({ overview, onDelete, onNavigateNodes, onSubAction, onWrite }) {
  const emptyDraft = { name: "", url: "", type: "auto", enabled: true };
  const [editor, setEditor] = useState(null);
  const [draft, setDraft] = useState(emptyDraft);
  const [saving, setSaving] = useState(false);
  // 保存请求的取消控制器：订阅保存可能触发同步拉取（最长 3 分钟），
  // 期间「取消」/关闭对话框必须能立即中断等待，而不是卡到请求结束。
  const saveAbortRef = useRef(null);
  // 单个订阅的同步/测速是同步接口（最长 3 分钟），期间必须给行内按钮明确的加载态，
  // 否则点击后到完成 toast 之间页面毫无反馈。key 形如 `${name}:${action}`。
  const [pendingAction, setPendingAction] = useState("");

  /**
   * runSubAction 执行单个订阅的同步或测速，并维护行内加载状态。
   *
   * 参数说明：
   * - name: string，订阅名称。
   * - action: string，`refresh` 或 `test`。
   *
   * 返回值说明：返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 已有动作进行中时忽略新的点击；接口错误由 onSubAction 统一 toast，这里只负责状态复位。
   */
  async function runSubAction(name, action) {
    if (pendingAction) return;
    setPendingAction(`${name}:${action}`);
    try {
      await onSubAction(name, action);
    } finally {
      setPendingAction("");
    }
  }

  /**
   * openEditor 打开新增或编辑对话框。
   *
   * 参数说明：
   * - subscription: object | null，现有订阅；null 表示新增。
   *
   * 返回值说明：无。
   *
   * 可能的异常/错误情况：无；缺失字段使用安全默认值。
   */
  function openEditor(subscription = null) {
    setEditor(subscription ? { mode: "edit", originalName: subscription.name } : { mode: "add", originalName: "" });
    setDraft(subscription ? {
      name: subscription.name || "",
      url: subscription.url || "",
      type: subscription.type || "auto",
      enabled: subscription.enabled !== false,
    } : emptyDraft);
  }

  /**
   * updateDraft 更新订阅对话框的单个字段。
   *
   * 参数说明：
   * - key: string，字段名。
   * - value: string | boolean，字段值。
   *
   * 返回值说明：无。
   *
   * 可能的异常/错误情况：无；最终合法性由提交时后端校验。
   */
  function updateDraft(key, value) {
    setDraft((current) => ({ ...current, [key]: value }));
  }

  /**
   * submitSubscription 提交新增或编辑事务。
   *
   * 参数说明：
   * - event: React.FormEvent<HTMLFormElement>，表单提交事件。
   *
   * 返回值说明：返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 必填项为空时不提交；启用时的下载、校验、热更新或持久化失败由 onWrite toast。
   */
  async function submitSubscription(event) {
    event.preventDefault();
    if (!draft.name.trim() || !draft.url.trim() || saving || !editor) return;
    const controller = new AbortController();
    saveAbortRef.current = controller;
    setSaving(true);
    try {
      const payload = { ...draft, name: draft.name.trim(), url: draft.url.trim() };
      const editing = editor.mode === "edit";
      const url = editing ? `/api/subscriptions/${encodeURIComponent(editor.originalName)}` : "/api/subscriptions";
      const saved = await onWrite(url, payload, editing ? "订阅已更新" : "订阅已添加", editing ? "PUT" : "POST", controller.signal);
      if (saved) setEditor(null);
    } finally {
      saveAbortRef.current = null;
      setSaving(false);
    }
  }

  /**
   * cancelEditor 取消编辑：保存进行中先中断请求，再关闭对话框。
   *
   * 参数说明：
   * - close: boolean，是否同时关闭对话框（「取消」按钮保持打开以便修改后重试，X/遮罩关闭则直接收起）。
   *
   * 返回值说明：无。
   *
   * 可能的异常/错误情况：无；中断由 AbortController 完成，失败静默忽略。
   */
  function cancelEditor(close) {
    if (saving && saveAbortRef.current) {
      saveAbortRef.current.abort();
    }
    if (close || !saving) setEditor(null);
  }

  /**
   * toggleSubscription 切换现有订阅启用状态。
   *
   * 参数说明：
   * - subscription: object，当前订阅完整快照。
   * - enabled: boolean，目标启用状态。
   *
   * 返回值说明：返回 Promise<boolean>，由通用写入器返回提交结果。
   *
   * 可能的异常/错误情况：
   * 启用且拉取失败、又没有可用缓存时后端拒绝，轮询后开关保持原状态。
   */
  function toggleSubscription(subscription, enabled) {
    return onWrite(
      `/api/subscriptions/${encodeURIComponent(subscription.name)}`,
      { name: subscription.name, url: subscription.url, type: subscription.type || "auto", enabled },
      enabled ? "订阅已启用" : "订阅已停用",
      "PUT",
    );
  }

  return (
    <div className="stack">
      <PageHeader eyebrow="代理资源" title="订阅管理" detail="管理来源、启用状态、同步与缓存健康度。">
        <Button type="button" onClick={() => openEditor()}><Plus size={16} aria-hidden="true" />添加订阅</Button>
      </PageHeader>
      <div className="subscription-cards">
        {(overview.subscriptions || []).map((subscription) => {
          const info = formatUserInfo(subscription.userinfo);
          const stateLabel = {
            disabled: "已停用", empty: "无节点", error: "异常", degraded: "使用缓存", healthy: "正常",
          }[subscription.state] || "未知";
          const testing = pendingAction === `${subscription.name}:test`;
          const syncing = pendingAction === `${subscription.name}:refresh`;
          return (
            <article className="subscription-card" key={subscription.name}>
              <div className="subscription-card-head">
                <div className="subscription-icon"><Rss size={18} aria-hidden="true" /></div>
                <div><h3>{subscription.name}</h3><p title={subscription.url}>{subscription.url}</p></div>
                <UISwitch ariaLabel={`启用 ${subscription.name}`} checked={subscription.enabled !== false} onCheckedChange={(enabled) => toggleSubscription(subscription, enabled)} />
              </div>
              <div className="subscription-stats">
                <span><b>{subscription.alive}</b>/{subscription.total} 可用</span>
                <StatusBadge ok={subscription.state === "healthy"} text={stateLabel} />
                <Badge variant="outline">{(subscription.type || "auto").toUpperCase()}</Badge>
              </div>
              {info && <div className="subscription-usage"><span>{info.usage}</span><span className={classNames(info.urgent && "urgent")}>{info.expire}</span></div>}
              <div className="subscription-card-actions">
                <Button size="sm" variant="ghost" type="button" onClick={() => onNavigateNodes(subscription.name)}>查看节点</Button>
                <Button disabled={!subscription.enabled || Boolean(pendingAction)} loading={testing} size="sm" variant="outline" type="button" onClick={() => runSubAction(subscription.name, "test")}>{testing ? "测速中…" : "测速"}</Button>
                <Button disabled={!subscription.enabled || Boolean(pendingAction)} loading={syncing} size="sm" variant="outline" type="button" onClick={() => runSubAction(subscription.name, "refresh")}>{!syncing && <RefreshCw size={14} aria-hidden="true" />}{syncing ? "同步中…" : "同步"}</Button>
                <Button aria-label={`编辑订阅 ${subscription.name}`} size="icon" variant="ghost" type="button" onClick={() => openEditor(subscription)}><Pencil size={15} aria-hidden="true" /></Button>
                <Button aria-label={`删除订阅 ${subscription.name}`} size="icon" variant="destructive-ghost" type="button" onClick={() => onDelete(`/api/subscriptions/${encodeURIComponent(subscription.name)}`, "订阅已删除", `订阅 ${subscription.name}`)}><Trash2 size={15} aria-hidden="true" /></Button>
              </div>
            </article>
          );
        })}
      </div>
      {(overview.subscriptions || []).length === 0 && <EmptyState title="还没有订阅" detail="添加订阅后，节点会自动出现在代理节点页面。" />}

      <Dialog open={Boolean(editor)} onOpenChange={(open) => { if (!open) cancelEditor(true); }}>
        <DialogContent>
          <DialogClose className="dialog-close" aria-label="关闭订阅对话框"><X size={16} aria-hidden="true" /></DialogClose>
          <form onSubmit={submitSubscription}>
            <DialogHeader>
              <DialogTitle>{editor?.mode === "edit" ? "编辑订阅" : "添加订阅"}</DialogTitle>
              <DialogDescription>仅改名会立即保存；修改地址或启用订阅时会立即拉取并校验，耗时取决于订阅响应速度，可随时取消。</DialogDescription>
            </DialogHeader>
            <div className="dialog-form">
              <Field label="订阅名称"><Input autoFocus value={draft.name} placeholder="例如：主力机场" onChange={(event) => updateDraft("name", event.target.value)} /></Field>
              <Field label="订阅地址"><Input value={draft.url} placeholder="https://example.com/subscription" onChange={(event) => updateDraft("url", event.target.value)} /></Field>
              <Field label="解析类型">
                <Select
                  ariaLabel="订阅解析类型"
                  value={draft.type}
                  onValueChange={(value) => updateDraft("type", value)}
                  options={[{ value: "auto", label: "自动识别" }, { value: "clash", label: "Clash YAML" }, { value: "share", label: "分享链接" }]}
                />
              </Field>
              <UISwitch checked={draft.enabled} label="保存后启用此订阅" onCheckedChange={(enabled) => updateDraft("enabled", enabled)} />
            </div>
            <DialogFooter>
              <Button variant="outline" type="button" onClick={() => cancelEditor(false)}>{saving ? "取消等待" : "取消"}</Button>
              <Button disabled={!draft.name.trim() || !draft.url.trim()} loading={saving} type="submit">{editor?.mode === "edit" ? "保存修改" : "添加订阅"}</Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </div>
  );
}
