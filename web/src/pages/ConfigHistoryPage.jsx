/** 配置历史页面负责预检、明确确认和恢复反馈；完整配置不进入浏览器。 */
import { useEffect, useState } from "react";
import { Button, ButtonLink } from "@/components/ui/button";
import { PageHeader } from "@/components/PageHeader";
import { requestJSON } from "@/lib/api";

/** ConfigHistoryPage 展示版本；参数 confirmation/toast/restart 为应用回调；返回 JSX，网络失败显示错误。 */
export function ConfigHistoryPage({ requestConfirmation, showToast, onRestart }) {
  const [data, setData] = useState({ versions: [], pending_restart: false });
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  /** reload 读取元数据；无参数，返回 Promise<void>；错误保留原列表并显示提示。 */
  async function reload() { try { setData(await requestJSON("/api/config/history")); setError(""); } catch (e) { setError(e.message); } }
  useEffect(() => { const controller = new AbortController(); requestJSON("/api/config/history", { signal: controller.signal }).then(setData).catch((e) => { if (e.name !== "AbortError") setError(e.message); }); return () => controller.abort(); }, []);
  /** restore 先预检后确认；参数 version 为元数据；返回 Promise<void>，冲突时要求重新操作。 */
  async function restore(version) {
    setBusy(version.id);
    try {
      const endpoint = `/api/config/history/${encodeURIComponent(version.id)}`;
      const preview = await requestJSON(`${endpoint}/preview`, { method: "POST" });
      const accepted = await requestConfirmation({ title: "恢复此版本配置？", description: `将修改：${preview.sections.join("、") || "无差异"}。当前配置会先保存到历史。重启后生效，期间暂停设置写入；管理地址或凭据变化后可能需要重新登录。`, confirmLabel: "恢复配置" });
      if (!accepted) return;
      await requestJSON(`${endpoint}/restore`, { method: "POST", body: JSON.stringify({ digest: preview.digest, base_digest: preview.base_digest }) });
      showToast("配置已恢复，重启后生效"); await reload();
    } catch (e) { showToast(e.message, "err"); } finally { setBusy(""); }
  }
  return <div className="space-y-5">
    <PageHeader title="配置历史" detail="仅在配置内容变化时保存变更前快照，连续相同快照不重复记录，最多保留 30 个版本。下载的历史文件已脱敏；历史不包含独立的身份密钥文件。" />
    {error && <p role="alert" className="text-destructive">{error}</p>}
    {data.pending_restart && <div role="status" className="panel p-4 space-y-3"><p>配置已写入，等待重启生效。请重启后再修改设置。</p><Button onClick={onRestart}>重启守护进程</Button></div>}
    <Button variant="outline" disabled={Boolean(busy)} onClick={reload}>刷新历史</Button>
    {!data.versions.length && <p className="panel p-5 text-muted-foreground">还没有配置历史。下一次保存配置时会自动保留变更前版本。</p>}
    {data.versions.map((version) => <section key={version.id} className="panel p-5 space-y-3">
      <div className="flex flex-wrap justify-between gap-2"><strong>{new Date(version.created_at).toLocaleString()}</strong><span>{version.reason}</span></div>
      <p className="text-sm break-words text-muted-foreground">涉及配置：{version.sections.join("、") || "格式变更"}</p>
      <code className="block break-all text-xs">{version.id}</code>
      <div className="flex flex-wrap gap-2"><Button disabled={Boolean(busy) || data.pending_restart} onClick={() => restore(version)}>{busy === version.id ? "正在预检…" : "预检并恢复"}</Button><ButtonLink variant="outline" href={`/api/config/history/${encodeURIComponent(version.id)}/export`} download>下载脱敏版本</ButtonLink></div>
    </section>)}
  </div>;
}
