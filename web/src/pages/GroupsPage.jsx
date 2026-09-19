import { useMemo, useRef, useState } from "react";
import { ArrowRightLeft, ChevronDown, Copy, Pencil, Plus, Search, Trash2, X } from "lucide-react";
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
import { Select } from "@/components/ui/select";
import { Field } from "@/components/Field";
import { EXIT_SPECIALS, FixedNodeDialog } from "@/components/FixedNodeDialog";
import { PageHeader } from "@/components/PageHeader";
import { PanelTitle } from "@/components/PanelTitle";
import { StatusBadge } from "@/components/StatusBadge";
import { classNames, delayClass, formatDelay, sortByDelay } from "@/lib/format";

const GROUP_TYPE_LABELS = {
  "fallback": "故障转移",
  "url-test": "自动测速",
  "load-balance": "负载均衡",
  "select": "手动选择",
};

/** selectExitLabel 把分组出口取值转换为按钮文案；value 为 string，返回 string，无异常。 */
function selectExitLabel(value) {
  if (value === "AUTO") return "自动最快（AUTO）";
  if (value === "DIRECT") return "直连（DIRECT）";
  return value || "选择出口节点";
}


/**
 * GroupsPage 渲染节点分组页。
 *
 * 参数说明：
 * - forms: object，表单状态。
 * - groupSort: string，待选节点排序。
 * - overview: object，概览数据。
 * - selectedNodes: Set<string>，当前勾选节点。
 * - onCopy/onDelete/onForm/onPost/onSort/onSubmit/onToggleNode: Function，操作回调。
 *   onPost 同时承载 select 分组的出口切换（POST /api/groups/{name}/select）。
 *
 * 返回值说明：
 * 返回分组页 React 元素。
 *
 * 可能的异常/错误情况：
 * 表单缺失本地拦截，后端冲突由父组件展示。
 */
export function GroupsPage({ forms, groupSort, overview, selectedNodes, onCopy, onDelete, onForm, onPost, onSort, onSubmit, onToggleNode }) {
  const [createOpen, setCreateOpen] = useState(false);
  const [editingName, setEditingName] = useState("");
  const [nodeQuery, setNodeQuery] = useState("");
  const [expandedGroups, setExpandedGroups] = useState(() => new Set());
  const [pickerGroup, setPickerGroup] = useState(null);
  const pickerTrigger = useRef(null);
  const nodes = groupSort === "delay" ? sortByDelay(overview.nodes, "delay") : overview.nodes;
  const sourceOptions = useMemo(() => {
    const names = (overview.subscriptions || []).filter((subscription) => subscription.enabled).map((subscription) => subscription.name);
    if ((overview.manual_nodes || []).length > 0 || overview.nodes.some((node) => node.subscription === "manual")) {
      names.unshift("manual");
    }
    return names;
  }, [overview]);
  const usedBy = useMemo(() => {
    const result = {};
    (overview.groups || []).forEach((group) => {
      (group.nodes || []).forEach((node) => {
        result[node] = [...(result[node] || []), group.name];
      });
    });
    return result;
  }, [overview.groups]);

  /**
   * groupMemberNames 计算分组当前的成员节点名。
   *
   * 功能说明：
   * 内置 PROXY 组（规则模式默认出口）成员是全部可用节点；订阅来源分组随订阅动态变化；
   * 手动分组则使用固定成员。列表渲染与出口弹窗共用这一份口径，避免两处判断漂移。
   *
   * 参数说明：
   * - group: object，overview 中的完整策略分组。
   *
   * 返回值说明：返回 string[] 成员节点名。
   *
   * 可能的异常/错误情况：无；分组字段缺失时按空成员处理。
   */
  function groupMemberNames(group) {
    if (group.builtin) return overview.nodes.filter((node) => node.alive).map((node) => node.name);
    if (group.subscription) return overview.nodes.filter((node) => node.subscription === group.subscription).map((node) => node.name);
    return group.nodes || [];
  }

  /*
   * 出口弹窗的候选只保留该分组当前存活的成员（与后端 groupMemberAlive 校验同口径），
   * 内置 PROXY 组额外提供 AUTO / DIRECT 两个保留出口。
   */
  const pickerNodes = pickerGroup
    ? groupMemberNames(pickerGroup).map((name) => overview.nodes.find((node) => node.name === name)).filter((node) => node?.alive)
    : [];
  const canAuto = overview.nodes.some((node) => node.alive && !node.tunnel);

  const normalizedQuery = nodeQuery.trim().toLowerCase();
  const filteredNodes = normalizedQuery
    ? nodes.filter((node) => `${node.name} ${node.subscription || ""}`.toLowerCase().includes(normalizedQuery))
    : nodes;
  const selectedCount = overview.nodes.reduce((count, node) => count + (selectedNodes.has(node.name) ? 1 : 0), 0);

  /**
   * toggleGroupExpanded 展开或收起某个分组的成员节点列表。
   *
   * 参数说明：
   * - name: string，分组名。
   *
   * 返回值说明：无。
   *
   * 可能的异常/错误情况：无；展开状态只保存在本地，刷新概览不影响。
   */
  function toggleGroupExpanded(name) {
    setExpandedGroups((current) => {
      const next = new Set(current);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });
  }

  /**
   * setNodesSelection 把一批节点的勾选状态统一调整为目标值。
   *
   * 功能说明：
   * 全局勾选集合只暴露单节点 toggle，这里参考 syncSelectedNodes 的差量模式做批量
   * 操作，避免逐个点击，也避免重复 toggle 把已符合目标的节点改反。
   *
   * 参数说明：
   * - names: string[]，需要调整的节点名。
   * - shouldSelect: boolean，目标勾选状态。
   *
   * 返回值说明：无。
   *
   * 可能的异常/错误情况：已不在节点目录中的名字会被忽略。
   */
  function setNodesSelection(names, shouldSelect) {
    const nameSet = new Set(names);
    overview.nodes.forEach((node) => {
      if (nameSet.has(node.name) && selectedNodes.has(node.name) !== shouldSelect) onToggleNode(node.name);
    });
  }

  /**
   * syncSelectedNodes 把全局节点选择集合调整为目标集合。
   *
   * 参数说明：
   * - targetNames: string[]，编辑分组时需要选中的节点名。
   *
   * 返回值说明：无。
   *
   * 可能的异常/错误情况：
   * 已从当前节点目录消失的旧成员不会出现在选择器中，但后端保存完整目标前仍会校验。
   */
  function syncSelectedNodes(targetNames) {
    const target = new Set(targetNames || []);
    overview.nodes.forEach((node) => {
      if (selectedNodes.has(node.name) !== target.has(node.name)) onToggleNode(node.name);
    });
  }

  /**
   * openCreateDialog 复位分组草稿并打开新建对话框。
   *
   * 参数说明：无。
   * 返回值说明：无。
   * 可能的异常/错误情况：无；草稿清理只影响尚未提交的表单状态。
   */
  function openCreateDialog() {
    setEditingName("");
    setNodeQuery("");
    onForm("groupName", "");
    onForm("groupPort", "");
    onForm("groupType", "fallback");
    onForm("groupSubscription", "");
    syncSelectedNodes([]);
    setCreateOpen(true);
  }

  /**
   * openEditDialog 用现有分组值填充编辑对话框。
   *
   * 参数说明：
   * - group: object，overview 中的完整策略分组。
   *
   * 返回值说明：无。
   *
   * 可能的异常/错误情况：无；分组名在编辑期间锁定以保护 dialer-proxy 引用。
   */
  function openEditDialog(group) {
    setEditingName(group.name);
    setNodeQuery("");
    onForm("groupName", group.name);
    onForm("groupPort", String(group.port));
    onForm("groupType", group.type || "url-test");
    onForm("groupSubscription", group.subscription || "");
    syncSelectedNodes(group.nodes || []);
    setCreateOpen(true);
  }

  /**
   * removeStaleNodes 从手动分组中移除已不在节点目录的成员。
   *
   * 功能说明：
   * 通过 PUT 提交完整分组实现移除，复用后端的端口冲突校验、热更新与持久化事务。
   * 单个移除与批量清理共用此入口，只是 names 长度不同。
   *
   * 参数说明：
   * - group: object，overview 中的完整策略分组。
   * - names: string[]，待移除的失效节点名。
   *
   * 返回值说明：返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 如果移除后分组不再有任何成员，后端会拒绝并 toast 提示，配置保持不变。
   */
  async function removeStaleNodes(group, names) {
    const removal = new Set(names);
    await onPost(`/api/groups/${encodeURIComponent(group.name)}`, {
      name: group.name,
      port: group.port,
      type: group.type || "fallback",
      subscription: "",
      nodes: (group.nodes || []).filter((name) => !removal.has(name)),
    }, names.length > 1 ? `已清除 ${names.length} 个失效节点` : `已移除失效节点 ${names[0]}`, "PUT");
  }

  /**
   * submitGroupDialog 提交策略分组并在事务成功后关闭对话框。
   *
   * 参数说明：无；字段由父级 forms 与 selectedNodes 提供。
   *
   * 返回值说明：返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 表单不完整、端口冲突或后端持久化失败时保持弹窗打开，方便继续修正。
   */
  async function submitGroupDialog() {
    if (await onSubmit(editingName)) setCreateOpen(false);
  }

  /**
   * openGroupPicker 打开某个 select 分组的出口选择弹窗。
   *
   * 参数说明：
   * - event: React.MouseEvent，触发按钮事件，用于记录焦点归还入口。
   * - group: object，overview 中的完整策略分组（select 类型）。
   *
   * 返回值说明：无。
   *
   * 可能的异常/错误情况：无；分组在弹窗打开期间被删除时后端会拒绝提交并提示。
   */
  function openGroupPicker(event, group) {
    pickerTrigger.current = event.currentTarget;
    setPickerGroup(group);
  }

  /**
   * selectGroupNode 切换 select 分组的手动出口节点。
   *
   * 功能说明：
   * 后端要求目标节点在分组当前可用成员中（存活且属于该组），因此弹窗只列出存活成员；
   * 切换立即热更新并持久化到 state-dir，重启后保持。
   *
   * 参数说明：
   * - group: object，overview 中的完整策略分组（select 类型）。
   * - nodeName: string，目标出口节点名。
   *
   * 返回值说明：返回 Promise<boolean>；成功返回 true，失败返回 false 以便弹窗保留选择并提示重试。
   *
   * 可能的异常/错误情况：
   * 分组非 select 类型、节点恰好失效或热更新失败时由 onPost 展示后端错误，分组保持原出口。
   */
  async function selectGroupNode(group, nodeName) {
    if (!group || !nodeName || nodeName === group.selected) return true;
    return onPost(`/api/groups/${encodeURIComponent(group.name)}/select`, { node: nodeName }, `分组 ${group.name} 出口已切换到「${selectExitLabel(nodeName)}」`);
  }


  return (
    <div className="stack">
      <PageHeader eyebrow="访问策略" title="策略分组" detail="为指定场景提供独立入口，并查看候选节点健康度。">
        <Button type="button" onClick={openCreateDialog}><Plus size={16} aria-hidden="true" />新建分组</Button>
      </PageHeader>
      <section className="panel">
        <PanelTitle title="已有策略分组" detail="首个内置分组是规则模式的默认出口；其余分组提供独立代理入口" />
        <ul className="item-list">
          {(overview.groups || []).map((group) => {
            const isExpanded = expandedGroups.has(group.name);
            const isBuiltin = Boolean(group.builtin);
            // 内置 PROXY 组（规则模式默认出口）成员是全部可用节点；订阅来源分组随订阅
            // 动态变化；手动分组则展示固定成员（口径见 groupMemberNames）。
            const memberNames = groupMemberNames(group);
            const staleNames = group.subscription || isBuiltin
              ? []
              : memberNames.filter((name) => !overview.nodes.some((node) => node.name === name));
            // select 分组的可切换出口：后端只接受当前存活成员（见 groupMemberAlive），
            // 这里用同一口径过滤，避免列出注定被 400 拒绝的选项；内置 PROXY 组额外提供
            // AUTO（有可用节点时）与 DIRECT。
            const aliveMembers = memberNames.filter((name) => overview.nodes.some((node) => node.name === name && node.alive));
            return (
            <li className="group-item" key={group.name}>
              <div className="group-row">
                <button aria-expanded={isExpanded} aria-label={`查看分组 ${group.name} 的节点`} className="group-toggle" type="button" onClick={() => toggleGroupExpanded(group.name)}>
                  <ChevronDown size={15} aria-hidden="true" />
                  <b>{group.name}</b>
                  {isBuiltin && <Badge variant="secondary">内置</Badge>}
                </button>
                {isBuiltin ? <span className="delay-muted">主端口</span> : <button className="copy-link" type="button" onClick={() => onCopy(group.port)}>:{group.port}<Copy size={14} /></button>}
                <span>{GROUP_TYPE_LABELS[group.type] || group.type || "自动测速"}</span>
                <span>{isBuiltin ? "规则模式默认出口" : group.subscription ? `来源：${group.subscription}` : `${(group.nodes || []).length} 个固定节点`}</span>
                <StatusBadge
                  ok={isBuiltin
                    ? aliveMembers.length > 0
                    : group.subscription
                      ? overview.subscriptions.some((subscription) => subscription.name === group.subscription && subscription.enabled && subscription.alive > 0)
                      : (group.nodes || []).some((name) => overview.nodes.some((node) => node.name === name && node.alive))}
                  text={isBuiltin
                    ? `${aliveMembers.length} 个可用`
                    : group.subscription
                      ? `${overview.subscriptions.find((subscription) => subscription.name === group.subscription)?.alive || 0} 个可用`
                      : `${(group.nodes || []).filter((name) => overview.nodes.some((node) => node.name === name && node.alive)).length}/${(group.nodes || []).length} 可用`}
                />
                {group.type === "select" && (
                  <div className="group-select">
                    <span className="group-select-label">{isBuiltin ? "默认出口" : "当前出口"}</span>
                    <button
                      aria-haspopup="dialog"
                      aria-label={isBuiltin ? "选择规则模式默认出口" : `选择分组 ${group.name} 的出口节点`}
                      className="group-select-trigger"
                      disabled={!isBuiltin && aliveMembers.length === 0}
                      title={selectExitLabel(group.selected)}
                      type="button"
                      onClick={(event) => openGroupPicker(event, group)}
                    >
                      <span>{selectExitLabel(group.selected)}</span>
                      <ArrowRightLeft size={13} aria-hidden="true" />
                    </button>
                  </div>
                )}
                {!isBuiltin && <Button aria-label={`编辑策略分组 ${group.name}`} size="icon" variant="ghost" type="button" onClick={() => openEditDialog(group)}><Pencil size={15} aria-hidden="true" /></Button>}
                {!isBuiltin && <Button aria-label={`删除策略分组 ${group.name}`} size="icon" variant="destructive-ghost" type="button" onClick={() => onDelete(`/api/groups/${encodeURIComponent(group.name)}`, "分组已删除", `策略分组 ${group.name}`)}>
                  <Trash2 size={16} aria-hidden="true" />
                </Button>}
              </div>
              {isExpanded && (
                <ul className="group-node-list">
                  {staleNames.length > 1 && (
                    <li>
                      <button className="group-clean-stale" type="button" onClick={() => removeStaleNodes(group, staleNames)}>
                        <Trash2 size={12} aria-hidden="true" />清除 {staleNames.length} 个失效节点
                      </button>
                    </li>
                  )}
                  {memberNames.length === 0 && (
                    <li className="group-node-empty">{group.subscription ? "该订阅下暂无节点" : "该分组暂无成员节点"}</li>
                  )}
                  {memberNames.map((name) => {
                    const node = overview.nodes.find((item) => item.name === name);
                    return (
                      <li className="group-node" key={name}>
                        <i className={node && node.alive ? "on" : ""} aria-hidden="true" />
                        <span className="group-node-name">{name}</span>
                        {group.type === "select" && group.selected === name && <span className="group-node-selected">当前出口</span>}
                        {node ? (
                          <span className={delayClass(node)}>{formatDelay(node)}</span>
                        ) : (
                          <>
                            <span className="delay-muted">已不在节点目录</span>
                            <button aria-label={`从分组 ${group.name} 移除失效节点 ${name}`} className="group-node-remove" type="button" onClick={() => removeStaleNodes(group, [name])}>
                              <Trash2 size={12} aria-hidden="true" />
                            </button>
                          </>
                        )}
                      </li>
                    );
                  })}
                </ul>
              )}
            </li>
            );
          })}
        </ul>
      </section>
      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent className="group-dialog">
        <DialogClose className="dialog-close" aria-label="关闭新建分组对话框"><X size={16} aria-hidden="true" /></DialogClose>
        <DialogHeader><DialogTitle>{editingName ? "编辑策略分组" : "新建策略分组"}</DialogTitle><DialogDescription>可从一个订阅自动取节点，也可以手动勾选节点。编辑时分组名保持不变。</DialogDescription></DialogHeader>
        <div className="form-grid group-form">
          <Field label="分组名称"><input disabled={Boolean(editingName)} value={forms.groupName} onChange={(event) => onForm("groupName", event.target.value)} placeholder="例如：视频线路" /></Field>
          <Field label="本机端口"><input type="number" min="1" max="65535" value={forms.groupPort} onChange={(event) => onForm("groupPort", event.target.value)} placeholder="例如：42020" /></Field>
          <Field label="选择策略">
            <Select
              ariaLabel="策略分组选择策略"
              value={forms.groupType}
              onValueChange={(value) => onForm("groupType", value)}
              options={[
                { value: "fallback", label: "故障转移（按顺序切换）" },
                { value: "url-test", label: "自动测速（选择最快）" },
                { value: "load-balance", label: "负载均衡（分散连接）" },
                { value: "select", label: "手动选择（固定出口，可在列表切换）" },
              ]}
            />
          </Field>
          <Field label="节点来源">
            <Select
              ariaLabel="策略分组节点来源"
              value={forms.groupSubscription}
              onValueChange={(value) => onForm("groupSubscription", value)}
              options={[
                { value: "", label: "手动选择节点" },
                ...sourceOptions.map((name) => ({ value: name, label: `使用订阅：${name}` })),
              ]}
            />
          </Field>
        </div>
        <div className="toolbar node-picker-toolbar">
          <div className="picker-search">
            <Field label="搜索节点">
              <div className="input-with-icon">
                <Search size={15} aria-hidden="true" />
                <input
                  aria-label="搜索候选节点"
                  disabled={Boolean(forms.groupSubscription)}
                  placeholder="节点名或订阅名…"
                  value={nodeQuery}
                  onChange={(event) => setNodeQuery(event.target.value)}
                />
              </div>
            </Field>
          </div>
          <Field compact label="候选节点排序">
            <Select
              ariaLabel="候选节点排序"
              value={groupSort}
              onValueChange={onSort}
              options={[{ value: "default", label: "默认顺序" }, { value: "delay", label: "延迟从低到高" }]}
            />
          </Field>
          <span className="picker-count">已选 {selectedCount} / 显示 {filteredNodes.length}</span>
          <Button disabled={Boolean(forms.groupSubscription) || filteredNodes.length === 0} size="sm" type="button" variant="ghost" onClick={() => setNodesSelection(filteredNodes.map((node) => node.name), true)}>全选结果</Button>
          <Button disabled={Boolean(forms.groupSubscription) || selectedCount === 0} size="sm" type="button" variant="ghost" onClick={() => setNodesSelection(overview.nodes.map((node) => node.name), false)}>清空</Button>
        </div>
        <div className="node-picker-scroll">
          <div className="node-picker">
            {filteredNodes.length === 0 && <p className="picker-empty">没有匹配的节点，换个关键词试试</p>}
            {filteredNodes.map((node) => (
              <label className={classNames("node-option", (!node.alive || forms.groupSubscription) && "disabled")} key={`${node.subscription}:${node.name}`}>
                <input checked={selectedNodes.has(node.name)} disabled={Boolean(forms.groupSubscription)} type="checkbox" onChange={() => onToggleNode(node.name)} />
                <span className="node-option-main">
                  <span className="node-name">{node.name}</span>
                  {(usedBy[node.name] || []).length > 0 && (
                    <span className="node-option-tags">
                      {(usedBy[node.name] || []).map((group) => <small key={group} title={group}>{group}</small>)}
                    </span>
                  )}
                </span>
                <span className={delayClass(node)}>{formatDelay(node)}</span>
              </label>
            ))}
          </div>
        </div>
        <DialogFooter><Button variant="outline" type="button" onClick={() => setCreateOpen(false)}>取消</Button><Button type="button" onClick={submitGroupDialog}>{editingName ? <Pencil size={16} aria-hidden="true" /> : <Plus size={16} aria-hidden="true" />}{editingName ? "保存修改" : "创建分组"}</Button></DialogFooter>
        </DialogContent>
      </Dialog>
      {/*
       * 分组出口弹窗：内置 PROXY 组沿用默认出口文案并附 AUTO / DIRECT 保留出口，其余
       * select 分组只列出该分组当前存活的成员。
       */}
      {pickerGroup && (
        <FixedNodeDialog
          canAuto={canAuto}
          currentBadge={pickerGroup.builtin ? "当前默认出口" : "当前出口"}
          currentNode={pickerGroup.selected}
          description={pickerGroup.builtin ? undefined : `分组 ${pickerGroup.name} 的固定出口；切换后立即热更新并保持到重启之后。`}
          invalidHint="请选择该分组中当前可用的成员节点。"
          nodes={pickerNodes}
          specials={pickerGroup.builtin ? EXIT_SPECIALS : []}
          title={pickerGroup.builtin ? "选择默认出口" : `选择分组出口 · ${pickerGroup.name}`}
          triggerElement={pickerTrigger.current}
          onClose={() => setPickerGroup(null)}
          onSelect={(value) => selectGroupNode(pickerGroup, value)}
        />
      )}
    </div>
  );
}
