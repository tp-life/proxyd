import { formatRemotePath, formatRemoteTime, maskRemoteKey, formatRemoteAuditAction, parseListen, sshSetEnvSuffix, PeerConnectDialog } from "@/components/remote/RemoteConnectionTools";
import { RemoteDevicesIntro } from "@/components/remote/RemoteDevicesIntro";
import { RemoteServicesPanel } from "@/components/remote/RemoteServicesPanel";
import { RemoteAccessPanel } from "@/components/remote/RemoteAccessPanel";
import { RemoteAuditPanel } from "@/components/remote/RemoteAuditPanel";
import { RemoteDevicesPanel } from "@/components/remote/RemoteDevicesPanel";
import { RemoteForwardsPanel } from "@/components/remote/RemoteForwardsPanel";
import { NAV_ITEMS } from "@/lib/navigation";
import { useMemo, useState } from "react";
import { ChevronDown, ChevronUp, CircleAlert, Copy, Link2, RefreshCw, SquareTerminal, Terminal, Trash2, X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";

import { Dialog, DialogClose, DialogContent } from "@/components/ui/dialog";

import { Switch as UISwitch } from "@/components/ui/switch";

import { EmptyState } from "@/components/EmptyState";

import { PageHeader } from "@/components/PageHeader";

import { StatusBadge } from "@/components/StatusBadge";
import { classNames, formatBytes, maskRemoteSecret } from "@/lib/format";

/**
 * RemotePage 渲染远程连接页。
 *
 * 功能说明：
 * 该页是 remote 任务页面的共享编排容器，通过 view 选择设备、服务、授权、转发或审计。
 * 分页不会重置本容器中的未提交表单；导航注册表负责标题与 URL，业务写入仍由 hook 编排。
 * token 一律以摘要展示，完整值只在点击复制时显式从 `/api/remote/token` 获取。
 *
 * 参数说明：
 * - view: string，导航注册表中的 remote 子页面 id。
 * - status: object | null，`/api/remote` 响应（enabled/running/error/token/region/serve/forwards）。
 * - remotes: Array<object>，已保存的远程设备（name 与打码 token）。
 * - remoteProbes: object，以远端名称索引的在线、路径、RTT 与探测错误缓存。
 * - expandedRemote: string，当前展开质量详情的远端名称。
 * - auditEntries: Array<object>，最近 100 条入站连接审计事件。
 * - loading/refreshing: boolean，首次加载与刷新状态。
 * - error: string，最近一次加载错误文本。
 * - hasLoaded: boolean，是否至少完成过一次加载尝试。
 * - reload/toggleEnabled/copyToken/saveServe: Function，状态加载与服务/端口操作。
 * - saveAllow: Function，整体替换客户端公钥白名单。
 * - manageSSHKeys: Function，添加、导入、撤销 SSH 公钥及切换附加认证。
 * - saveKeyFile: Function，设置自定义服务端密钥文件（空串恢复内置托管密钥）。
 * - importKeyFile: Function，确认后上传并事务导入内置托管服务端私钥。
 * - probeRemote/toggleRemoteDetails: Function，手动探测与切换远端详情行。
 * - refreshAudit: Function，单独刷新连接审计列表。
 * - setBuiltinSSH: Function，热切换内嵌 SSH 服务（隧道 22 端口进程内处理）。
 * - saveShellUser: Function，设置远程会话降权账户（空串恢复进程用户）。
 * - setWebTerminal: Function，热切换高权限 Web Terminal；非回环 API 会先请求二次确认。
 * - resetTempKey/copyTempKey: Function，临时身份（应急 nodekey）的重置与私钥复制。
 * - addRemote/removeRemote/copySSHCommand: Function，远程设备操作。
 * - buildSSHCommand: Function，构造到指定设备的 `proxyd ssh` 命令（复制与
 *   Web Terminal 自动执行共用同一模板）。
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
 * - onOpenTerminal: Function，将目标交给全局终端宿主，页面切换不结束会话。
 * - 加载失败时保留旧数据并显示错误条带，用户可点击重试。
 * - 空列表时对应面板渲染空状态而非空表格。
 */
export function RemotePage({
  view = "remote/devices",
  onOpenTerminal,
  status,
  remotes,
  remoteProbes,
  expandedRemote,
  auditEntries,
  loading,
  refreshing,
  error,
  hasLoaded,
  reload,
  toggleEnabled,
  copyToken,
  saveServe,
  saveAllow,
  manageSSHKeys,
  saveKeyFile,
  importKeyFile,
  probeRemote,
  refreshAudit,
  toggleRemoteDetails,
  setBuiltinSSH,
  saveShellUser,
  setWebTerminal,
  resetTempKey,
  copyTempKey,
  addRemote,
  removeRemote,
  copySSHCommand,
  buildSSHCommand,
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
  const [allowTTL, setAllowTTL] = useState("permanent");
  const [allowPortsInput, setAllowPortsInput] = useState("");
  const [keyFileInput, setKeyFileInput] = useState("");
  const [shellUserInput, setShellUserInput] = useState("");
  const [remoteForm, setRemoteForm] = useState({ name: "", token: "" });
  const [forwardForm, setForwardForm] = useState({ name: "", listen: "", remoteSource: "", remoteToken: "", remotePort: "" });
  const [connectTarget, setConnectTarget] = useState(null);

  const page = NAV_ITEMS.find((item) => item.id === view);

  const serve = status?.serve || [];
  const allow = status?.allow || [];
  const activity = status?.client_activity || {};
  const peers = status?.peers || [];
  const tempPeer = peers.find((item) => item.key === status?.temp_key);
  const forwards = status?.forwards || [];
  const initialLoading = loading && !hasLoaded && !status;
  const terminalAvailable = Boolean(status?.web_terminal);

  const remoteColumns = useMemo(
    () => [
      { key: "name", header: "名称", sortable: true, width: "22%" },
      {
        key: "status",
        header: "状态",
        width: "22%",
        cell: (row) => {
          const probe = remoteProbes?.[row.name];
          return (
            <button
              className="inline-flex items-center gap-1.5 text-left text-xs"
              type="button"
              aria-expanded={expandedRemote === row.name}
              onClick={() => toggleRemoteDetails(row.name)}
            >
              {probe?.loading ? (
                <><RefreshCw className="animate-spin" size={13} aria-hidden="true" /><span>检测中</span></>
              ) : probe?.online ? (
                <><StatusBadge ok text={`${formatRemotePath(probe)} · ${probe.rtt_ms || 0}ms`} />{expandedRemote === row.name ? <ChevronUp size={13} aria-hidden="true" /> : <ChevronDown size={13} aria-hidden="true" />}</>
              ) : probe ? (
                <><StatusBadge ok={false} text="离线" />{expandedRemote === row.name ? <ChevronUp size={13} aria-hidden="true" /> : <ChevronDown size={13} aria-hidden="true" />}</>
              ) : (
                <><span className="text-muted-foreground">点击检测</span><ChevronDown size={13} aria-hidden="true" /></>
              )}
            </button>
          );
        },
      },
      {
        key: "token",
        header: "token 摘要",
        width: "28%",
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
            <Button
              disabled={!terminalAvailable}
              size="sm"
              title={terminalAvailable ? `打开 Web Terminal 并执行 ${buildSSHCommand(row.name)}` : "需先在「本机服务」开启 Web Terminal"}
              variant="outline"
              type="button"
              onClick={() => onOpenTerminal({ command: buildSSHCommand(row.name), target: row.name })}
            >
              <SquareTerminal size={14} aria-hidden="true" />
              <span>终端</span>
            </Button>
            <Button size="sm" variant="outline" type="button" onClick={() => copySSHCommand(row.name)}>
              <Terminal size={14} aria-hidden="true" />
              <span>复制 SSH 命令</span>
            </Button>
            <Button size="sm" variant="outline" type="button" onClick={() => copyText(`${buildSSHCommand(row.name)} --diagnose`, "SSH 诊断命令已复制；如需密钥可追加 -i 私钥路径")}>复制诊断命令</Button>
            <Button aria-label={`删除远程设备 ${row.name}`} size="icon" variant="destructive-ghost" type="button" onClick={() => removeRemote(row.name)}>
              <Trash2 size={16} aria-hidden="true" />
            </Button>
          </div>
        ),
      },
    ],
    [onOpenTerminal, buildSSHCommand, copySSHCommand, copyText, expandedRemote, remoteProbes, removeRemote, terminalAvailable, toggleRemoteDetails],
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

  const auditColumns = useMemo(
    () => [
      {
        key: "time",
        header: "时间",
        width: "17%",
        cell: (row) => <span className="whitespace-nowrap text-xs tabular-nums">{formatRemoteTime(row.time)}</span>,
      },
      {
        key: "client",
        header: "客户端",
        width: "19%",
        cell: (row) => <span className="text-xs" title={row.ssh_fingerprint || row.client_key}>{row.ssh_key_name || row.client_name || maskRemoteKey(row.ssh_fingerprint || row.client_key)}</span>,
      },
      { key: "ssh_fingerprint", header: "SSH 指纹", width: "170px", cell: (row) => <code className="break-all text-xs">{row.ssh_fingerprint || "—"}</code> },
      { key: "target_port", header: "端口", width: "70px" },
      {
        key: "action",
        header: "动作",
        width: "78px",
        cell: (row) => <Badge variant={(row.action === "rejected" || row.action === "ssh_failed") ? "destructive" : "secondary"}>{formatRemoteAuditAction(row.action)}</Badge>,
      },
      {
        key: "duration_ms",
        header: "时长",
        width: "82px",
        cell: (row) => row.duration_ms > 0 ? `${row.duration_ms}ms` : "—",
      },
      {
        key: "traffic",
        header: "收 / 发",
        width: "130px",
        cell: (row) => <span className="whitespace-nowrap text-xs">{formatBytes(row.rx_bytes || 0)} / {formatBytes(row.tx_bytes || 0)}</span>,
      },
      { key: "reason", header: "原因", cell: (row) => <span className="text-xs text-muted-foreground">{row.reason || "—"}</span> },
    ],
    [],
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
   * submitAllowKey 校验公钥、TTL 与端口范围，并把最小授权条目整体提交。
   *
   * 参数说明：event 为表单提交事件。
   * 返回值说明：返回 Promise<void>。
   * 可能的异常/错误情况：公钥、端口或重复项非法时本地拦截；后端校验失败由 saveAllow toast。
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
    const ports = allowPortsInput.trim()
      ? allowPortsInput.split(",").map((part) => Number(part.trim()))
      : [];
    if (ports.some((port) => !Number.isInteger(port) || port < 1 || port > 65535) || new Set(ports).size !== ports.length) {
      return;
    }
    const ttlMilliseconds = {
      "1h": 60 * 60 * 1000,
      "1d": 24 * 60 * 60 * 1000,
      "7d": 7 * 24 * 60 * 60 * 1000,
    }[allowTTL];
    const entry = {
      name,
      key,
      ...(ttlMilliseconds ? { expires_at: new Date(Date.now() + ttlMilliseconds).toISOString() } : {}),
      ...(ports.length > 0 ? { ports } : {}),
    };
    if (await saveAllow([...allow, entry])) {
      setAllowInput("");
      setAllowNameInput("");
      setAllowTTL("permanent");
      setAllowPortsInput("");
    }
  }

  /**
   * removeAllowKey 从白名单移除条目并整体提交；删空后的最终策略还会考虑临时身份。
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
   * submitShellUser 提交远程会话降权账户；空输入恢复进程用户语义。
   *
   * 参数说明：event 为表单提交事件。
   * 返回值说明：返回 Promise<void>。
   * 可能的异常/错误情况：账户非法或不存在由后端 400 经 saveShellUser toast。
   */
  async function submitShellUser(event) {
    event.preventDefault();
    if (await saveShellUser(shellUserInput.trim())) {
      setShellUserInput("");
    }
  }

  /**
   * clearShellUser 一键清除 shell-user 配置，远程会话恢复进程用户身份。
   *
   * 参数说明：无。
   * 返回值说明：返回 Promise<void>。
   * 可能的异常/错误情况：后端校验失败由 saveShellUser toast。
   */
  async function clearShellUser() {
    await saveShellUser("");
  }

  /**
   * openSSHPort 一键把 SSH 服务端口并入暴露列表。
   *
   * 参数说明：无；SSH 使用系统约定的 TCP 22 端口。
   * 返回值说明：返回 Promise<void>。
   * 可能的异常/错误情况：后端校验失败由 saveServe toast；端口已在列表时不重复提交。
   */
  async function openSSHPort() {
    const port = 22;
    if (serve.includes(port)) {
      return;
    }
    await saveServe([...serve, port].sort((a, b) => a - b));
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
      <PageHeader eyebrow="远程访问" title={page?.label || "设备与连接"} detail={page?.detail}>
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

      {view === "remote/devices" && <RemoteDevicesIntro  />}

      {initialLoading ? (
        <EmptyState title="正在加载远程连接状态" detail="等待 /api/remote 返回服务状态。" />
      ) : (
<>
{view === "remote/services" && <RemoteServicesPanel clearShellUser={clearShellUser} copyText={copyText} copyToken={copyToken} onOpenTerminal={onOpenTerminal} openSSHPort={openSSHPort} removeServePort={removeServePort} serve={serve} serveInput={serveInput} setBuiltinSSH={setBuiltinSSH} setServeInput={setServeInput} setShellUserInput={setShellUserInput} setWebTerminal={setWebTerminal} shellUserInput={shellUserInput} status={status} submitServePort={submitServePort} submitShellUser={submitShellUser} terminalAvailable={terminalAvailable} toggleEnabled={toggleEnabled} />}
{view === "remote/access" && <RemoteAccessPanel activity={activity} allow={allow} allowInput={allowInput} allowNameInput={allowNameInput} allowPortsInput={allowPortsInput} allowTTL={allowTTL} copyTempKey={copyTempKey} copyText={copyText} importKeyFile={importKeyFile} keyFileInput={keyFileInput} manageSSHKeys={manageSSHKeys} peers={peers} removeAllowKey={removeAllowKey} resetTempKey={resetTempKey} saveKeyFile={saveKeyFile} setAllowInput={setAllowInput} setAllowNameInput={setAllowNameInput} setAllowPortsInput={setAllowPortsInput} setAllowTTL={setAllowTTL} setKeyFileInput={setKeyFileInput} status={status} submitAllowKey={submitAllowKey} submitKeyFile={submitKeyFile} tempPeer={tempPeer} />}
{view === "remote/audit" && <RemoteAuditPanel auditColumns={auditColumns} auditEntries={auditEntries} refreshAudit={refreshAudit} />}
{view === "remote/devices" && <RemoteDevicesPanel expandedRemote={expandedRemote} probeRemote={probeRemote} remoteColumns={remoteColumns} remoteForm={remoteForm} remoteProbes={remoteProbes} remotes={remotes} setRemoteForm={setRemoteForm} setSshSetEnvTerm={setSshSetEnvTerm} sshSetEnvTerm={sshSetEnvTerm} submitRemote={submitRemote} />}
{view === "remote/forwards" && <RemoteForwardsPanel forwardColumns={forwardColumns} forwardForm={forwardForm} forwards={forwards} remotes={remotes} setForwardForm={setForwardForm} submitForward={submitForward} />}
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
