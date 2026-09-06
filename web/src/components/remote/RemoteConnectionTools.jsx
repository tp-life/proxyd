/** 远程连接对话框、探测详情与展示函数，独立于页面表单和配置写入。 */

import { useEffect, useState } from "react";
import { Copy, Plus, RefreshCw } from "lucide-react";

import { Button } from "@/components/ui/button";

import { DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";

import { Field } from "@/components/Field";

import { PanelTitle } from "@/components/PanelTitle";

/**
 * RemoteProbeDetail 渲染远端设备展开行，并提供手动探测入口。
 *
 * 参数说明：
 * - name: string，远端设备名称。
 * - probe: object | undefined，最近一次探测状态。
 * - onRefresh: Function，手动重新执行 disco ping。
 *
 * 返回值说明：返回包含状态、路径、RTT、端点和检测时间的 React 元素。
 * 可能的异常/错误情况：探测失败时显示后端原因；没有结果时提示首次检测。
 */
export function RemoteProbeDetail({ name, probe, onRefresh }) {
  return (
    <div className="grid gap-2 rounded-md bg-muted/40 p-3 text-xs">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <strong className="text-foreground">{name} · 连接质量</strong>
        <Button loading={Boolean(probe?.loading)} size="sm" variant="outline" type="button" onClick={onRefresh}>
          <RefreshCw size={13} aria-hidden="true" />
          <span>立即检测</span>
        </Button>
      </div>
      {probe ? (
        <div className="grid gap-2 sm:grid-cols-4">
          <span><b className="text-foreground">状态：</b>{probe.online ? "在线" : "离线"}</span>
          <span><b className="text-foreground">路径：</b>{formatRemotePath(probe)}</span>
          <span><b className="text-foreground">RTT：</b>{probe.rtt_ms > 0 ? `${probe.rtt_ms}ms` : "—"}</span>
          <span><b className="text-foreground">检测：</b>{formatRemoteTime(probe.checked_at)}</span>
          {(probe.endpoint || probe.derp_region) && (
            <span className="break-all sm:col-span-4"><b className="text-foreground">链路：</b>{probe.endpoint || `DERP ${probe.derp_region}`}</span>
          )}
          {probe.error && <span className="break-words text-destructive sm:col-span-4">{probe.error}</span>}
        </div>
      ) : (
        <span className="text-muted-foreground">展开后会立即检测，并每 30 秒自动刷新本行。</span>
      )}
    </div>
  );
}

/**
 * formatRemotePath 把 direct/derp/unknown 路径转换为中文，并附加 DERP 区域。
 *
 * 参数说明：observation 为服务端 peer 或远端 probe 对象；可为空。
 * 返回值说明：返回“直连”“中继（区域）”或“路径未知”。
 * 可能的异常/错误情况：未知枚举与空对象均安全降级，不抛异常。
 */
export function formatRemotePath(observation) {
  if (observation?.path === "direct") return "直连";
  if (observation?.path === "derp") {
    return observation.derp_region ? `中继（${observation.derp_region}）` : "中继";
  }
  return "路径未知";
}

/**
 * isRemoteAllowExpired 判断授权是否已经到达 expires_at 边界。
 *
 * 参数说明：entry 为客户端授权对象。
 * 返回值说明：有合法 expires_at 且不晚于当前时间时返回 true。
 * 可能的异常/错误情况：非法日期按未过期展示，后端配置校验仍是最终保障。
 */
export function isRemoteAllowExpired(entry) {
  if (!entry?.expires_at) return false;
  const expiresAt = Date.parse(entry.expires_at);
  return Number.isFinite(expiresAt) && expiresAt <= Date.now();
}

/**
 * formatRemoteAllowExpiry 把授权绝对过期时间转换为永久、已过期或剩余时长。
 *
 * 参数说明：entry 为含可选 expires_at 的客户端授权。
 * 返回值说明：返回简短中文期限文本。
 * 可能的异常/错误情况：无法解析的日期显示“时间无效”，便于发现异常配置。
 */
export function formatRemoteAllowExpiry(entry) {
  if (!entry?.expires_at) return "永久";
  const remaining = Date.parse(entry.expires_at) - Date.now();
  if (!Number.isFinite(remaining)) return "时间无效";
  if (remaining <= 0) return "已过期";
  if (remaining < 60 * 60 * 1000) return `剩余 ${Math.max(1, Math.ceil(remaining / 60_000))} 分钟`;
  if (remaining < 24 * 60 * 60 * 1000) return `剩余 ${Math.ceil(remaining / 3_600_000)} 小时`;
  return `剩余 ${Math.ceil(remaining / 86_400_000)} 天`;
}

/**
 * formatRemoteTime 把 ISO 时间转换为本地紧凑文本。
 *
 * 参数说明：value 为 ISO 字符串或 Date 可解析值。
 * 返回值说明：返回本地日期时间；空值或非法值返回“—”。
 * 可能的异常/错误情况：Intl 格式化失败时安全返回“—”。
 */
export function formatRemoteTime(value) {
  if (!value) return "—";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return "—";
  return parsed.toLocaleString("zh-CN", { hour12: false });
}

/**
 * maskRemoteKey 缩短审计表中的客户端公钥，同时保留可识别首尾。
 *
 * 参数说明：value 为 nodekey 文本。
 * 返回值说明：短文本原样返回，长文本折叠为首尾摘要，空值显示“未知客户端”。
 * 可能的异常/错误情况：无；非字符串通过 String 安全转换。
 */
export function maskRemoteKey(value) {
  const text = String(value || "");
  if (!text) return "未知客户端";
  if (text.length <= 20) return text;
  return `${text.slice(0, 11)}…${text.slice(-6)}`;
}

/**
 * formatRemoteAuditAction 把连接审计动作转换为中文。
 *
 * 参数说明：action 为 connected/rejected/disconnected 枚举。
 * 返回值说明：返回建立/拒绝/断开；未知枚举原样显示以兼容新版后端。
 * 可能的异常/错误情况：无。
 */
export function formatRemoteAuditAction(action) {
  return { connected: "建立", rejected: "拒绝", disconnected: "断开", ssh_authenticated: "SSH 已认证", ssh_failed: "SSH 失败", ssh_disconnected: "SSH 结束", ssh_disconnect_requested: "请求断开" }[action] || action || "—";
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
export function parseListen(listen) {
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
export function sshSetEnvSuffix(enabled) {
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
export function SnippetRow({ label, text, multiline = false, onCopy }) {
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
export function PeerConnectDialog({ name, forwards, fetchPeerToken, createSSHForward, toggleForward, copyText, sshSetEnvTerm }) {
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
        <DialogDescription>SSH 与文件传输都通过同一条 tailcat 加密隧道，优先 NAT 穿透直连。</DialogDescription>
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
