/**
 * RemoteFilesPanel 提供浏览器内的远端目录浏览、上传与下载界面。
 * 数据链路固定为浏览器 → 本机 proxyd 管理 API → tailcat 隧道 → SSH/SFTP，
 * 组件不读取完整 tailcat token，也不持久化 SSH 私钥或口令。
 */

import { useEffect, useRef, useState } from "react";
import { ArrowUp, Download, File, Folder, KeyRound, RefreshCw, Upload } from "lucide-react";

import { Button } from "@/components/ui/button";
import { EmptyState } from "@/components/EmptyState";
import { Field } from "@/components/Field";
import { PanelTitle } from "@/components/PanelTitle";
import { formatBytes } from "@/lib/format";

/**
 * joinRemotePath 把当前 SFTP 目录与文件名拼为远端路径。
 * 参数说明：directory 为目录；name 为服务端返回或浏览器选择的文件名。
 * 返回值说明：使用 `/` 分隔的字符串，保留根目录语义。
 * 可能的异常/错误情况：无；服务端仍负责校验路径存在性和权限。
 */
function joinRemotePath(directory, name) {
  if (!directory || directory === ".") return name;
  if (directory === "/") return `/${name}`;
  return `${directory.replace(/\/$/, "")}/${name}`;
}

/**
 * parentRemotePath 计算目录工具栏的上一级路径。
 * 参数说明：directory 为 SFTP 返回的规范化路径。
 * 返回值说明：根目录保持 `/`，相对路径无父级时返回 `.`。
 * 可能的异常/错误情况：无；Windows SFTP 盘符等特殊路径最终由服务端 RealPath 规范化。
 */
function parentRemotePath(directory) {
  const value = (directory || ".").replace(/\/+$/, "");
  if (!value || value === "/") return "/";
  const index = value.lastIndexOf("/");
  if (index < 0) return ".";
  return index === 0 ? "/" : value.slice(0, index);
}

/**
 * formatRemoteFileTime 把远端 ISO 时间转换为本地短日期时间。
 * 参数说明：value 为后端 JSON 时间字符串。
 * 返回值说明：合法时间返回本地格式，缺失或非法时返回 `-`。
 * 可能的异常/错误情况：无；解析失败被降级显示。
 */
function formatRemoteFileTime(value) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "-" : date.toLocaleString("zh-CN", { hour12: false });
}

/**
 * RemoteFilesPanel 渲染远端文件传输面板。
 *
 * 参数说明：
 * - remotes: Array<object>，保存的远端设备摘要列表。
 * - apiLoopback: boolean，管理 API 是否只监听回环，用于私钥传输风险提示。
 * - listRemoteFiles: Function，列出目录的应用用例。
 * - uploadRemoteFile: Function，上传单个浏览器 File 并报告进度。
 * - downloadRemoteFile: Function，下载远端普通文件。
 *
 * 返回值说明：React 元素；凭据只保存在组件内存，卸载页面即释放。
 * 可能的异常/错误情况：连接或传输错误显示在面板内；完整错误也由用例 toast 呈现。
 */
export function RemoteFilesPanel({ apiLoopback, remotes, listRemoteFiles, uploadRemoteFile, downloadRemoteFile }) {
  const [remoteName, setRemoteName] = useState("");
  const [directory, setDirectory] = useState(".");
  const [entries, setEntries] = useState([]);
  const [username, setUsername] = useState("proxyd");
  const [privateKey, setPrivateKey] = useState("");
  const [privateKeyName, setPrivateKeyName] = useState("");
  const [passphrase, setPassphrase] = useState("");
  const [loading, setLoading] = useState(false);
  const [transferring, setTransferring] = useState(false);
  const [progress, setProgress] = useState(0);
  const [error, setError] = useState("");
  const uploadInputRef = useRef(null);

  useEffect(() => {
    if (remoteName && remotes.some((item) => item.name === remoteName)) return;
    setRemoteName(remotes[0]?.name || "");
    setDirectory(".");
    setEntries([]);
  }, [remoteName, remotes]);

  /**
   * credentials 构造单次请求共享的连接字段。
   * 参数说明：无，读取当前组件内存状态。
   * 返回值说明：object，供三个文件用例使用。
   * 可能的异常/错误情况：无；空私钥表示仅使用隧道身份/SSH none 认证。
   */
  function credentials() {
    return { remote: remoteName, username, privateKey, passphrase };
  }

  /**
   * loadDirectory 连接远端并加载指定目录。
   * 参数说明：target 为待浏览的 SFTP 路径，默认使用输入框当前值。
   * 返回值说明：Promise<boolean>，加载成功为 true。
   * 可能的异常/错误情况：未选择设备或用例失败时保留原列表并展示错误提示。
   */
  async function loadDirectory(target = directory) {
    if (!remoteName) {
      setError("请先添加并选择远程设备");
      return false;
    }
    setLoading(true);
    setError("");
    const listing = await listRemoteFiles({ ...credentials(), path: target || "." });
    setLoading(false);
    if (!listing) {
      setError("目录读取失败，请检查远端在线状态、内嵌 SSH/SFTP 与登录凭据");
      return false;
    }
    setDirectory(listing.path || target || ".");
    setEntries(listing.entries || []);
    return true;
  }

  /**
   * handlePrivateKeyFile 把用户选择的 SSH 私钥读入本页内存。
   * 参数说明：event 为 file input change 事件，仅使用首个文件。
   * 返回值说明：Promise<void>。
   * 可能的异常/错误情况：浏览器读取失败时清空凭据并显示错误；内容不会写 localStorage。
   */
  async function handlePrivateKeyFile(event) {
    const file = event.target.files?.[0];
    if (!file) return;
    try {
      setPrivateKey(await file.text());
      setPrivateKeyName(file.name);
      setError("");
    } catch (readError) {
      setPrivateKey("");
      setPrivateKeyName("");
      setError(`SSH 私钥读取失败：${readError.message}`);
    }
  }

  /**
   * handleRemoteChange 切换文件目标设备，并主动清除上一台设备的临时 SSH 凭据。
   * 参数说明：event 为设备 select 的 change 事件。
   * 返回值说明：无。
   * 可能的异常/错误情况：无；清空私钥可避免用户误把 A 设备凭据发送给 B 设备。
   */
  function handleRemoteChange(event) {
    setRemoteName(event.target.value);
    setDirectory(".");
    setEntries([]);
    setPrivateKey("");
    setPrivateKeyName("");
    setPassphrase("");
    if (uploadInputRef.current) uploadInputRef.current.value = "";
  }

  /**
   * handleUploadFile 上传用户选择的单个文件到当前目录。
   * 参数说明：event 为隐藏 file input 的 change 事件。
   * 返回值说明：Promise<void>；成功后重新加载目录。
   * 可能的异常/错误情况：同名目标会由后端以临时文件原子替换；失败时显示错误，
   * 且始终重置 input，允许再次选择同一文件。
   */
  async function handleUploadFile(event) {
    const file = event.target.files?.[0];
    if (!file || !remoteName) return;
    setTransferring(true);
    setProgress(0);
    setError("");
    try {
      await uploadRemoteFile({
        ...credentials(),
        path: joinRemotePath(directory, file.name),
        file,
        onProgress: setProgress,
      });
      await loadDirectory(directory);
    } catch (uploadError) {
      setError(`上传失败：${uploadError.message}`);
    } finally {
      setTransferring(false);
      setProgress(0);
      event.target.value = "";
    }
  }

  /**
   * handleDownload 下载一个服务端已确认的普通文件。
   * 参数说明：entry 为目录列表中的文件条目。
   * 返回值说明：Promise<void>。
   * 可能的异常/错误情况：下载用例失败时由全局 toast 展示，按钮状态仍会恢复。
   */
  async function handleDownload(entry) {
    setTransferring(true);
    setError("");
    await downloadRemoteFile({ ...credentials(), path: entry.path, filename: entry.name });
    setTransferring(false);
  }

  return (
    <section className="panel">
      <PanelTitle
        title="直接传输文件"
        detail="通过本机 proxyd 和 tailcat 隧道浏览、上传或下载远端文件"
        help={{
          heading: "文件传输链路",
          paragraphs: ["远端需开启内嵌 SSH（新版会同时提供标准 SFTP），或在 22 端口提供支持 SFTP 的系统 SSH 服务。"],
          items: [
            "未开启额外 SSH 公钥认证时无需私钥；开启后可临时选择私钥文件与填写口令",
            "私钥和口令只保存在当前页面内存并随单次请求发送，不会保存到 proxyd 配置",
            "上传采用同目录临时文件后原子替换；同名文件会被覆盖",
            "网页下载会暂存为浏览器 Blob；超大文件建议使用 proxyd scp",
          ],
          note: "文件流与终端使用同一个 shell-user 权限边界；自定义中继启用时也沿用 token 内嵌的中继信息。",
        }}
      />

      <div className="form-grid remote-form">
        <Field label="远程设备">
          <select value={remoteName} onChange={handleRemoteChange}>
            <option value="">请选择设备</option>
            {remotes.map((item) => <option key={item.name} value={item.name}>{item.name}</option>)}
          </select>
        </Field>
        <Field label="远端目录">
          <input className="mono-input" value={directory} onChange={(event) => setDirectory(event.target.value)} placeholder=". 或 /home/user" />
        </Field>
        <Button disabled={!remoteName} loading={loading} type="button" onClick={() => loadDirectory()}>
          <RefreshCw size={16} aria-hidden="true" /><span>连接并浏览</span>
        </Button>
      </div>

      <details className="mt-3 rounded-md border bg-muted/30 px-3 py-2">
        <summary className="cursor-pointer text-sm font-medium"><KeyRound className="mr-1 inline" size={15} aria-hidden="true" />可选 SSH 登录凭据</summary>
        {apiLoopback === false && (
          <p className="permission-note warn">管理 API 当前不是回环监听。只有在前方已配置可信 HTTPS 与访问控制时才上传 SSH 私钥；明文 HTTP 会暴露私钥与口令。</p>
        )}
        <div className="mt-3 grid gap-3 md:grid-cols-3">
          <Field label="SSH 用户名" hint="内嵌 SSH 忽略此值，系统 sshd 会使用">
            <input value={username} onChange={(event) => setUsername(event.target.value)} placeholder="proxyd" />
          </Field>
          <Field label="SSH 私钥" hint={privateKeyName ? `已载入 ${privateKeyName}（仅内存）` : "仅在远端要求公钥认证时选择"}>
            <input type="file" onChange={handlePrivateKeyFile} />
          </Field>
          <Field label="私钥口令" hint="没有口令请留空">
            <input type="password" value={passphrase} onChange={(event) => setPassphrase(event.target.value)} autoComplete="off" />
          </Field>
        </div>
      </details>

      <div className="mt-3 flex flex-wrap items-center gap-2">
        <Button disabled={!remoteName || loading || transferring} size="sm" variant="outline" type="button" onClick={() => loadDirectory(parentRemotePath(directory))}>
          <ArrowUp size={15} aria-hidden="true" /><span>上一级</span>
        </Button>
        <Button disabled={!remoteName || loading || transferring} size="sm" type="button" onClick={() => uploadInputRef.current?.click()}>
          <Upload size={15} aria-hidden="true" /><span>{transferring && progress > 0 ? `上传中 ${progress}%` : "上传文件"}</span>
        </Button>
        <input ref={uploadInputRef} className="hidden" type="file" onChange={handleUploadFile} />
        <code className="min-w-0 flex-1 truncate text-xs text-muted-foreground" title={directory}>{directory}</code>
      </div>

      {error && <div className="mt-3 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive">{error}</div>}

      {entries.length === 0 ? (
        <EmptyState compact title="尚未读取目录" detail="选择远程设备后点击“连接并浏览”，即可直接传输文件。" />
      ) : (
        <div className="mt-3 divide-y rounded-md border">
          {entries.map((entry) => (
            <div key={entry.path} className="flex min-w-0 items-center gap-3 px-3 py-2">
              {entry.is_dir ? <Folder className="shrink-0 text-warning" size={18} aria-hidden="true" /> : <File className="shrink-0 text-muted-foreground" size={18} aria-hidden="true" />}
              <button className="min-w-0 flex-1 truncate text-left text-sm hover:underline" type="button" title={entry.path} onClick={() => entry.is_dir ? loadDirectory(entry.path) : handleDownload(entry)}>
                {entry.name}
              </button>
              <span className="hidden text-xs text-muted-foreground sm:inline">{entry.is_dir ? "目录" : formatBytes(entry.size)}</span>
              <span className="hidden text-xs text-muted-foreground lg:inline">{formatRemoteFileTime(entry.mod_time)}</span>
              {!entry.is_dir && (
                <Button aria-label={`下载 ${entry.name}`} disabled={transferring} size="icon" variant="ghost" type="button" onClick={() => handleDownload(entry)}>
                  <Download size={15} aria-hidden="true" />
                </Button>
              )}
            </div>
          ))}
        </div>
      )}
    </section>
  );
}
