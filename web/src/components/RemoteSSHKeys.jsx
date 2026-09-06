import { useState } from "react";
import { Copy, Plus, Trash2, Upload } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Switch as UISwitch } from "@/components/ui/switch";

/**
 * RemoteSSHKeys 管理内嵌 SSH 的附加公钥认证，公钥与隧道身份分开展示。
 * 参数说明：status 为 object|null，包含 ssh_auth_required、ssh_keys；
 * manageSSHKeys 为 Function，返回 Promise<boolean> 的写入方法；copyText 为 Function，复制公钥文本。
 * 返回值说明：React 元素，包含模式开关、文件导入、添加表单与逐项删除。
 * 错误情况：文件读取或格式预检失败显示就地错误；服务端拒绝操作时保留输入以便修正。
 */
export function RemoteSSHKeys({ status, manageSSHKeys, copyText }) {
  const [name, setName] = useState("");
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const keys = status?.ssh_keys || [];
  const required = Boolean(status?.ssh_auth_required);

  /**
   * submit 导入粘贴的公钥，操作期间禁用重复提交。
   * 参数说明：event 为 React.FormEvent<HTMLFormElement>，用于阻止浏览器表单跳转。
   * 返回值说明：Promise<void>，成功后清空表单。
   * 错误情况：误贴私钥、空文本或请求失败不会清空用户输入；私钥预检在网络请求前完成。
   */
  async function submit(event) {
    event.preventDefault();
    setError("");
    if (!text.trim() || text.includes("PRIVATE KEY")) {
      setError("请提供 .pub 或 authorized_keys 公钥内容，不要上传私钥。");
      return;
    }
    setBusy(true);
    try {
      if (await manageSSHKeys("add", { name: name.trim(), public_key: text }, "SSH 公钥已添加")) {
        setName("");
        setText("");
      }
    } finally {
      setBusy(false);
    }
  }

  /**
   * readPublicFile 将本机公钥文件载入表单，交给用户检查后再提交。
   * 参数说明：event 为 React.ChangeEvent<HTMLInputElement>，包含可选 File。
   * 返回值说明：Promise<void>，只读取文本，不自动上传。
   * 错误情况：超过 64 KiB、误选私钥或文件不可读时显示错误，保留原表单。
   */
  async function readPublicFile(event) {
    const file = event.target.files?.[0];
    event.target.value = "";
    if (!file) return;
    setError("");
    try {
      if (file.size > 64 * 1024) throw new Error("公钥文件不得超过 64 KiB");
      const content = await file.text();
      if (content.includes("PRIVATE KEY")) throw new Error("这是私钥文件，请选择对应的 .pub 公钥文件");
      setText(content);
    } catch (readError) {
      setError(readError.message || "无法读取公钥文件");
    }
  }

  /**
   * changeAuth 保存认证模式，同时串行化该面板中的写操作。
   * 参数说明：next 为 boolean，是否要求 SSH 公钥认证。
   * 返回值说明：Promise<void>。
   * 错误情况：网络或事务失败由 hook 提示，开关继续显示服务端已确认的原值。
   */
  async function changeAuth(next) {
    setBusy(true);
    try {
      await manageSSHKeys("auth", { required: next }, next ? "已开启 SSH 公钥认证" : "已恢复隧道免密登录");
    } finally {
      setBusy(false);
    }
  }

  /**
   * removeKey 按稳定指纹撤销单把公钥，避免同名条目误删。
   * 参数说明：fingerprint 为 string，服务端返回的 SHA256 公钥指纹。
   * 返回值说明：Promise<void>。
   * 错误情况：事务失败保留原列表；删除最后一项也不会自动关闭公钥认证。
   */
  async function removeKey(fingerprint) {
    setBusy(true);
    try {
      await manageSSHKeys("delete", { identifier: fingerprint }, "SSH 公钥已删除");
    } finally {
      setBusy(false);
    }
  }

  /**
   * changeKeyPolicy 串行提交生命周期或断开操作，不整体覆盖其他管理员的授权条目。
   * 参数说明：action 为 string；value 为 object；message 为 string；返回值：Promise<boolean>。
   * 错误情况：失败保留服务端原状态，错误由 hook 展示。
   */
  async function changeKeyPolicy(action, value, message) {
    setBusy(true);
    try { return await manageSSHKeys(action, value, message); }
    finally { setBusy(false); }
  }

  return (
    <section className="mt-3 grid gap-3 rounded-md border bg-muted/40 px-3 py-3" aria-label="SSH 登录公钥管理">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-medium">SSH 登录公钥</span>
        <Badge variant="secondary">{keys.length} 把公钥</Badge>
      </div>
      <UISwitch checked={required} disabled={busy} className="mt-0 border-0 pt-0" label="额外要求 SSH 公钥认证" onCheckedChange={changeAuth} />
      <p className="m-0 text-xs text-muted-foreground">
        开启后，客户端通过隧道验证仍需提供匹配的 SSH 私钥。授权变更立即影响后续认证，已有连接保留，可按公钥显式断开。隧道白名单和 Web Terminal 独立管理。
        {!status?.builtin_ssh && " 当前内嵌 SSH 已关闭，公钥配置会在开启后生效。"}
      </p>
      {required && keys.length === 0 && <p role="status" className="m-0 text-xs text-destructive">尚无授权公钥，当前拒绝所有内嵌 SSH 登录。添加公钥或关闭附加认证后可恢复连接。</p>}
      <form className="grid gap-2" onSubmit={submit}>
        <input aria-label="SSH 公钥名称" value={name} maxLength={128} disabled={busy} onChange={(event) => setName(event.target.value)} placeholder="名称（可选，如工作电脑）" />
        <textarea aria-label="SSH 公钥内容" className="mono-input min-h-24 w-full rounded-md border bg-background p-2 text-xs" value={text} disabled={busy} onChange={(event) => setText(event.target.value)} placeholder="ssh-ed25519 AAAA…；也可导入多行 authorized_keys" />
        <div className="flex flex-wrap items-center gap-2">
          <label className="beui-link-button config-upload">
            <Upload size={14} aria-hidden="true" />读取公钥文件
            <input type="file" disabled={busy} onChange={readPublicFile} aria-label="读取 SSH 公钥文件" />
          </label>
          <Button size="sm" variant="outline" type="submit" disabled={busy || !text.trim()}><Plus size={14} aria-hidden="true" />添加公钥</Button>
        </div>
        {error && <p role="alert" className="m-0 text-xs text-destructive">{error}</p>}
      </form>
      {keys.length > 0 && <ul className="m-0 grid list-none gap-2 p-0">
        {keys.map((entry) => (
          <li key={entry.fingerprint} className="grid gap-1 rounded-md border bg-background p-2">
            <div className="flex flex-wrap items-center gap-2">
              <span className="min-w-0 flex-1 break-all text-sm">{entry.name || "未命名公钥"}</span>
              <Badge variant="secondary">{entry.type}</Badge>
              <Button size="sm" variant="ghost" type="button" aria-label={`复制 SSH 公钥 ${entry.name || entry.fingerprint}`} onClick={() => copyText(entry.public_key, "SSH 公钥已复制")}><Copy size={14} aria-hidden="true" /></Button>
              <Button size="sm" variant="ghost" type="button" disabled={busy} aria-label={`删除 SSH 公钥 ${entry.name || entry.fingerprint}`} onClick={() => removeKey(entry.fingerprint)}><Trash2 size={14} aria-hidden="true" /></Button>
            </div>
            <code className="break-all text-xs text-muted-foreground">{entry.fingerprint}</code>
            <div className="flex flex-wrap gap-2 text-xs text-muted-foreground"><Badge variant={entry.disabled || entry.expired ? "destructive" : "success"}>{entry.disabled ? "已禁用" : entry.expired ? "已过期" : "已启用"}</Badge><span>活动连接：{entry.active || 0}</span><span>最近认证：{entry.last_used_at ? new Date(entry.last_used_at).toLocaleString() : "本次运行暂无"}</span></div>
            <SSHKeyPolicy entry={entry} busy={busy} onChange={changeKeyPolicy} />
            <details><summary className="cursor-pointer text-xs text-muted-foreground">查看公钥</summary><code className="block break-all pt-1 text-xs">{entry.public_key}</code></details>
          </li>
        ))}
      </ul>}
      <p className="m-0 text-xs text-muted-foreground">最近认证时间与活动连接仅统计当前进程，重启后清空；完整事件在「连接审计」查看。</p>
      <p className="m-0 text-xs text-muted-foreground">客户端连接：<code>proxyd ssh home -i ~/.ssh/id_ed25519</code>。只上传公钥，私钥保存在客户端。</p>
    </section>
  );
}

/**
 * SSHKeyPolicy 编辑一把公钥的状态与到期时间，并提供显式断开动作。
 * 参数说明：entry 为 object，公钥快照；busy 为 boolean；onChange 为异步写入函数。
 * 返回值说明：React 元素。
 * 错误情况：日期无效时由浏览器拦截，服务端失败保留输入；时间以本地时区输入、UTC 保存。
 */
function SSHKeyPolicy({ entry, busy, onChange }) {
  const [expiry, setExpiry] = useState("");
  /**
   * saveExpiry 只提交当前公钥到期时间；空值显式表示永久。
   * 参数说明：event 为 React.FormEvent；返回值：Promise<void>。
   * 错误情况：无效日期不提交，事务错误交由上层显示。
   */
  async function saveExpiry(event) {
    event.preventDefault();
    const date = expiry ? new Date(expiry) : null;
    if (date && !Number.isFinite(date.getTime())) return;
    await onChange("update", { identifier: entry.fingerprint, expires_at: date ? date.toISOString() : "" }, "SSH 公钥到期时间已更新");
  }
  return <div className="grid gap-2 pt-1">
    <p className="m-0 text-xs text-muted-foreground">到期时间：{entry.expires_at ? new Date(entry.expires_at).toLocaleString() : "永久"}</p>
    <form className="flex min-w-0 flex-wrap items-center gap-2" onSubmit={saveExpiry}>
      <input className="min-w-0 max-w-full text-xs" aria-label={`到期时间 ${entry.name || entry.fingerprint}`} type="datetime-local" disabled={busy} value={expiry} onChange={(event) => setExpiry(event.target.value)} />
      <Button size="sm" variant="outline" type="submit" disabled={busy}>{expiry ? "设置到期" : "设为永久"}</Button>
      <Button size="sm" variant="outline" type="button" disabled={busy} aria-label={`${entry.disabled ? "启用" : "禁用"} SSH 公钥 ${entry.name || entry.fingerprint}`} onClick={() => onChange("update", { identifier: entry.fingerprint, disabled: !entry.disabled }, "SSH 公钥状态已更新")}>{entry.disabled ? "启用" : "禁用"}</Button>
      <Button size="sm" variant="outline" type="button" disabled={busy || !entry.active} onClick={() => onChange("disconnect", { fingerprint: entry.fingerprint }, "已请求断开该公钥的现有连接")}>断开现有连接</Button>
    </form>
    <span className="text-xs text-muted-foreground">断开不会撤销授权；需要禁止重连时请先禁用公钥。</span>
  </div>;
}
