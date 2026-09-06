/** 诊断中心使用统一用例并支持取消；浏览器只保留脱敏报告。 */
import { useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { PageHeader } from "@/components/PageHeader";
import { requestJSON } from "@/lib/api";

const labels = { module_proxy: "代理模块", module_remote: "远程访问模块", dns_primary: "主地图 DNS", dns_fallback: "备用地图 DNS", derp_primary: "主 DERP 地图", derp_fallback: "备用 DERP 地图", derp: "DERP 中继", tunnel: "隧道连接", ssh_banner: "SSH 服务", ssh_auth: "SSH 认证", shell: "登录环境" };
const states = { passed: "通过", failed: "失败", skipped: "未检查" };

/** DiagnosticsPage 展示阶段结果；无参数；返回 JSX，加载/检查错误由本页展示，离开时取消请求。 */
export function DiagnosticsPage() {
  const [peers, setPeers] = useState([]);
  const [peer, setPeer] = useState("");
  const [report, setReport] = useState(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const active = useRef(null);
  useEffect(() => { const controller = new AbortController(); requestJSON("/api/remote/remotes", { signal: controller.signal }).then((data) => setPeers(Array.isArray(data) ? data : data.remotes || [])).catch((e) => { if (e.name !== "AbortError") setError(e.message); }); return () => { controller.abort(); active.current?.abort(); }; }, []);
  /** run 请求最长 50 秒的检查；无参数，返回 Promise<void>；取消不误报失败，旧报告先清除。 */
  async function run() {
    const controller = new AbortController(); active.current = controller; setBusy(true); setError(""); setReport(null);
    try { setReport(await requestJSON("/api/diagnostics", { method: "POST", signal: controller.signal, body: JSON.stringify({ peer }) })); }
    catch (e) { setError(e.name === "AbortError" ? "诊断已取消" : e.message); }
    finally { if (active.current === controller) { active.current = null; setBusy(false); } }
  }
  /** download 导出固定结构报告；无参数、无返回；报告为空时忽略，临时 URL 随后释放。 */
  function download() { if (!report) return; const url = URL.createObjectURL(new Blob([JSON.stringify(report, null, 2)], { type: "application/json" })); const link = document.createElement("a"); link.href = url; link.download = "proxyd-diagnostics.json"; link.click(); setTimeout(() => URL.revokeObjectURL(url), 1000); }
  return <div className="space-y-5">
    <PageHeader title="诊断中心" detail="逐项检查模块、DNS、中继、隧道、SSH 认证和登录环境。完整检查最多约 50 秒。" />
    <section className="panel p-5 space-y-4">
      <label className="block space-y-2"><span>诊断对象</span><select className="w-full rounded-md border bg-background p-2" value={peer} disabled={busy} onChange={(e) => setPeer(e.target.value)}><option value="">本机服务与网络</option>{peers.map((item) => <option value={item.name} key={item.name}>{item.name}</option>)}</select></label>
      <p className="text-sm text-muted-foreground">检查从守护进程所在机器发起。远端需要 SSH 私钥时，可在客户端运行 proxyd ssh &lt;设备&gt; --diagnose -i &lt;密钥文件&gt;。</p>
      <div className="flex flex-wrap gap-2"><Button onClick={run} disabled={busy}>{busy ? "正在逐项检查…" : "开始诊断"}</Button>{busy && <Button variant="outline" onClick={() => active.current?.abort()}>取消诊断</Button>}{report && <Button variant="outline" onClick={download}>导出脱敏报告</Button>}</div>
    </section>
    {error && <p role="alert" className="text-destructive">{error}</p>}
    {report && <div className="space-y-3" aria-live="polite">{report.steps.map((step) => <section key={step.id} className="panel p-4 space-y-2"><div className="flex flex-wrap justify-between gap-2"><strong>{labels[step.id] || step.id}</strong><span className={step.status === "failed" ? "text-destructive" : "text-muted-foreground"}>{states[step.status] || step.status} · {step.duration_ms} ms</span></div><p className="text-sm break-words">{step.detail}</p></section>)}</div>}
  </div>;
}
