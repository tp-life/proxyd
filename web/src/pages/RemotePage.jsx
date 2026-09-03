import { useEffect, useMemo, useState } from "react";
import { CircleAlert, Copy, Link2, Plus, RefreshCw, Terminal, Trash2, X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Table } from "@/components/ui/data-table";
import { Dialog, DialogClose, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Select } from "@/components/ui/select";
import { Switch as UISwitch } from "@/components/ui/switch";
import { EmptyState } from "@/components/EmptyState";
import { Field } from "@/components/Field";
import { PageHeader } from "@/components/PageHeader";
import { PanelTitle } from "@/components/PanelTitle";
import { StatusBadge } from "@/components/StatusBadge";
import { classNames, maskRemoteSecret, tableViewportHeight } from "@/lib/format";

/**
 * RemotePage 渲染远程连接页。
 *
 * 功能说明：
 * 该页管理基于 tailcat 隧道的远程访问模块，与代理功能相互独立。内容按角色分两组：
 * 「服务端」（让别人连入本机：服务状态卡——开关、运行状态、本机 token、DERP 区域、
 * 客户端公钥白名单、临时身份——以及暴露端口管理）与「客户端」（从本机连到别的机器：
 * 远程设备列表与本地转发列表）。token 一律以摘要展示，完整值只在点击复制时显式
 * 从 `/api/remote/token` 获取。
 *
 * 参数说明：
 * - status: object | null，`/api/remote` 响应（enabled/running/error/token/region/serve/forwards）。
 * - remotes: Array<object>，已保存的远程设备（name 与打码 token）。
 * - loading/refreshing: boolean，首次加载与刷新状态。
 * - error: string，最近一次加载错误文本。
 * - hasLoaded: boolean，是否至少完成过一次加载尝试。
 * - reload/toggleEnabled/copyToken/saveServe: Function，状态加载与服务/端口操作。
 * - saveAllow: Function，整体替换客户端公钥白名单。
 * - saveKeyFile: Function，设置自定义服务端密钥文件（空串恢复内置托管密钥）。
 * - setBuiltinSSH: Function，热切换内嵌免密 SSH 服务（隧道 22 端口进程内处理）。
 * - resetTempKey/copyTempKey: Function，临时身份（应急 nodekey）的重置与私钥复制。
 * - addRemote/removeRemote/copySSHCommand: Function，远程设备操作。
 * - addForward/toggleForward/removeForward: Function，本地转发操作。
 * - fetchPeerToken/createSSHForward/copyText: Function，「连接」对话框的 token 获取、
 *   SSH 转发创建与命令复制。
 * - sshSetEnvTerm: boolean，统一开关：复制的所有 SSH 命令与 ssh config 是否携带
 *   SetEnv TERM=xterm-256color。
 * - setSshSetEnvTerm: Function，切换该开关（持久化到 localStorage）。
 *
 * 返回值说明：
 * 返回远程连接页 React 元素。
 *
 * 可能的异常/错误情况：
 * - 加载失败时保留旧数据并显示错误条带，用户可点击重试。
 * - 空列表时对应面板渲染空状态而非空表格。
 */
export function RemotePage({
  status,
  remotes,
  loading,
  refreshing,
  error,
  hasLoaded,
  reload,
  toggleEnabled,
  copyToken,
  saveServe,
  saveAllow,
  saveKeyFile,
  setBuiltinSSH,
  resetTempKey,
  copyTempKey,
  addRemote,
  removeRemote,
  copySSHCommand,
  addForward,
  toggleForward,
  removeForward,
  fetchPeerToken,
  createSSHForward,
  copyText,
  sshSetEnvTerm,
  setSshSetEnvTerm,
}) {
  const [serveInput, setServeInput] = useState("");
  const [allowInput, setAllowInput] = useState("");
  const [allowNameInput, setAllowNameInput] = useState("");
  const [keyFileInput, setKeyFileInput] = useState("");
  const [remoteForm, setRemoteForm] = useState({ name: "", token: "" });
  const [forwardForm, setForwardForm] = useState({ name: "", listen: "", remoteSource: "", remoteToken: "", remotePort: "" });
  const [connectTarget, setConnectTarget] = useState(null);

  const serve = status?.serve || [];
  const allow = status?.allow || [];
  const activity = status?.client_activity || {};
  const forwards = status?.forwards || [];
  const initialLoading = loading && !hasLoaded && !status;

  const remoteColumns = useMemo(
    () => [
      { key: "name", header: "名称", sortable: true, width: "30%" },
      {
        key: "token",
        header: "token 摘要",
        width: "40%",
        cell: (row) => <code className="font-mono text-xs text-muted-foreground">{maskRemoteSecret(row.token)}</code>,
      },
      {
        key: "actions",
        header: "操作",
        align: "right",
        cell: (row) => (
          <div className="flex items-center justify-end gap-1">
            <Button size="sm" variant="outline" type="button" onClick={() => setConnectTarget(row.name)}>
              <Link2 size={14} aria-hidden="true" />
              <span>连接</span>
            </Button>
            <Button size="sm" variant="outline" type="button" onClick={() => copySSHCommand(row.name)}>
              <Terminal size={14} aria-hidden="true" />
              <span>复制 SSH 命令</span>
            </Button>
            <Button aria-label={`删除远程设备 ${row.name}`} size="icon" variant="destructive-ghost" type="button" onClick={() => removeRemote(row.name)}>
              <Trash2 size={16} aria-hidden="true" />
            </Button>
          </div>
        ),
      },
    ],
    [copySSHCommand, removeRemote],
  );

  const forwardColumns = useMemo(
    () => [
      { key: "name", header: "名称", sortable: true, width: "16%" },
      {
        key: "listen",
        header: "本地监听",
        width: "17%",
        cell: (row) => (
          <div className="grid justify-items-start gap-1">
            <code className="font-mono text-xs">{row.listen}</code>
            {row.remote_port === 22 && row.enabled && (
              <button
                className="copy-link font-mono text-xs"
                title="复制 SSH 命令"
                type="button"
                onClick={() => copyText(`ssh ${parseListen(row.listen).host} -p ${parseListen(row.listen).port}${sshSetEnvSuffix(sshSetEnvTerm)}`, "SSH 命令已复制")}
              >
                ssh {parseListen(row.listen).host} -p {parseListen(row.listen).port}{sshSetEnvSuffix(sshSetEnvTerm)}<Copy size={12} aria-hidden="true" />
              </button>
            )}
          </div>
        ),
      },
      {
        key: "remote",
        header: "远端:端口",
        width: "20%",
        cell: (row) => (
          <span className="font-mono text-xs" title={row.remote?.length > 24 ? "完整 token 已省略" : undefined}>
            {maskRemoteSecret(row.remote)}:{row.remote_port}
            {row.remote_port === 22 && <Badge className="ml-1 align-middle font-sans" variant="secondary">SSH</Badge>}
          </span>
        ),
      },
      {
        key: "enabled",
        header: "启用",
        width: "76px",
        cell: (row) => (
          <UISwitch
            ariaLabel={`${row.enabled ? "停用" : "启用"}转发 ${row.name}`}
            checked={Boolean(row.enabled)}
            className="mt-0 border-0 pt-0"
            onCheckedChange={(enabled) => toggleForward(row.name, enabled)}
          />
        ),
      },
      {
        key: "active",
        header: "活动连接",
        sortable: true,
        width: "90px",
        cell: (row) => <span className="tabular-nums">{row.active ?? 0}</span>,
      },
      {
        key: "last_error",
        header: "最近错误",
        cell: (row) =>
          row.last_error
            ? <span className="break-words text-xs text-destructive">{row.last_error}</span>
            : <span className="text-muted-foreground">-</span>,
      },
      {
        key: "actions",
        header: "操作",
        align: "right",
        width: "64px",
        cell: (row) => (
          <Button aria-label={`删除本地转发 ${row.name}`} size="icon" variant="destructive-ghost" type="button" onClick={() => removeForward(row.name)}>
            <Trash2 size={16} aria-hidden="true" />
          </Button>
        ),
      },
    ],
    [copyText, removeForward, sshSetEnvTerm, toggleForward],
  );

  /**
   * submitServePort 把输入端口并入列表并整体提交。
   *
   * 参数说明：event 为表单提交事件。
   * 返回值说明：返回 Promise<void>。
   * 可能的异常/错误情况：端口非法或已存在时本地拦截；后端校验失败由 saveServe toast。
   */
  async function submitServePort(event) {
    event.preventDefault();
    const port = Number.parseInt(serveInput, 10);
    if (!Number.isInteger(port) || port < 1 || port > 65535) {
      return;
    }
    if (serve.includes(port)) {
      setServeInput("");
      return;
    }
    if (await saveServe([...serve, port].sort((a, b) => a - b))) {
      setServeInput("");
    }
  }

  /**
   * removeServePort 从列表中移除端口并整体提交。
   *
   * 参数说明：port 为待移除端口号。
   * 返回值说明：返回 Promise<void>。
   * 可能的异常/错误情况：后端校验失败由 saveServe toast。
   */
  async function removeServePort(port) {
    await saveServe(serve.filter((item) => item !== port));
  }

  /**
   * submitAllowKey 校验并把输入的客户端公钥并入白名单后整体提交。
   *
   * 参数说明：event 为表单提交事件。
   * 返回值说明：返回 Promise<void>。
   * 可能的异常/错误情况：格式非法（非 nodekey: 前缀）或已存在时本地拦截；后端校验失败由 saveAllow toast。
   */
  async function submitAllowKey(event) {
    event.preventDefault();
    const key = allowInput.trim();
    const name = allowNameInput.trim();
    if (!key.startsWith("nodekey:") || allow.some((entry) => entry.key === key)) {
      return;
    }
    if (name && allow.some((entry) => entry.name === name)) {
      return;
    }
    if (await saveAllow([...allow, { name, key }])) {
      setAllowInput("");
      setAllowNameInput("");
    }
  }

  /**
   * removeAllowKey 从白名单移除条目并整体提交；删空后恢复放行所有客户端。
   *
   * 参数说明：key 为待移除条目的公钥。
   * 返回值说明：返回 Promise<void>。
   * 可能的异常/错误情况：后端校验失败由 saveAllow toast。
   */
  async function removeAllowKey(key) {
    await saveAllow(allow.filter((item) => item.key !== key));
  }

  /**
   * submitKeyFile 提交自定义服务端密钥文件路径；空输入恢复内置托管密钥。
   *
   * 参数说明：event 为表单提交事件。
   * 返回值说明：返回 Promise<void>。
   * 可能的异常/错误情况：后端校验失败（文件已存在但非合法密钥）由 saveKeyFile toast。
   */
  async function submitKeyFile(event) {
    event.preventDefault();
    if (await saveKeyFile(keyFileInput.trim())) {
      setKeyFileInput("");
    }
  }

  /**
   * openSSHPort 一键把 22 端口并入暴露列表，供对端经隧道 SSH 登录本机。
   *
   * 参数说明：无。
   * 返回值说明：返回 Promise<void>。
   * 可能的异常/错误情况：后端校验失败由 saveServe toast；22 已在列表时不重复提交。
   */
  async function openSSHPort() {
    if (serve.includes(22)) {
      return;
    }
    await saveServe([...serve, 22].sort((a, b) => a - b));
  }

  /**
   * submitRemote 校验并提交新增远程设备表单。
   *
   * 参数说明：event 为表单提交事件。
   * 返回值说明：返回 Promise<void>。
   * 可能的异常/错误情况：缺字段时本地拦截；后端 400 由 addRemote toast。
   */
  async function submitRemote(event) {
    event.preventDefault();
    if (!remoteForm.name.trim() || !remoteForm.token.trim()) {
      return;
    }
    if (await addRemote(remoteForm.name.trim(), remoteForm.token.trim())) {
      setRemoteForm({ name: "", token: "" });
    }
  }

  /**
   * submitForward 校验并提交新增本地转发表单。
   *
   * 参数说明：event 为表单提交事件。
   * 返回值说明：返回 Promise<void>。
   * 可能的异常/错误情况：缺字段或端口非法由 addForward toast；成功后清空表单。
   */
  async function submitForward(event) {
    event.preventDefault();
    const saved = await addForward({
      name: forwardForm.name,
      listen: forwardForm.listen,
      remote: forwardForm.remoteSource || forwardForm.remoteToken,
      remotePort: forwardForm.remotePort,
    });
    if (saved) {
      setForwardForm({ name: "", listen: "", remoteSource: "", remoteToken: "", remotePort: "" });
    }
  }

  return (
    <div className="stack">
      <PageHeader eyebrow="远程" title="远程连接" detail="基于 tailcat 隧道的端到端加密远程访问，与代理功能相互独立。token 即连接凭据，请注意保密。">
        <Button disabled={loading} loading={refreshing} type="button" variant="outline" onClick={reload} aria-label="刷新远程连接状态">
          <RefreshCw className={classNames(refreshing && "animate-spin")} size={16} aria-hidden="true" />
          <span>刷新</span>
        </Button>
      </PageHeader>

      {error && (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-md border border-warning/30 bg-warning-soft px-4 py-3 text-sm text-warning">
          <div className="flex min-w-0 items-start gap-2">
            <CircleAlert size={16} aria-hidden="true" />
            <span className="min-w-0 break-words">{error}</span>
          </div>
          <Button className="h-11" type="button" variant="outline" onClick={reload} aria-label="重试加载远程连接状态">
            <RefreshCw size={16} aria-hidden="true" />
            <span>重试</span>
          </Button>
        </div>
      )}

      <section className="panel">
        <PanelTitle title="快速上手" detail="两台机器之间互访 SSH/scp 只需两步：对端配置「服务端」，本机配置「客户端」" />
        <ol className="m-0 grid list-decimal gap-1 pl-5 text-sm text-muted-foreground">
          <li>在对端机器的「服务端」区启用远程连接并开放 SSH（22 端口），复制其 token。</li>
          <li>在本机「客户端」区把 token 添加为「远程设备」，点击「连接」即可生成 ssh/scp 命令；文件传输直接走 scp。</li>
        </ol>
      </section>

      {initialLoading ? (
        <EmptyState title="正在加载远程连接状态" detail="等待 /api/remote 返回服务状态。" />
      ) : (
        <>
          <div className="remote-group">
            <h2>服务端</h2>
            <p>让别人连入本机：开启隧道服务、暴露本机端口、管理允许连入的客户端</p>
          </div>

          <section className="panel">
            <PanelTitle
              title="服务状态"
              detail="tailcat 隧道服务的开关与运行状态"
              help={{
                heading: "服务状态使用方式",
                paragraphs: ["开启后本机运行 tailcat 隧道服务端，持有本机 token 的对端可经 WireGuard 加密隧道访问「暴露端口」。"],
                items: [
                  "本机 token：复制后发给对方，对方添加为「远程设备」即可连接本机",
                  "客户端公钥：本机连接别人时的身份；对端用 tailcat serve --allow=<此公钥> 可配置白名单，只放行本机",
                  "密钥文件：决定 token 的服务端私钥；默认内置托管。若对端客户端是 tailcat 命令行，可填 tailcat genkey --key=default 生成的密钥文件路径（macOS 通常在 ~/Library/Application Support/tailcat/keys/default.private.json），两边用同一把密钥，token 即一致",
                  "内嵌免密 SSH：开启后隧道 22 端口由 proxyd 进程内 SSH 服务直接处理（与 tailcat serve no-auth-ssh 同模型），对端 proxyd ssh / tailcat ssh 即可登录本机——无需系统 sshd（macOS 远程登录）、无需账号密码，隧道密钥握手本身就是认证；注意安全：持有 token 即可获得本机 shell，建议配合「允许的客户端」白名单使用",
                  "允许的客户端：添加对端的客户端公钥后，只有列表内的机器能连入本机（token+私钥双重校验）；清空则恢复放行所有。可给每个公钥起别名方便管理（CLI：proxyd remote allow add <公钥> [别名]，del 按别名或公钥删除）",
                  "临时身份：给「客户端」使用的应急 nodekey（本机是服务端，它不是本机 token，不要填进远程设备）。公钥自动叠加进白名单；私钥复制后存密码管理器，没带电脑时在别的机器用 PROXYD_CLIENT_KEY=<私钥> 连入本机；重置只换这一对，不影响手动添加的白名单",
                ],
                note: "token 即连接凭据，泄露等于端口暴露，请只发给可信对端。",
              }}
            />
            <div className="flex flex-wrap items-center gap-3">
              <UISwitch checked={Boolean(status?.enabled)} className="mt-0 border-0 pt-0" label="启用远程连接服务" onCheckedChange={toggleEnabled} />
              {status?.running ? (
                <StatusBadge ok text="运行中" />
              ) : status?.enabled ? (
                <StatusBadge ok={false} text="配置已开启但未运行" />
              ) : (
                <Badge variant="secondary">已停止</Badge>
              )}
            </div>
            <div className="mt-2 flex flex-wrap items-center gap-2">
              <UISwitch checked={Boolean(status?.builtin_ssh)} className="mt-0 border-0 pt-0" label="内嵌免密 SSH 服务（隧道 22 端口，无需系统 sshd）" onCheckedChange={setBuiltinSSH} />
              <span className="text-xs text-muted-foreground">隧道即认证：持有 token 即可登录本机 shell，建议配合白名单</span>
            </div>
            {status?.enabled && !status?.running && status?.error && (
              <p className="permission-note warn">{status.error}</p>
            )}
            {status?.running && status?.token && (
              <div className="mt-3 flex flex-wrap items-center gap-2 rounded-md border bg-muted/40 px-3 py-2">
                <span className="text-xs font-medium text-muted-foreground">本机 token</span>
                <code className="font-mono text-sm">{status.token}</code>
                <Button size="sm" variant="outline" type="button" onClick={copyToken}>
                  <Copy size={14} aria-hidden="true" />
                  <span>复制完整 token</span>
                </Button>
              </div>
            )}
            <dl className="mt-3 grid gap-3 text-sm sm:grid-cols-2">
              <div className="grid gap-1">
                <dt className="text-xs font-medium text-muted-foreground">DERP 区域</dt>
                <dd className="text-foreground">{status?.region || "自动"}</dd>
              </div>
              <div className="grid gap-1">
                <dt className="text-xs font-medium text-muted-foreground">暴露端口</dt>
                <dd className="text-foreground">{serve.length > 0 ? serve.join("、") : "未暴露任何端口"}</dd>
              </div>
              {status?.client_key && (
                <div className="grid gap-1 sm:col-span-2">
                  <dt className="text-xs font-medium text-muted-foreground">客户端公钥（对端 --allow 白名单用）</dt>
                  <dd className="flex flex-wrap items-center gap-2">
                    <code className="min-w-0 break-all font-mono text-xs text-foreground">{status.client_key}</code>
                    <Button size="sm" variant="outline" type="button" onClick={() => copyText(status.client_key, "客户端公钥已复制")}>
                      <Copy size={14} aria-hidden="true" />
                      <span>复制</span>
                    </Button>
                  </dd>
                </div>
              )}
            </dl>
            {status?.custom_key_file && (
              <div className="mt-3 grid gap-2 rounded-md border bg-muted/40 px-3 py-2">
                <span className="text-xs font-medium text-muted-foreground">服务端密钥文件（决定 token）：当前使用自定义密钥</span>
                {status?.key_file && (
                  <code className="min-w-0 break-all font-mono text-xs text-muted-foreground">{status.key_file}</code>
                )}
                <form className="flex flex-wrap items-center gap-2" onSubmit={submitKeyFile}>
                  <input
                    aria-label="自定义服务端密钥文件路径"
                    className="mono-input min-w-0 flex-1"
                    value={keyFileInput}
                    onChange={(event) => setKeyFileInput(event.target.value)}
                    placeholder="tailcat 密钥文件路径（~/ 开头亦可），留空保存即恢复内置托管"
                  />
                  <Button size="sm" variant="outline" type="submit"><span>保存</span></Button>
                  <Button size="sm" variant="outline" type="button" onClick={() => saveKeyFile("")}>
                    <span>恢复默认</span>
                  </Button>
                </form>
                <span className="text-xs text-muted-foreground">
                  填 tailcat genkey --key=default 生成的密钥文件可让 tailcat 命令行与本服务 token 一致；切换密钥即更换身份，旧 token 立即失效。也可用 CLI：proxyd remote keyfile。
                </span>
              </div>
            )}
            <div className="mt-3 grid gap-2 rounded-md border bg-muted/40 px-3 py-2">
              <span className="text-xs font-medium text-muted-foreground">
                允许的客户端（公钥白名单）{allow.length === 0 && !status?.temp_key && "：当前放行所有持有 token 的客户端"}
                {allow.length === 0 && status?.temp_key && "：未手动添加，但下方临时身份已生效（仅临时身份可连入）"}
              </span>
              <form className="flex flex-wrap items-center gap-2" onSubmit={submitAllowKey}>
                <input
                  aria-label="允许的客户端别名（可选）"
                  className="w-28 shrink-0"
                  value={allowNameInput}
                  onChange={(event) => setAllowNameInput(event.target.value)}
                  placeholder="别名（可选）"
                />
                <input
                  aria-label="添加允许的客户端公钥"
                  className="mono-input min-w-0 flex-1"
                  value={allowInput}
                  onChange={(event) => setAllowInput(event.target.value)}
                  placeholder="nodekey:...（对端状态卡中的客户端公钥）"
                />
                <Button size="sm" variant="outline" type="submit"><Plus size={14} aria-hidden="true" /><span>添加</span></Button>
              </form>
              {allow.length > 0 && (
                <ul className="m-0 grid list-none gap-1.5 p-0">
                  {allow.map((entry) => (
                    <li key={entry.key} className="flex items-center gap-2">
                      {entry.name && <Badge variant="secondary" className="shrink-0">{entry.name}</Badge>}
                      <code className="min-w-0 flex-1 break-all font-mono text-xs">{entry.key}</code>
                      {(activity[entry.key] || 0) > 0 && <Badge variant="secondary">活动连接 {activity[entry.key]}</Badge>}
                      <button aria-label={`移出白名单 ${entry.name || entry.key.slice(0, 20)}`} className="inline-flex shrink-0 items-center text-muted-foreground hover:text-destructive" type="button" onClick={() => removeAllowKey(entry.key)}>
                        <X size={13} aria-hidden="true" />
                      </button>
                    </li>
                  ))}
                </ul>
              )}
            </div>
            <div className="mt-3 grid gap-2 rounded-md border bg-muted/40 px-3 py-2">
              <span className="text-xs font-medium text-muted-foreground">临时身份（应急 nodekey · 给客户端使用）</span>
              {status?.temp_key ? (
                <>
                  <div className="flex flex-wrap items-center gap-2">
                    <code className="min-w-0 flex-1 break-all font-mono text-xs text-foreground">{status.temp_key}</code>
                    {(activity[status.temp_key] || 0) > 0 && <Badge variant="secondary">活动连接 {activity[status.temp_key]}</Badge>}
                  </div>
                  <div className="flex flex-wrap gap-2">
                    <Button size="sm" variant="outline" type="button" onClick={() => copyText(status.temp_key, "临时身份公钥已复制")}>
                      <Copy size={14} aria-hidden="true" />
                      <span>复制公钥</span>
                    </Button>
                    <Button size="sm" variant="outline" type="button" onClick={copyTempKey}>
                      <Copy size={14} aria-hidden="true" />
                      <span>复制私钥</span>
                    </Button>
                    <Button size="sm" variant="outline" type="button" onClick={resetTempKey}>
                      <RefreshCw size={14} aria-hidden="true" />
                      <span>重置</span>
                    </Button>
                  </div>
                </>
              ) : (
                <div className="flex flex-wrap items-center gap-2">
                  <span className="min-w-0 flex-1 text-xs text-muted-foreground">
                    尚未生成（默认为空，需手动生成）。这是「客户端」身份：在别的机器上连接本机时使用，不是本机 token，也不要填进远程设备。生成后把私钥存入密码管理器：没带电脑时，在任何机器用 PROXYD_CLIENT_KEY=&lt;私钥&gt; 或 --client-key 即可连入本机。
                  </span>
                  <Button size="sm" variant="outline" type="button" onClick={resetTempKey}>
                    <Plus size={14} aria-hidden="true" />
                    <span>生成临时身份</span>
                  </Button>
                </div>
              )}
            </div>
          </section>

          <section className="panel">
            <PanelTitle
              title="暴露端口"
              detail="这些是本机端口，持有 token 的对端可经隧道访问（如 22 用于 SSH）"
              help={{
                heading: "暴露端口使用方式",
                paragraphs: ["只有列出的本机端口对隧道对端可达，未列出的端口会被隧道直接拒绝。"],
                items: [
                  "输入端口号（1-65535）后点「添加端口」，立即生效",
                  "添加 22 即允许对端 SSH 登录本机（仍需通过本机系统账号认证）",
                  "对端若用 tailcat v0.5.0+，可 tailcat serve 22 --allow=<本机客户端公钥> 进一步只放行你",
                ],
              }}
            />
            <form className="form-grid" onSubmit={submitServePort}>
              <Field label="端口">
                <input aria-label="新增暴露端口" max="65535" min="1" type="number" value={serveInput} onChange={(event) => setServeInput(event.target.value)} placeholder="例如：22（1-65535）" />
              </Field>
              <Button className="form-submit" type="submit"><Plus size={16} aria-hidden="true" /><span>添加端口</span></Button>
              {status?.enabled && !serve.includes(22) && (
                <Button className="form-submit" type="button" variant="outline" onClick={openSSHPort}>
                  <Terminal size={16} aria-hidden="true" />
                  <span>开放 SSH（22 端口）</span>
                </Button>
              )}
            </form>
            {serve.length === 0 ? (
              <EmptyState compact title="尚未暴露端口" detail="添加本机端口后，持有 token 的对端即可经隧道访问该端口。" />
            ) : (
              <ul className="m-0 flex list-none flex-wrap gap-2 p-0">
                {serve.map((port) => (
                  <li key={port}>
                    <Badge className="gap-1.5 px-2.5 py-1 font-mono text-sm" variant="secondary">
                      {port}
                      {port === 22 && <span className="font-sans text-xs text-muted-foreground">SSH</span>}
                      <button aria-label={`移除端口 ${port}`} className="inline-flex items-center" type="button" onClick={() => removeServePort(port)}>
                        <X size={13} aria-hidden="true" />
                      </button>
                    </Badge>
                  </li>
                ))}
              </ul>
            )}
          </section>

          <div className="remote-group">
            <h2>客户端</h2>
            <p>从本机连到别的机器：保存对端 token、建立本地转发、复制 SSH/scp 命令</p>
          </div>

          <section className="panel">
            <PanelTitle
              title="远程设备"
              detail="保存对端 token，供本地转发与 SSH 连接使用"
              help={{
                heading: "远程设备使用方式",
                paragraphs: ["填入名称和对端完整 token（对端状态卡或 proxyd remote token 获取）即可保存。"],
                items: [
                  "「连接」：弹出对话框，可创建本地转发后ssh 127.0.0.1，或直接复制 proxyd ssh/scp 命令",
                  "「复制 SSH 命令」：得到 proxyd ssh <名称>，终端直接粘贴使用，无需守护进程",
                  "下方「SSH 携带 TERM」统一开关控制所有复制的 ssh/proxyd ssh 命令与 ssh config 是否带 SetEnv TERM=xterm-256color（修复部分终端下回车/颜色异常；对端 sshd 未放行 TERM 时可关闭）",
                  "应急场景：proxyd remote genkey 生成一次性身份，公钥提前录入对端白名单；之后在任何机器用 PROXYD_CLIENT_KEY=<私钥> proxyd ssh <名称>（私钥不进 shell 历史）或 proxyd ssh --client-key <私钥> <名称> 连接",
                ],
                note: "token 列表只显示摘要，完整值仅在点击复制时按需获取。",
              }}
            />
            <div className="mb-3 flex flex-wrap items-center gap-2">
              <UISwitch
                checked={Boolean(sshSetEnvTerm)}
                className="mt-0 border-0 pt-0"
                label="SSH 携带 TERM 环境变量（SetEnv TERM=xterm-256color）"
                onCheckedChange={setSshSetEnvTerm}
              />
              <span className="text-xs text-muted-foreground">统一控制本页所有复制的 ssh 命令与 ssh config</span>
            </div>
            <form className="form-grid remote-form" onSubmit={submitRemote}>
              <Field label="名称">
                <input aria-label="远程设备名称" value={remoteForm.name} onChange={(event) => setRemoteForm((current) => ({ ...current, name: event.target.value }))} placeholder="例如：家里的 NAS" />
              </Field>
              <Field label="对端 token">
                <input aria-label="远程设备 token" className="mono-input" value={remoteForm.token} onChange={(event) => setRemoteForm((current) => ({ ...current, token: event.target.value }))} placeholder="tc...（对端状态卡或 proxyd remote token 获取）" />
              </Field>
              <Button className="form-submit" type="submit"><Plus size={16} aria-hidden="true" /><span>添加设备</span></Button>
            </form>
            {remotes.length === 0 ? (
              <EmptyState compact title="暂无远程设备" detail="添加对端 token 后，即可建立到该设备的本地转发或 SSH 连接。" />
            ) : (
              <Table
                className="data-table mt-3"
                columns={remoteColumns}
                data={remotes}
                emptyState="暂无远程设备"
                getRowId={(row) => row.name}
                height={tableViewportHeight(remotes.length, 480)}
                minColumnWidth={88}
                resizable
              />
            )}
          </section>

          <section className="panel">
            <PanelTitle
              title="本地转发"
              detail="把本机监听地址经隧道转发到远端设备的指定端口"
              help={{
                heading: "本地转发使用方式",
                paragraphs: ["把本机回环端口经隧道映射到远端设备的指定端口，任何 TCP 客户端（ssh、数据库工具等）都能直接使用。"],
                items: [
                  "远端选已保存的设备，或切到「手动输入 token」直接粘贴",
                  "远端端口 22 的转发会标记为 SSH，列表里可直接复制 ssh 命令（同样受「SSH 携带 TERM」统一开关控制）",
                  "停用转发会保留配置，只关闭监听",
                ],
              }}
            />
            <form className="form-grid forward-form" onSubmit={submitForward}>
              <Field label="名称">
                <input aria-label="转发名称" value={forwardForm.name} onChange={(event) => setForwardForm((current) => ({ ...current, name: event.target.value }))} placeholder="例如：nas-ssh" />
              </Field>
              <Field label="监听地址">
                <input aria-label="转发监听地址" className="mono-input" value={forwardForm.listen} onChange={(event) => setForwardForm((current) => ({ ...current, listen: event.target.value }))} placeholder="127.0.0.1:2222" />
              </Field>
              <Field label="远端">
                <Select
                  ariaLabel="选择远端设备"
                  value={forwardForm.remoteSource}
                  onValueChange={(value) => setForwardForm((current) => ({ ...current, remoteSource: value }))}
                  options={[
                    { value: "", label: "手动输入 token" },
                    ...remotes.map((item) => ({ value: item.name, label: item.name })),
                  ]}
                />
              </Field>
              {forwardForm.remoteSource === "" && (
                <Field label="远端 token">
                  <input aria-label="远端 token" className="mono-input" value={forwardForm.remoteToken} onChange={(event) => setForwardForm((current) => ({ ...current, remoteToken: event.target.value }))} placeholder="tc...（完整 token）" />
                </Field>
              )}
              <Field label="远端端口">
                <input aria-label="远端端口" max="65535" min="1" type="number" value={forwardForm.remotePort} onChange={(event) => setForwardForm((current) => ({ ...current, remotePort: event.target.value }))} placeholder="例如：22" />
              </Field>
              <Button className="form-submit" type="submit"><Plus size={16} aria-hidden="true" /><span>添加转发</span></Button>
            </form>
            {forwards.length === 0 ? (
              <EmptyState compact title="暂无本地转发" detail="添加转发后，访问本机监听地址即相当于访问远端设备的对应端口。" />
            ) : (
              <Table
                className="data-table mt-3"
                columns={forwardColumns}
                data={forwards}
                emptyState="暂无本地转发"
                getRowId={(row) => row.name}
                height={tableViewportHeight(forwards.length, 480)}
                minColumnWidth={88}
                resizable
              />
            )}
          </section>
        </>
      )}

      <Dialog open={connectTarget !== null} onOpenChange={(open) => { if (!open) setConnectTarget(null); }}>
        <DialogContent>
          <DialogClose className="dialog-close" aria-label="关闭连接对话框"><X size={16} aria-hidden="true" /></DialogClose>
          {connectTarget && (
            <PeerConnectDialog
              forwards={forwards}
              name={connectTarget}
              copyText={copyText}
              createSSHForward={createSSHForward}
              fetchPeerToken={fetchPeerToken}
              sshSetEnvTerm={sshSetEnvTerm}
              toggleForward={toggleForward}
            />
          )}
        </DialogContent>
      </Dialog>
    </div>
  );
}

/**
 * parseListen 把转发监听地址拆成 host 与端口。
 *
 * 参数说明：
 * - listen: string，形如 `127.0.0.1:10022` 的监听地址；缺省 host 时按 127.0.0.1 处理。
 *
 * 返回值说明：
 * 返回 `{ host, port }`；无法识别时端口为原文本。
 *
 * 可能的异常/错误情况：
 * 无；非法输入会退化为兜底值，不抛出异常。
 */
function parseListen(listen) {
  const text = String(listen || "");
  const index = text.lastIndexOf(":");
  if (index === -1) {
    return { host: "127.0.0.1", port: text };
  }
  return { host: text.slice(0, index) || "127.0.0.1", port: text.slice(index + 1) };
}

/**
 * sshSetEnvSuffix 按统一开关返回 SSH 命令的 SetEnv TERM 后缀。
 *
 * 参数说明：
 * - enabled: boolean，统一开关状态。
 *
 * 返回值说明：
 * 开启时返回 ` -o SetEnv=TERM=xterm-256color`，关闭时返回空串。
 */
function sshSetEnvSuffix(enabled) {
  return enabled ? " -o SetEnv=TERM=xterm-256color" : "";
}

/**
 * SnippetRow 渲染一条带独立复制按钮的命令片段。
 *
 * 参数说明：
 * - label: string，片段用途说明。
 * - text: string，完整命令文本。
 * - multiline: boolean，多行片段（如 ssh config 块）用 pre 保留缩进换行。
 * - onCopy: Function，复制回调。
 *
 * 返回值说明：
 * 返回片段行 React 元素。
 *
 * 可能的异常/错误情况：
 * 无；复制失败由 onCopy 内部 toast。
 */
function SnippetRow({ label, text, multiline = false, onCopy }) {
  return (
    <div className="grid gap-1">
      <span className="text-xs font-medium text-muted-foreground">{label}</span>
      <div className="flex items-start gap-2 rounded-md border bg-muted/40 px-3 py-2">
        {multiline ? (
          <pre className="m-0 min-w-0 flex-1 whitespace-pre-wrap break-all font-mono text-xs">{text}</pre>
        ) : (
          <code className="min-w-0 flex-1 break-all font-mono text-xs">{text}</code>
        )}
        <Button className="shrink-0" size="sm" variant="outline" type="button" onClick={() => onCopy(text)}>
          <Copy size={14} aria-hidden="true" />
          <span>复制</span>
        </Button>
      </div>
    </div>
  );
}

/**
 * PeerConnectDialog 渲染「连接到某台远程设备」对话框。
 *
 * 功能说明：
 * 分两个区块给出到同一台对端的两种 SSH 用法：A「本地转发（推荐）」复用或创建一条
 * 指向对端 22 端口的本地转发，生成标准 ssh/scp 命令，任何终端工具都可用；B「直接
 * 连接（proxyd CLI）」按需获取完整 token，生成 proxyd ssh/scp 命令与 ssh config 的
 * ProxyCommand 配置块。文件传输复用 scp，不引入单独的文件传输功能。
 *
 * 参数说明：
 * - name: string，对端设备名称。
 * - forwards: Array<object>，当前本地转发列表，用于匹配已有的 SSH 转发。
 * - fetchPeerToken: Function，按需获取完整 token。
 * - createSSHForward/toggleForward: Function，创建或启用 SSH 转发。
 * - copyText: Function，复制命令片段。
 * - sshSetEnvTerm: boolean，统一开关：复制的 SSH 命令与 ssh config 是否携带
 *   SetEnv TERM=xterm-256color。
 *
 * 返回值说明：
 * 返回对话框主体 React 元素。
 *
 * 可能的异常/错误情况：
 * - token 获取失败时区块 B 显示错误与重试按钮，不影响区块 A。
 * - 创建转发失败时由 hook toast 错误，对话框保持可重试状态。
 */
function PeerConnectDialog({ name, forwards, fetchPeerToken, createSSHForward, toggleForward, copyText, sshSetEnvTerm }) {
  const [user, setUser] = useState("");
  const [busy, setBusy] = useState(false);
  const [created, setCreated] = useState(null);
  const [tokenRetry, setTokenRetry] = useState(0);
  const [tokenState, setTokenState] = useState({ loading: true, token: "", error: "" });

  useEffect(() => {
    let cancelled = false;
    setTokenState({ loading: true, token: "", error: "" });
    fetchPeerToken(name)
      .then((nextToken) => {
        if (!cancelled) setTokenState({ loading: false, token: nextToken, error: "" });
      })
      .catch((fetchError) => {
        if (!cancelled) setTokenState({ loading: false, token: "", error: fetchError.message || "获取失败" });
      });
    return () => {
      cancelled = true;
    };
  }, [name, fetchPeerToken, tokenRetry]);

  const existing = forwards.find((item) => item.remote === name && item.remote_port === 22);
  const activeForward = existing?.enabled ? existing : created;
  const login = user.trim() ? `${user.trim()}@` : "";
  const { host, port } = parseListen(activeForward?.listen);
  const token = tokenState.token;
  const sshConfig = [
    `Host ${name}`,
    `    ProxyCommand proxyd remote pipe ${token} 22`,
    `    User ${user.trim() || "<登录用户>"}`,
    ...(sshSetEnvTerm ? [`    SetEnv TERM=xterm-256color`] : []),
  ].join("\n");

  /**
   * handleCreateForward 创建自动分配端口的 SSH 转发并记录返回的监听地址。
   *
   * 参数说明：无。
   * 返回值说明：返回 Promise<void>。
   * 可能的异常/错误情况：创建失败由 createSSHForward toast，对话框保持可重试。
   */
  async function handleCreateForward() {
    setBusy(true);
    try {
      const forward = await createSSHForward(name);
      if (forward) setCreated(forward);
    } finally {
      setBusy(false);
    }
  }

  /**
   * handleEnableForward 启用已存在但停用的 SSH 转发。
   *
   * 参数说明：无。
   * 返回值说明：返回 Promise<void>。
   * 可能的异常/错误情况：启用失败由 toggleForward toast。
   */
  async function handleEnableForward() {
    setBusy(true);
    try {
      await toggleForward(existing.name, true);
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <DialogHeader>
        <DialogTitle>连接到 {name}</DialogTitle>
        <DialogDescription>通过 tailcat 隧道 SSH 登录该设备；文件传输直接使用 scp，走同一隧道。</DialogDescription>
      </DialogHeader>
      <div className="dialog-form">
        <Field label="登录用户" hint="可选；填写后命令会带上 user@ 前缀">
          <input aria-label="SSH 登录用户" value={user} onChange={(event) => setUser(event.target.value)} placeholder="登录用户（可选）" />
        </Field>

        <div className="grid gap-2">
          <PanelTitle title="本地转发（推荐）" detail="经本机 127.0.0.1 端口连接，任何 SSH/scp 客户端都可用" />
          {activeForward ? (
            <>
              <SnippetRow label="SSH 登录" text={`ssh ${login}${host} -p ${port}${sshSetEnvSuffix(sshSetEnvTerm)}`} onCopy={(text) => copyText(text, "SSH 命令已复制")} />
              <SnippetRow label="scp 传文件" text={`scp -P ${port} <本地文件> ${login}${host}:<远程路径>`} onCopy={(text) => copyText(text, "scp 命令已复制")} />
            </>
          ) : existing ? (
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-xs text-muted-foreground">已有转发 {existing.name}（已停用），启用后即可连接。</span>
              <Button loading={busy} size="sm" variant="outline" type="button" onClick={handleEnableForward}>
                <span>启用 SSH 转发</span>
              </Button>
            </div>
          ) : (
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-xs text-muted-foreground">尚无指向该设备 22 端口的转发，创建后自动分配本机端口。</span>
              <Button loading={busy} size="sm" variant="outline" type="button" onClick={handleCreateForward}>
                <Plus size={14} aria-hidden="true" />
                <span>创建 SSH 转发</span>
              </Button>
            </div>
          )}
        </div>

        <div className="grid gap-2">
          <PanelTitle title="直接连接（proxyd CLI）" detail="无需创建转发，命令里直接携带完整 token" />
          {tokenState.loading ? (
            <p className="m-0 text-xs text-muted-foreground">正在获取完整 token…</p>
          ) : tokenState.error ? (
            <div className="flex flex-wrap items-center gap-2">
              <span className="break-words text-xs text-destructive">token 获取失败：{tokenState.error}</span>
              <Button size="sm" variant="outline" type="button" onClick={() => setTokenRetry((count) => count + 1)}>
                <RefreshCw size={14} aria-hidden="true" />
                <span>重试</span>
              </Button>
            </div>
          ) : (
            <>
              <SnippetRow label="proxyd SSH" text={`proxyd ssh ${token}${sshSetEnvSuffix(sshSetEnvTerm)}`} onCopy={(text) => copyText(text, "命令已复制")} />
              <SnippetRow label="proxyd scp" text={`proxyd scp <本地文件> ${token}:<远程路径>`} onCopy={(text) => copyText(text, "命令已复制")} />
              <SnippetRow label="ssh config（ProxyCommand）" multiline text={sshConfig} onCopy={(text) => copyText(text, "ssh config 已复制")} />
            </>
          )}
        </div>
      </div>
    </>
  );
}
