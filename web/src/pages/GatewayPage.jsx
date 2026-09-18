import { useState } from "react";
import { Pencil, Plus, RefreshCw, Trash2, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Dialog, DialogClose, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Select } from "@/components/ui/select";
import { Switch as UISwitch } from "@/components/ui/switch";
import { EmptyState } from "@/components/EmptyState";
import { Field } from "@/components/Field";
import { PageHeader } from "@/components/PageHeader";
import { PanelTitle } from "@/components/PanelTitle";
import { StatusBadge } from "@/components/StatusBadge";
import { PHASE_LABELS } from "@/lib/dashboard";

/** GATEWAY_POLICY_LABELS 是设备出口策略的展示文案。 */
const GATEWAY_POLICY_LABELS = { direct: "直连", proxy: "代理", "": "代理" };

/**
 * policyLabel 把设备策略投影为可读文案。
 * 参数说明：policy 为 string（direct|proxy|group:<名>|空）。
 * 返回值说明：返回 string 文案。
 * 可能的异常/错误情况：未知值原样展示，便于发现后端枚举漂移。
 */
function policyLabel(policy) {
  if (policy && policy.startsWith("group:")) return `分组：${policy.slice("group:".length)}`;
  return GATEWAY_POLICY_LABELS[policy] || policy || "代理";
}

/**
 * GatewayPage 渲染 LAN 网关页：模块状态、启用前预检、设备登记表与使用指引。
 *
 * 功能说明：
 * 旁路由模式下 proxyd 不接管 DHCP；本页负责登记下游设备（名称/IP/策略）、展示
 * 执行层状态（pf/nftables 规则与 IPv4 转发），并给出把设备网关/DNS 指向本机的
 * 静态指引。macOS 的特权 helper 安装引导经预检卡展示。
 *
 * 参数说明：
 * - overview: object | null，`/api/gateway` 响应（相位/平台/转发/规则/端口/设备表）。
 * - precheck: object | null，`/api/gateway/precheck` 响应（平台支持性与修复指引）。
 * - groups: Array<object>，策略分组列表（设备策略的 group: 选项来源）。
 * - loading/refreshing/hasLoaded: boolean，加载状态。
 * - error: string，最近一次加载错误文本。
 * - reload/toggleEnabled: Function，状态刷新与模块启停。
 * - addDevice/updateDevice/removeDevice: Function，设备表 CRUD。
 *
 * 返回值说明：
 * 返回网关页 React 元素。
 *
 * 可能的异常/错误情况：
 * 加载失败保留旧数据并显示错误条带；写操作失败由 hook toast，对话框保持打开。
 */
export function GatewayPage({ overview, precheck, groups, loading, refreshing, error, hasLoaded, reload, toggleEnabled, addDevice, updateDevice, removeDevice }) {
  const [dialogOpen, setDialogOpen] = useState(false);
  const [editingName, setEditingName] = useState("");
  const [form, setForm] = useState({ name: "", ip: "", mac: "", policy: "proxy" });

  const devices = overview?.devices || [];
  const phase = overview?.phase || "idle";
  const enabled = phase !== "disabled";
  const initialLoading = loading && !hasLoaded && !overview;

  const policyOptions = [
    { value: "proxy", label: "代理（默认）" },
    { value: "direct", label: "直连" },
    ...(groups || []).map((group) => ({ value: `group:${group.name}`, label: `分组：${group.name}` })),
  ];

  /**
   * openCreateDialog 复位设备草稿并打开登记对话框。
   * 参数说明：无。返回值说明：无。可能的异常/错误情况：无。
   */
  function openCreateDialog() {
    setEditingName("");
    setForm({ name: "", ip: "", mac: "", policy: "proxy" });
    setDialogOpen(true);
  }

  /**
   * openEditDialog 用现有设备值填充编辑对话框（名称锁定，后端不支持改名）。
   * 参数说明：device 为设备表中的完整条目。返回值说明：无。
   * 可能的异常/错误情况：无。
   */
  function openEditDialog(device) {
    setEditingName(device.name);
    setForm({ name: device.name, ip: device.ip, mac: device.mac || "", policy: device.policy || "proxy" });
    setDialogOpen(true);
  }

  /**
   * submitDeviceDialog 提交设备表单；事务成功后关闭对话框。
   * 参数说明：无。
   * 返回值说明：返回 Promise<void>。
   * 可能的异常/错误情况：表单不完整本地拦截；后端校验失败保持弹窗打开。
   */
  async function submitDeviceDialog() {
    const device = { name: form.name.trim(), ip: form.ip.trim(), mac: form.mac.trim(), policy: form.policy };
    if (!device.name || !device.ip) return;
    const ok = editingName ? await updateDevice(editingName, device) : await addDevice(device);
    if (ok) setDialogOpen(false);
  }

  return (
    <div className="stack">
      <PageHeader eyebrow="网关" title="LAN 网关（旁路由）" detail="局域网设备把网关/DNS 指向本机，即可获得与控制台一致的分流能力。">
        <Button type="button" variant="outline" disabled={refreshing} onClick={reload}>
          <RefreshCw size={16} aria-hidden="true" className={refreshing ? "animate-spin" : undefined} />刷新
        </Button>
      </PageHeader>
      {error && <p className="text-sm text-destructive" role="alert">{error}</p>}
      {initialLoading && <EmptyState title="正在加载网关状态" detail="首次进入时读取模块相位与设备表。" />}

      <section className="panel p-5 space-y-4">
        <PanelTitle title="模块状态" detail="执行层由平台分治：macOS 经特权 helper 应用 pf 规则，Linux 经 nftables" />
        <div className="rounded-md border p-3 space-y-2 text-sm" role="status">
          <p>运行状态：{PHASE_LABELS[phase] || phase}
            {overview?.next_retry_at && <span className="text-muted-foreground">（下次重试 {new Date(overview.next_retry_at).toLocaleTimeString()}）</span>}
          </p>
          <p>平台：{overview?.platform || "未知"}（{overview?.supported ? "支持" : "不支持"}）</p>
          <p>IPv4 转发：{overview?.forwarding ? "已开启" : "未开启"}；转发规则：{overview?.applied ? "已应用" : "未应用"}</p>
          <p>入口端口：redir {overview?.redir_port}，tproxy {overview?.tproxy_port}（仅 Linux），DNS {overview?.dns_listen_port}（劫持 53：{overview?.dns_redirect ? "开" : "关"}）</p>
          {overview?.runner && <p className="text-muted-foreground">执行层：{overview.runner}</p>}
          {overview?.error && <p className="text-destructive">{overview.error}</p>}
        </div>
        <UISwitch
          checked={enabled}
          className="mt-0 border-0 pt-0"
          label="启用 LAN 网关（设备表非空才开始分流；停用会清除转发规则）"
          onCheckedChange={toggleEnabled}
        />
      </section>

      <section className="panel p-5 space-y-3">
        <PanelTitle title="启用前检查" detail="特权前置条件：macOS 需要 root helper，Linux 需要 setcap 能力位" />
        {precheck ? (
          <div className="rounded-md border p-3 space-y-2 text-sm" role="status">
            <p className="flex items-center gap-2">
              <StatusBadge ok={precheck.ready} text={precheck.ready ? "前置条件已就绪" : "前置条件未就绪"} />
              <span className="text-muted-foreground">{precheck.platform}（{precheck.supported ? "支持" : "不支持"}）</span>
            </p>
            {precheck.detail && <p className={precheck.ready ? "text-muted-foreground" : ""}>{precheck.detail}</p>}
            {!precheck.ready && precheck.supported && precheck.platform === "darwin" && (
              <p className="text-muted-foreground">安装是本地特权操作，请在终端执行：sudo proxyd helper install</p>
            )}
          </div>
        ) : (
          <p className="text-sm text-muted-foreground">正在读取预检结果…</p>
        )}
      </section>

      <section className="panel">
        <div className="flex items-center justify-between gap-4">
          <PanelTitle title="设备登记表" detail="仅表内设备被分流；表外局域网流量不受影响" />
          <Button type="button" size="sm" className="mr-4" onClick={openCreateDialog}><Plus size={15} aria-hidden="true" />登记设备</Button>
        </div>
        {devices.length === 0 ? (
          <EmptyState title="设备表为空" detail="登记设备并把它的网关/DNS 指向本机后，该设备的流量开始分流。" />
        ) : (
          <ul className="item-list">
            {devices.map((device) => (
              <li className="group-item" key={device.name}>
                <div className="group-row">
                  <b>{device.name}</b>
                  <span>{device.ip}</span>
                  <span className="text-muted-foreground">{device.mac || "-"}</span>
                  <StatusBadge ok text={policyLabel(device.policy)} />
                  <Button aria-label={`编辑设备 ${device.name}`} size="icon" variant="ghost" type="button" onClick={() => openEditDialog(device)}><Pencil size={15} aria-hidden="true" /></Button>
                  <Button aria-label={`删除设备 ${device.name}`} size="icon" variant="destructive-ghost" type="button" onClick={() => removeDevice(device.name)}><Trash2 size={16} aria-hidden="true" /></Button>
                </div>
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className="panel p-5 space-y-2">
        <PanelTitle title="使用指引" detail="旁路由不接管 DHCP，设备指向错误只影响该设备本身" />
        <ol className="list-decimal space-y-1 pl-5 text-sm text-muted-foreground">
          <li>在上方登记设备（填写它的局域网 IPv4 地址与出口策略）。</li>
          <li>在该设备的网络设置中，把「网关/路由器」改成本机的局域网 IP。</li>
          <li>开启 DNS 劫持时，把设备的 DNS 也改成本机 IP（下游 53 端口会被转发到本机 {overview?.dns_listen_port ?? 1053}）。</li>
          <li>macOS 上 UDP 阶段一仅支持 DNS 劫持 + 直连，QUIC/DoH 可能仍走直连；需要完整 UDP 分流请使用 Linux。</li>
          <li>主进程异常退出后，helper 看门狗约 90 秒自动清除转发规则，下游设备恢复直连。</li>
        </ol>
      </section>

      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="group-dialog">
          <DialogClose className="dialog-close" aria-label="关闭设备对话框"><X size={16} aria-hidden="true" /></DialogClose>
          <DialogHeader>
            <DialogTitle>{editingName ? "编辑网关设备" : "登记网关设备"}</DialogTitle>
            <DialogDescription>IP 是分流匹配依据（IPv4 或 /32 CIDR）；MAC 仅用于识别展示。编辑时名称保持不变。</DialogDescription>
          </DialogHeader>
          <div className="form-grid">
            <Field label="设备名称"><input disabled={Boolean(editingName)} value={form.name} onChange={(event) => setForm((current) => ({ ...current, name: event.target.value }))} placeholder="例如：客厅电视" /></Field>
            <Field label="IPv4 地址"><input value={form.ip} onChange={(event) => setForm((current) => ({ ...current, ip: event.target.value }))} placeholder="例如：192.168.1.10" /></Field>
            <Field label="MAC（可选）"><input value={form.mac} onChange={(event) => setForm((current) => ({ ...current, mac: event.target.value }))} placeholder="aa:bb:cc:dd:ee:ff" /></Field>
            <Field label="出口策略">
              <Select
                ariaLabel="设备出口策略"
                value={form.policy}
                onValueChange={(value) => setForm((current) => ({ ...current, policy: value }))}
                options={policyOptions}
              />
            </Field>
          </div>
          <DialogFooter>
            <Button variant="outline" type="button" onClick={() => setDialogOpen(false)}>取消</Button>
            <Button type="button" disabled={!form.name.trim() || !form.ip.trim()} onClick={submitDeviceDialog}>
              {editingName ? <Pencil size={16} aria-hidden="true" /> : <Plus size={16} aria-hidden="true" />}
              {editingName ? "保存修改" : "登记设备"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
