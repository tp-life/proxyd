import { useMemo, useState } from "react";
import { ArrowDown, ArrowUp, Pencil, Plus, Search, Trash2, X } from "lucide-react";
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
import { Field } from "@/components/Field";
import { PageHeader } from "@/components/PageHeader";
import { PanelTitle } from "@/components/PanelTitle";

/**
 * RulesPage 渲染规则与规则源页。
 *
 * 参数说明：
 * - forms/ruleContent/ruleUrls/overview: object，页面数据。
 * - onDelete/onForm/onPost/onViewContent: Function，操作回调。
 *
 * 返回值说明：
 * 返回规则页 React 元素。
 *
 * 可能的异常/错误情况：
 * 表单缺失本地拦截；API 失败由父组件展示。
 */
export function RulesPage({ forms, ruleContent, ruleUrls, overview, onDelete, onForm, onPost, onViewContent }) {
  const [query, setQuery] = useState("");
  const [editing, setEditing] = useState(null);
  const visibleRules = useMemo(() => {
    const normalized = query.trim().toLowerCase();
    return (overview.custom_rules || [])
      .map((rule, index) => ({ rule, index }))
      .filter((item) => !normalized || item.rule.toLowerCase().includes(normalized));
  }, [overview.custom_rules, query]);

  /**
   * saveEditedRule 原位保存自定义规则。
   *
   * 参数说明：
   * - event: React.FormEvent<HTMLFormElement>，编辑弹窗提交事件。
   *
   * 返回值说明：返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 空规则直接拦截；后端校验、mihomo 重载或持久化失败由 onPost 统一展示。
   */
  async function saveEditedRule(event) {
    event.preventDefault();
    if (!editing?.rule.trim()) return;
    const saved = await onPost(`/api/rules/${editing.index}`, { rule: editing.rule.trim() }, "规则已更新", "PUT");
    if (saved) setEditing(null);
  }

  /**
   * moveRule 调整规则在完整 custom-rules 列表中的优先级。
   *
   * 参数说明：
   * - from: number，当前零基下标。
   * - to: number，目标零基下标。
   *
   * 返回值说明：返回 Promise<boolean>，由写入器给出结果。
   *
   * 可能的异常/错误情况：
   * 越界按钮在 UI 已禁用；并发配置变化仍由后端事务校验并回滚。
   */
  function moveRule(from, to) {
    return onPost("/api/rules/reorder", { from, to }, "规则优先级已更新");
  }

  return (
    <div className="stack">
      <PageHeader eyebrow="访问策略" title="规则管理" detail="维护自定义访问规则与远程规则源，列表顺序即匹配优先级。" />
      <section className="panel">
        <PanelTitle title="自定义访问规则" detail="规则按从上到下的顺序匹配，新增规则会写入当前配置" />
        <form
          className="form-grid rule-form"
          onSubmit={async (event) => {
            event.preventDefault();
            if (!forms.rule.trim()) return;
            if (await onPost("/api/rules", { rule: forms.rule.trim() }, "规则已添加")) onForm("rule", "");
          }}
        >
          <Field label="规则内容" hint="格式由规则类型、匹配值和目标策略组成">
            <input aria-label="自定义规则内容" className="mono-input" value={forms.rule} onChange={(event) => onForm("rule", event.target.value)} placeholder="DOMAIN-SUFFIX,example.com,DIRECT" />
          </Field>
          <Button className="form-submit" type="submit"><Plus size={16} aria-hidden="true" /><span>添加规则</span></Button>
        </form>
        <div className="toolbar rule-toolbar">
          <Field compact label="搜索自定义规则"><div className="input-with-icon"><Search size={15} aria-hidden="true" /><Input value={query} placeholder="类型、域名或策略" onChange={(event) => setQuery(event.target.value)} /></div></Field>
          <Badge variant="outline">{visibleRules.length}/{(overview.custom_rules || []).length} 条</Badge>
        </div>
        <ul className="item-list rule-list">
          {visibleRules.map(({ rule, index }) => (
            <li key={`${rule}:${index}`}>
              <span className="rule-index">{index + 1}</span><code>{rule}</code>
              <div className="row-actions">
                <Button aria-label={`上移规则 ${rule}`} disabled={index === 0} size="icon" variant="ghost" type="button" onClick={() => moveRule(index, index - 1)}><ArrowUp size={15} aria-hidden="true" /></Button>
                <Button aria-label={`下移规则 ${rule}`} disabled={index === overview.custom_rules.length - 1} size="icon" variant="ghost" type="button" onClick={() => moveRule(index, index + 1)}><ArrowDown size={15} aria-hidden="true" /></Button>
                <Button aria-label={`编辑规则 ${rule}`} size="icon" variant="ghost" type="button" onClick={() => setEditing({ index, rule })}><Pencil size={15} aria-hidden="true" /></Button>
                <Button aria-label={`删除规则 ${rule}`} size="icon" variant="destructive-ghost" type="button" onClick={() => onDelete(`/api/rules/${index}`, "规则已删除", `规则 ${rule}`)}>
                <Trash2 size={16} aria-hidden="true" />
                </Button>
              </div>
            </li>
          ))}
        </ul>
      </section>
      <section className="panel">
        <PanelTitle title="远程规则源" detail="远程内容只读；需要改动时请更新源文件或重新配置 URL" />
        <form
          className="form-grid rule-source-form"
          onSubmit={async (event) => {
            event.preventDefault();
            if (!forms.ruleURLName.trim() || !forms.ruleURL.trim()) return;
            if (await onPost("/api/rule-urls", { name: forms.ruleURLName.trim(), url: forms.ruleURL.trim() }, "规则 URL 已添加")) {
              onForm("ruleURLName", "");
              onForm("ruleURL", "");
            }
          }}
        >
          <Field label="规则源名称"><input value={forms.ruleURLName} onChange={(event) => onForm("ruleURLName", event.target.value)} placeholder="例如：局域网直连" /></Field>
          <Field label="规则源地址"><input value={forms.ruleURL} onChange={(event) => onForm("ruleURL", event.target.value)} placeholder="https://example.com/rules.txt" /></Field>
          <Button className="form-submit" type="submit"><Plus size={16} aria-hidden="true" /><span>添加规则源</span></Button>
        </form>
        <div className="rule-url-list">
          {ruleUrls.map((ruleURL) => (
            <article className="rule-url" key={ruleURL.name}>
              <div>
                <b>{ruleURL.name}</b>
                <code>{ruleURL.url}</code>
              </div>
              <span>{ruleURL.error ? "拉取失败" : `${ruleURL.count} 条${ruleURL.warn ? "（缓存）" : ""}`}</span>
              <Button size="sm" variant="outline" type="button" onClick={() => onViewContent(ruleURL.name)}>{ruleContent[ruleURL.name]?.open ? "收起内容" : "查看内容"}</Button>
              <Button aria-label={`删除规则源 ${ruleURL.name}`} size="icon" variant="destructive-ghost" type="button" onClick={() => onDelete(`/api/rule-urls/${encodeURIComponent(ruleURL.name)}`, "规则 URL 已删除", `规则源 ${ruleURL.name}`)}>
                <Trash2 size={16} aria-hidden="true" />
              </Button>
              {ruleContent[ruleURL.name]?.open && <pre>{ruleContent[ruleURL.name].text}</pre>}
            </article>
          ))}
        </div>
      </section>
      <Dialog open={Boolean(editing)} onOpenChange={(open) => { if (!open) setEditing(null); }}>
        <DialogContent>
          <DialogClose className="dialog-close" aria-label="关闭规则编辑对话框"><X size={16} aria-hidden="true" /></DialogClose>
          <form onSubmit={saveEditedRule}>
            <DialogHeader><DialogTitle>编辑自定义规则</DialogTitle><DialogDescription>规则会保留当前优先级位置，并在保存前完成配置校验。</DialogDescription></DialogHeader>
            <div className="dialog-form"><Field label="规则内容"><Input className="mono-input" value={editing?.rule || ""} onChange={(event) => setEditing((current) => ({ ...current, rule: event.target.value }))} /></Field></div>
            <DialogFooter><Button variant="outline" type="button" onClick={() => setEditing(null)}>取消</Button><Button disabled={!editing?.rule.trim()} type="submit">保存规则</Button></DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </div>
  );
}
