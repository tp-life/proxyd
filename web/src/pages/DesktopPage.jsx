/**
 * 独立远程桌面管理页面。
 *
 * 功能说明：
 * 服务端区域负责检测操作系统 RDP/VNC 真实监听端口并控制 tailcat 暴露；客户端区域
 * 负责保存连接档案、创建临时回环转发和唤起系统桌面客户端。页面不管理密码，也不
 * 重复管理远端 token，后者始终归属“远程连接”模块。
 *
 * 可能的异常/错误情况：
 * 浏览器与 proxyd 不在同一台机器时，系统客户端无法访问由守护进程绑定的 127.0.0.1
 * 地址；页面会展示明确警告，但仍允许管理员查看和维护配置。
 */
import React, { useEffect, useMemo, useState } from "react";
import {
  CircleAlert,
  Copy,
  Laptop,
  Link2,
  Monitor,
  Pencil,
  Plus,
  RefreshCw,
  Server,
  Square,
  Trash2,
} from "lucide-react";
import { EmptyState } from "@/components/EmptyState";
import { Field } from "@/components/Field";
import { PageHeader } from "@/components/PageHeader";
import { PanelTitle } from "@/components/PanelTitle";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Select } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

const desktopTabStorageKey = "proxyd.desktop.activeTab";
const protocolOptions = [
  { value: "rdp", label: "RDP（Windows / Linux）" },
  { value: "vnc", label: "VNC（macOS / 通用）" },
];

/**
 * readDesktopTab 恢复用户上次查看的桌面页签。
 *
 * 参数说明：无。
 * 返回值说明：server 或 client；未知值回退 server。
 * 可能的异常/错误情况：localStorage 在隐私模式下不可用时静默回退。
 */
function readDesktopTab() {
  try {
    return localStorage.getItem(desktopTabStorageKey) === "client" ? "client" : "server";
  } catch {
    return "server";
  }
}

/**
 * emptyConnectionForm 创建新增档案的初始表单。
 *
 * 参数说明：remote 为可选的首个远端名称。
 * 返回值说明：返回独立表单对象，默认使用 RDP 3389。
 * 可能的异常/错误情况：无；远端为空时提交按钮会保持禁用。
 */
function emptyConnectionForm(remote = "") {
  return { name: "", remote, protocol: "rdp", remote_port: "3389", username: "" };
}

/**
 * protocolDefaultPort 返回页面选择协议时建议的默认端口。
 *
 * 参数说明：protocol 为 rdp 或 vnc。
 * 返回值说明：VNC 返回 5900，其余值保守返回 3389。
 * 可能的异常/错误情况：无；后端仍会做最终协议与范围校验。
 */
function protocolDefaultPort(protocol) {
  return protocol === "vnc" ? 5900 : 3389;
}

/**
 * formatSessionTime 把 RFC3339 会话时间转换为本地短时间。
 *
 * 参数说明：value 为后端时间字符串。
 * 返回值说明：有效值返回 zh-CN 本地时间，无效或空值返回“-”。
 * 可能的异常/错误情况：Date 无法解析时不会抛错，而是回退“-”。
 */
function formatSessionTime(value) {
  const date = new Date(value);
  return value && !Number.isNaN(date.getTime())
    ? date.toLocaleString("zh-CN", { hour12: false })
    : "-";
}

/**
 * DesktopPage 渲染远程桌面服务端与客户端管理页面。
 *
 * 参数说明：
 * - status/remotes/loading/refreshing/busy/error: useDesktopFeed 提供的页面状态。
 * - reload/saveService/saveConnection/deleteConnection: 配置读取与持久化操作。
 * - startSession/relaunchSession/stopSession: 临时桌面会话操作。
 * - copyText: 安全复制本地地址。
 * - onNavigateRemote: 跳转到远程连接页，用于补齐前置的 token/远端配置。
 *
 * 返回值说明：返回完整 React 页面。
 *
 * 可能的异常/错误情况：所有异步操作均由 Hook toast；页面仅在成功后清空编辑表单。
 */
export function DesktopPage({
  status,
  remotes = [],
  loading,
  refreshing,
  busy,
  error,
  reload,
  saveService,
  saveConnection,
  deleteConnection,
  startSession,
  relaunchSession,
  stopSession,
  copyText,
  onNavigateRemote,
}) {
  const [activeTab, setActiveTab] = useState(readDesktopTab);
  const [servicePorts, setServicePorts] = useState({});
  const [editingName, setEditingName] = useState("");
  const [showForm, setShowForm] = useState(false);
  const [form, setForm] = useState(() => emptyConnectionForm(remotes[0]?.name || ""));

  const remoteOptions = useMemo(
    () => remotes.map((remote) => ({ value: remote.name, label: remote.name })),
    [remotes],
  );
  const sessionsByConnection = useMemo(
    () => new Map((status?.sessions || []).map((session) => [session.connection_name, session])),
    [status?.sessions],
  );
  /*
   * 删除或重命名档案不会强制中断既有会话，这是避免编辑配置突然切断用户桌面的
   * 领域规则。这里单独筛出失去档案引用的会话，确保它们仍有显式断开入口。
   */
  const orphanSessions = useMemo(() => {
    const connectionNames = new Set((status?.connections || []).map((connection) => connection.name));
    return (status?.sessions || []).filter((session) => !connectionNames.has(session.connection_name));
  }, [status?.connections, status?.sessions]);

  useEffect(() => {
    const nextPorts = {};
    for (const service of status?.services || []) nextPorts[service.protocol] = String(service.port);
    setServicePorts(nextPorts);
  }, [status?.services]);

  useEffect(() => {
    if (!form.remote && remoteOptions.length > 0) {
      setForm((current) => ({ ...current, remote: remoteOptions[0].value }));
    }
  }, [form.remote, remoteOptions]);

  /**
   * handleTabChange 切换并记住服务端/客户端视图。
   *
   * 参数说明：next 为 server 或 client。
   * 返回值说明：无。
   * 可能的异常/错误情况：localStorage 写入失败时仅本次会话生效。
   */
  function handleTabChange(next) {
    setActiveTab(next);
    try {
      localStorage.setItem(desktopTabStorageKey, next);
    } catch {
      // 浏览器禁用持久存储时不影响当前页签切换。
    }
  }

  /**
   * updateFormField 更新连接档案表单的一个受控字段。
   *
   * 参数说明：field 为字段名；value 为输入值。
   * 返回值说明：无。
   * 可能的异常/错误情况：无；字段白名单由调用点固定，后端负责最终校验。
   */
  function updateFormField(field, value) {
    setForm((current) => ({ ...current, [field]: value }));
  }

  /**
   * handleProtocolChange 切换协议并同步建议端口。
   *
   * 参数说明：protocol 为 rdp 或 vnc。
   * 返回值说明：无。
   * 可能的异常/错误情况：无；这是建议值，用户之后仍可填写服务实际端口。
   */
  function handleProtocolChange(protocol) {
    setForm((current) => ({
      ...current,
      protocol,
      remote_port: String(protocolDefaultPort(protocol)),
    }));
  }

  /**
   * beginAddConnection 打开空白新增表单。
   *
   * 参数说明：无。
   * 返回值说明：无。
   * 可能的异常/错误情况：无；没有远端时仍展示表单，但提交按钮禁用并提示前置步骤。
   */
  function beginAddConnection() {
    setEditingName("");
    setForm(emptyConnectionForm(remoteOptions[0]?.value || ""));
    setShowForm(true);
  }

  /**
   * beginEditConnection 把已保存档案载入编辑表单。
   *
   * 参数说明：connection 为 status.connections 中的对象。
   * 返回值说明：无。
   * 可能的异常/错误情况：无；后端已保证档案字段合法。
   */
  function beginEditConnection(connection) {
    setEditingName(connection.name);
    setForm({
      name: connection.name,
      remote: connection.remote,
      protocol: connection.protocol,
      remote_port: String(connection.remote_port),
      username: connection.username || "",
    });
    setShowForm(true);
  }

  /**
   * cancelConnectionForm 放弃当前未提交编辑。
   *
   * 参数说明：无。
   * 返回值说明：无。
   * 可能的异常/错误情况：无；服务端已保存数据不会被改变。
   */
  function cancelConnectionForm() {
    setShowForm(false);
    setEditingName("");
  }

  /**
   * submitConnection 规范化并提交连接档案。
   *
   * 参数说明：event 为 React 表单提交事件。
   * 返回值说明：返回 Promise<void>；成功关闭表单，失败保留输入便于修正。
   * 可能的异常/错误情况：空字段和端口非法由后端/Hook提示；密码字段不存在，因此不会
   * 意外进入配置文件。
   */
  async function submitConnection(event) {
    event.preventDefault();
    const saved = await saveConnection({
      name: form.name.trim(),
      remote: form.remote,
      protocol: form.protocol,
      remote_port: Number(form.remote_port),
      username: form.username.trim(),
    }, editingName);
    if (saved) cancelConnectionForm();
  }

  /**
   * saveServicePort 保存端口但维持当前暴露状态。
   *
   * 参数说明：service 为后端服务快照。
   * 返回值说明：返回 Promise<boolean>。
   * 可能的异常/错误情况：端口范围、协议冲突或落盘失败由 Hook/后端提示并回滚。
   */
  async function saveServicePort(service) {
    return saveService(service.protocol, servicePorts[service.protocol], service.exposed);
  }

  /**
   * toggleServiceExposure 保存当前输入端口并切换隧道暴露状态。
   *
   * 参数说明：service 是当前协议状态；exposed 是目标开关值。
   * 返回值说明：返回 Promise<boolean>。
   * 可能的异常/错误情况：若远程连接服务未开启，端口仍可写入暴露列表但不会实际可达；
   * 页面保留全局警告，引导用户先开启隧道服务。
   */
  async function toggleServiceExposure(service, exposed) {
    return saveService(service.protocol, servicePorts[service.protocol], exposed);
  }

  const initialLoading = loading && !status;
  const formReady = Boolean(form.name.trim() && form.remote && form.protocol && form.remote_port);

  return (
    <div className="stack">
      <PageHeader
        eyebrow="远程访问"
        title="远程桌面"
        detail="独立管理桌面服务开放状态和常用 RDP/VNC 连接；数据通道复用 tailcat 加密隧道。"
      >
        <Button disabled={loading || refreshing} loading={refreshing} variant="outline" type="button" onClick={() => reload(false)}>
          {!refreshing && <RefreshCw size={16} aria-hidden="true" />}
          <span>刷新状态</span>
        </Button>
      </PageHeader>

      {error && (
        <div className="notice-row">
          <CircleAlert size={16} aria-hidden="true" />
          <span className="min-w-0 flex-1">远程桌面状态加载失败：{error}</span>
          <Button size="sm" variant="outline" type="button" onClick={() => reload(false)}>重试</Button>
        </div>
      )}

      <section className="panel compact-panel">
        <PanelTitle title="连接方式" detail="tailcat 先通过 DERP 完成发现，再尝试 NAT 穿透建立 WireGuard 点对点通道；无法直连时自动回退 DERP 中继。" />
        <p className="permission-note ok">proxyd 只转发桌面协议流量，不绕过 Windows、macOS 或 VNC 服务自身的账号与密码认证。</p>
      </section>

      {initialLoading ? (
        <EmptyState title="正在加载远程桌面状态" detail="正在检测本机桌面服务并读取保存的连接。" />
      ) : (
        <Tabs onValueChange={handleTabChange} value={activeTab}>
          {/*
           * 远程桌面与远程连接都以“本机扮演的连接角色”划分内容，因此复用相同的
           * 角色导航视觉。独立标题、方向标签与图标共同强调这里是一级分类入口，
           * 避免用户把“服务端 / 客户端”误认为下面面板中的普通文字标题。
           */}
          <nav className="remote-role-navigation" aria-labelledby="desktop-role-navigation-title">
            <div className="remote-role-navigation-heading">
              <div>
                <span className="remote-role-navigation-kicker">分类导航</span>
                <strong id="desktop-role-navigation-title">选择桌面角色</strong>
              </div>
              <span>本机可以共享桌面或连接远程桌面</span>
            </div>
            <TabsList className="remote-role-tabs-list" aria-label="远程桌面角色">
              <TabsTrigger className="remote-role-tab" value="server">
                <span className="remote-role-tab-heading">
                  <span className="remote-role-tab-icon"><Server size={18} aria-hidden="true" /></span>
                  <span className="tabs-trigger-title">服务端</span>
                  <span className="remote-role-tab-direction">接受连接</span>
                </span>
                <span className="tabs-trigger-detail">让别人连接本机：检测系统桌面服务、配置实际端口并控制隧道开放</span>
              </TabsTrigger>
              <TabsTrigger className="remote-role-tab" value="client">
                <span className="remote-role-tab-heading">
                  <span className="remote-role-tab-icon"><Laptop size={18} aria-hidden="true" /></span>
                  <span className="tabs-trigger-title">客户端</span>
                  <span className="remote-role-tab-direction">发起连接</span>
                </span>
                <span className="tabs-trigger-detail">从本机连接其他设备：保存常用连接、按需建立临时转发并打开系统客户端</span>
              </TabsTrigger>
            </TabsList>
          </nav>

          <TabsContent forceMount value="server">
            <div className="grid gap-4">
              {!status?.remote_enabled && (
                <div className="notice-row">
                  <CircleAlert size={16} aria-hidden="true" />
                  <span className="min-w-0 flex-1">远程连接服务尚未开启。这里可以先保存端口，但对端暂时无法访问。</span>
                  <Button size="sm" variant="outline" type="button" onClick={onNavigateRemote}>前往远程连接</Button>
                </div>
              )}

              <section className="panel">
                <PanelTitle
                  title="本机桌面服务"
                  detail="先在操作系统中启用桌面服务，再开放它真实监听的端口；proxyd 不会替你修改系统账号或防火墙策略。"
                  help={{
                    heading: "推荐配置",
                    paragraphs: ["Windows 通常使用 RDP 3389；macOS 屏幕共享和多数 VNC 服务通常使用 5900。若系统改过端口，请填写真实值。"],
                    items: [
                      "“服务已监听”表示 proxyd 能在本机回环地址完成 TCP 握手",
                      "“隧道已开放”表示该端口已经加入 remote.serve",
                      "只有远程连接服务运行、系统服务监听且端口开放时，对端才具备连接条件",
                    ],
                  }}
                />

                <div className="grid gap-4 lg:grid-cols-2">
                  {(status?.services || []).map((service) => {
                    const serviceBusy = busy === `service:${service.protocol}`;
                    const portChanged = servicePorts[service.protocol] !== String(service.port);
                    return (
                      <article className="grid gap-4 rounded-lg border bg-card p-4" key={service.protocol}>
                        <header className="flex flex-wrap items-start justify-between gap-3">
                          <div className="flex min-w-0 items-start gap-3">
                            <span className="grid size-10 shrink-0 place-items-center rounded-lg border bg-muted text-primary">
                              {service.protocol === "rdp" ? <Monitor size={20} aria-hidden="true" /> : <Server size={20} aria-hidden="true" />}
                            </span>
                            <div className="min-w-0">
                              <h3 className="m-0 text-sm font-semibold">{service.name}</h3>
                              <p className="m-0 mt-1 text-xs text-muted-foreground">推荐端口 {service.default_port} · 当前配置 {service.port}</p>
                            </div>
                          </div>
                          <div className="flex flex-wrap gap-2">
                            <Badge variant={service.listening ? "success" : "destructive"}>{service.listening ? "服务已监听" : "未检测到监听"}</Badge>
                            <Badge variant={service.exposed ? "success" : "outline"}>{service.exposed ? "隧道已开放" : "隧道未开放"}</Badge>
                          </div>
                        </header>

                        <Field label="系统服务端口" hint={`填写操作系统中 ${service.protocol.toUpperCase()} 服务真实监听的 TCP 端口`}>
                          <input
                            aria-label={`${service.name} 服务端口`}
                            min="1"
                            max="65535"
                            type="number"
                            value={servicePorts[service.protocol] || ""}
                            onChange={(event) => setServicePorts((current) => ({ ...current, [service.protocol]: event.target.value }))}
                          />
                        </Field>

                        <div className="flex flex-wrap items-center justify-between gap-3 border-t pt-3">
                          <Switch
                            checked={service.exposed}
                            disabled={serviceBusy}
                            label="允许对端经隧道访问"
                            onCheckedChange={(checked) => toggleServiceExposure(service, checked)}
                          />
                          <Button disabled={!portChanged} loading={serviceBusy} size="sm" type="button" onClick={() => saveServicePort(service)}>
                            保存端口
                          </Button>
                        </div>
                      </article>
                    );
                  })}
                </div>
              </section>
            </div>
          </TabsContent>

          <TabsContent forceMount value="client">
            <div className="grid gap-4">
              {status?.api_loopback === false && (
                <div className="notice-row">
                  <CircleAlert size={16} aria-hidden="true" />
                  <span>当前 API 允许非本机访问。一键打开只在浏览器与 proxyd 位于同一台机器时有效，因为临时转发只监听 proxyd 主机的 127.0.0.1。</span>
                </div>
              )}

              <section className="panel">
                <div className="flex flex-wrap items-start justify-between gap-3">
                  <PanelTitle title="我的桌面连接" detail="档案写入 proxyd 配置；只保存远端引用、协议、端口和可选用户名，密码由系统客户端管理。" />
                  <Button disabled={remoteOptions.length === 0} type="button" onClick={beginAddConnection}>
                    <Plus size={16} aria-hidden="true" />
                    <span>添加连接</span>
                  </Button>
                </div>

                {remoteOptions.length === 0 && (
                  <div className="mt-4 rounded-lg border border-dashed p-4 text-sm text-muted-foreground">
                    <p className="m-0">还没有可用的远程设备。请先在“远程连接 → 客户端”中保存对端 token。</p>
                    <Button className="mt-3" size="sm" variant="outline" type="button" onClick={onNavigateRemote}>添加远程设备</Button>
                  </div>
                )}

                {showForm && (
                  <form className="mt-4 grid gap-4 rounded-lg border bg-muted/25 p-4" onSubmit={submitConnection}>
                    <div className="flex items-center justify-between gap-3">
                      <h3 className="m-0 text-sm font-semibold">{editingName ? `编辑：${editingName}` : "添加桌面连接"}</h3>
                      <Badge variant="outline">不保存密码</Badge>
                    </div>
                    <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-5">
                      <Field label="连接名称" hint="用于列表识别，可修改">
                        <input aria-label="桌面连接名称" value={form.name} onChange={(event) => updateFormField("name", event.target.value)} placeholder="例如：办公室 Windows" />
                      </Field>
                      <Field label="远程设备" hint="来自远程连接模块">
                        <Select ariaLabel="远程设备" value={form.remote} onValueChange={(value) => updateFormField("remote", value)} options={remoteOptions} />
                      </Field>
                      <Field label="桌面协议">
                        <Select ariaLabel="桌面协议" value={form.protocol} onValueChange={handleProtocolChange} options={protocolOptions} />
                      </Field>
                      <Field label="远端端口" hint={`建议 ${protocolDefaultPort(form.protocol)}`}>
                        <input aria-label="桌面远端端口" min="1" max="65535" type="number" value={form.remote_port} onChange={(event) => updateFormField("remote_port", event.target.value)} />
                      </Field>
                      <Field label="登录用户名" hint="可选，不保存密码">
                        <input aria-label="桌面登录用户名" value={form.username} onChange={(event) => updateFormField("username", event.target.value)} placeholder="例如：DOMAIN\\user" />
                      </Field>
                    </div>
                    <div className="flex justify-end gap-2">
                      <Button variant="outline" type="button" onClick={cancelConnectionForm}>取消</Button>
                      <Button disabled={!formReady} loading={busy.startsWith("connection:")} type="submit">{editingName ? "保存修改" : "保存连接"}</Button>
                    </div>
                  </form>
                )}

                {(status?.connections || []).length === 0 ? (
                  <div className="mt-4">
                    <EmptyState compact title="尚未保存桌面连接" detail="添加连接后，可以一键建立临时隧道并打开系统 RDP/VNC 客户端。" />
                  </div>
                ) : (
                  <div className="mt-4 grid gap-3">
                    {(status?.connections || []).map((connection) => {
                      const session = sessionsByConnection.get(connection.name);
                      const startBusy = busy === `start:${connection.name}`;
                      const stopBusy = session && busy === `stop:${session.id}`;
                      return (
                        <article className="grid gap-3 rounded-lg border bg-card p-4" key={connection.name}>
                          <div className="flex flex-wrap items-start justify-between gap-3">
                            <div className="flex min-w-0 items-start gap-3">
                              <span className="grid size-10 shrink-0 place-items-center rounded-lg border bg-muted text-primary">
                                <Laptop size={20} aria-hidden="true" />
                              </span>
                              <div className="min-w-0">
                                <div className="flex flex-wrap items-center gap-2">
                                  <h3 className="m-0 text-sm font-semibold">{connection.name}</h3>
                                  <Badge variant="outline">{connection.protocol.toUpperCase()}</Badge>
                                  {session && <Badge variant="success">会话已建立</Badge>}
                                </div>
                                <p className="m-0 mt-1 text-xs text-muted-foreground">
                                  {connection.remote} · 远端端口 {connection.remote_port}{connection.username ? ` · 用户 ${connection.username}` : ""}
                                </p>
                              </div>
                            </div>
                            <div className="flex flex-wrap items-center gap-2">
                              {session ? (
                                <>
                                  <Button size="sm" type="button" onClick={() => relaunchSession(session)}>
                                    <Link2 size={14} aria-hidden="true" />
                                    <span>再次打开</span>
                                  </Button>
                                  <Button loading={stopBusy} size="sm" variant="outline" type="button" onClick={() => stopSession(session.id)}>
                                    {!stopBusy && <Square size={13} aria-hidden="true" />}
                                    <span>断开</span>
                                  </Button>
                                </>
                              ) : (
                                <Button loading={startBusy} size="sm" type="button" onClick={() => startSession(connection.name)}>
                                  {!startBusy && <Link2 size={14} aria-hidden="true" />}
                                  <span>连接</span>
                                </Button>
                              )}
                              <Button aria-label={`编辑桌面连接 ${connection.name}`} size="icon" variant="outline" type="button" onClick={() => beginEditConnection(connection)}>
                                <Pencil size={15} aria-hidden="true" />
                              </Button>
                              <Button aria-label={`删除桌面连接 ${connection.name}`} loading={busy === `delete:${connection.name}`} size="icon" variant="destructive-ghost" type="button" onClick={() => deleteConnection(connection.name)}>
                                <Trash2 size={15} aria-hidden="true" />
                              </Button>
                            </div>
                          </div>

                          {session && (
                            <div className="flex flex-wrap items-center gap-x-4 gap-y-2 rounded-md border bg-muted/30 px-3 py-2 text-xs text-muted-foreground">
                              <span>本地地址 <code className="font-mono text-foreground">{session.local_address}</code></span>
                              <span>活动连接 {session.active_connections}</span>
                              <span>启动于 {formatSessionTime(session.started_at)}</span>
                              <Button size="sm" variant="ghost" type="button" onClick={() => copyText(session.local_address, "本地桌面地址已复制")}>
                                <Copy size={13} aria-hidden="true" />
                                <span>复制地址</span>
                              </Button>
                            </div>
                          )}
                        </article>
                      );
                    })}
                  </div>
                )}
              </section>

              {orphanSessions.length > 0 && (
                <section className="panel">
                  <PanelTitle title="未关联的活动会话" detail="对应档案已被删除或重命名；会话按启动快照继续运行，可在这里再次打开或立即断开。" />
                  <div className="mt-4 grid gap-3">
                    {orphanSessions.map((session) => {
                      const stopBusy = busy === `stop:${session.id}`;
                      return (
                        <article className="flex flex-wrap items-center justify-between gap-3 rounded-lg border bg-card p-4" key={session.id}>
                          <div className="min-w-0">
                            <div className="flex flex-wrap items-center gap-2">
                              <h3 className="m-0 text-sm font-semibold">{session.connection_name}</h3>
                              <Badge variant="outline">{session.protocol.toUpperCase()}</Badge>
                              <Badge variant="success">会话仍在运行</Badge>
                            </div>
                            <p className="m-0 mt-1 text-xs text-muted-foreground">
                              本地地址 <code className="font-mono text-foreground">{session.local_address}</code> · 启动于 {formatSessionTime(session.started_at)}
                            </p>
                          </div>
                          <div className="flex flex-wrap gap-2">
                            <Button size="sm" variant="outline" type="button" onClick={() => copyText(session.local_address, "本地桌面地址已复制")}>
                              <Copy size={13} aria-hidden="true" />
                              <span>复制地址</span>
                            </Button>
                            <Button size="sm" type="button" onClick={() => relaunchSession(session)}>再次打开</Button>
                            <Button loading={stopBusy} size="sm" variant="outline" type="button" onClick={() => stopSession(session.id)}>
                              {!stopBusy && <Square size={13} aria-hidden="true" />}
                              <span>断开</span>
                            </Button>
                          </div>
                        </article>
                      );
                    })}
                  </div>
                </section>
              )}
            </div>
          </TabsContent>
        </Tabs>
      )}
    </div>
  );
}
