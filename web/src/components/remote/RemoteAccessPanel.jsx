/** RemoteAccessPanel 负责access任务面板，数据加载与配置事务由上层用例提供。 */

import { Copy, Download, Plus, RefreshCw, Upload, X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button, ButtonLink } from "@/components/ui/button";

import { Select } from "@/components/ui/select";

import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

import { PanelTitle } from "@/components/PanelTitle";
import { RemoteSSHKeys } from "@/components/RemoteSSHKeys";
import { StatusBadge } from "@/components/StatusBadge";
import { classNames, formatBytes } from "@/lib/format";
import { isRemoteAllowExpired, formatRemoteAllowExpiry, formatRemotePath } from "./RemoteConnectionTools";

/**
 * RemoteAccessPanel 渲染独立任务视图。
 * 参数：解构字段为状态对象、表单值或事件回调，沿用 RemotePage 的接口约定。
 * 返回：React 元素；网络错误由用例回调处理，本组件不直接写配置。
 */
export function RemoteAccessPanel({ activity, allow, allowInput, allowNameInput, allowPortsInput, allowTTL, copyTempKey, copyText, importKeyFile, keyFileInput, manageSSHKeys, peers, removeAllowKey, resetTempKey, saveKeyFile, setAllowInput, setAllowNameInput, setAllowPortsInput, setAllowTTL, setKeyFileInput, status, submitAllowKey, submitKeyFile, tempPeer }) {
 return (<><Tabs defaultValue="ssh">
 <TabsList aria-label="授权类型"><TabsTrigger value="ssh">SSH 公钥</TabsTrigger><TabsTrigger value="clients">客户端白名单</TabsTrigger><TabsTrigger value="identity">隧道身份</TabsTrigger></TabsList>
 <TabsContent value="ssh" forceMount>              <RemoteSSHKeys status={status} manageSSHKeys={manageSSHKeys} copyText={copyText} /></TabsContent>
 <TabsContent value="clients" forceMount><section className="panel"><PanelTitle title="客户端白名单与临时身份" detail="限制哪些隧道身份可以访问本机端口，与 SSH 公钥认证独立。" />
              <div className="mt-3 grid gap-2 rounded-md border bg-muted/40 px-3 py-2">
                <span className="text-xs font-medium text-muted-foreground">
                  允许的客户端（最小授权）{allow.length === 0 && !status?.temp_key && !status?.allow_restricted && "：当前放行所有持有 token 的客户端"}
                  {allow.length === 0 && !status?.temp_key && status?.allow_restricted && "：过期清扫后保持拒绝所有客户端"}
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
                  <Select
                    ariaLabel="客户端授权有效期"
                    value={allowTTL}
                    onValueChange={setAllowTTL}
                    options={[
                      { value: "permanent", label: "永久" },
                      { value: "1h", label: "1 小时" },
                      { value: "1d", label: "1 天" },
                      { value: "7d", label: "7 天" },
                    ]}
                  />
                  <input
                    aria-label="客户端限定端口"
                    className="w-36 shrink-0"
                    value={allowPortsInput}
                    onChange={(event) => setAllowPortsInput(event.target.value)}
                    placeholder="端口：22,8080"
                  />
                  <Button size="sm" variant="outline" type="submit"><Plus size={14} aria-hidden="true" /><span>添加</span></Button>
                </form>
                {allow.length > 0 && (
                  <ul className="m-0 grid list-none gap-1.5 p-0">
                    {allow.map((entry) => {
                      const peer = peers.find((item) => item.key === entry.key);
                      return (
                        <li key={entry.key} className={classNames("grid gap-1 rounded-md border px-2.5 py-2", isRemoteAllowExpired(entry) && "opacity-50")}>
                          <div className="flex flex-wrap items-center gap-2">
                            {entry.name && <Badge variant="secondary" className="shrink-0">{entry.name}</Badge>}
                            <code className="min-w-0 flex-1 break-all font-mono text-xs">{entry.key}</code>
                            <Badge variant={isRemoteAllowExpired(entry) ? "destructive" : "secondary"}>{formatRemoteAllowExpiry(entry)}</Badge>
                            <Badge variant="outline">{entry.ports?.length ? `端口 ${entry.ports.join("、")}` : "全部暴露端口"}</Badge>
                            <button aria-label={`移出白名单 ${entry.name || entry.key.slice(0, 20)}`} className="inline-flex shrink-0 items-center text-muted-foreground hover:text-destructive" type="button" onClick={() => removeAllowKey(entry.key)}>
                              <X size={13} aria-hidden="true" />
                            </button>
                          </div>
                          <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                            <StatusBadge ok={Boolean(peer?.online)} text={peer?.online ? "在线" : "离线"} />
                            <span>{formatRemotePath(peer)}</span>
                            <span>RTT —</span>
                            <span>收 {formatBytes(peer?.rx_bytes || 0)} · 发 {formatBytes(peer?.tx_bytes || 0)}</span>
                            {(activity[entry.key] || 0) > 0 && <Badge variant="secondary">活动连接 {activity[entry.key]}</Badge>}
                          </div>
                        </li>
                      );
                    })}
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
                    <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                      <StatusBadge ok={Boolean(tempPeer?.online)} text={tempPeer?.online ? "在线" : "离线"} />
                      <span>{formatRemotePath(tempPeer)}</span>
                      <span>RTT —</span>
                      <span>收 {formatBytes(tempPeer?.rx_bytes || 0)} · 发 {formatBytes(tempPeer?.tx_bytes || 0)}</span>
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
</section></TabsContent>
 <TabsContent value="identity" forceMount><section className="panel"><PanelTitle title="服务端身份文件" detail="用于保持或迁移本机隧道身份。" />              <div className="mt-3 grid gap-2 rounded-md border bg-muted/40 px-3 py-2">
                <span className="text-xs font-medium text-muted-foreground">
                  服务端密钥文件（决定 token）：{status?.custom_key_file ? "当前使用自定义路径" : "当前由 proxyd 内置托管"}
                </span>
                {status?.key_file && (
                  <code className="min-w-0 break-all font-mono text-xs text-muted-foreground">{status.key_file}</code>
                )}
                <div className="flex flex-wrap items-center gap-2">
                  <ButtonLink href="/api/remote/keyfile/export" download size="sm" variant="outline">
                    <Download size={14} aria-hidden="true" />
                    <span>导出私钥</span>
                  </ButtonLink>
                  <label className="beui-link-button config-upload">
                    <Upload size={14} aria-hidden="true" />导入私钥
                    <input
                      accept=".json,.private.json,application/json"
                      type="file"
                      onChange={(event) => {
                        importKeyFile(event.target.files?.[0]);
                        event.target.value = "";
                      }}
                    />
                  </label>
                </div>
                <form className="flex flex-wrap items-center gap-2" onSubmit={submitKeyFile}>
                  <input
                    aria-label="自定义服务端密钥文件路径"
                    className="mono-input min-w-0 flex-1"
                    value={keyFileInput}
                    onChange={(event) => setKeyFileInput(event.target.value)}
                    placeholder="可选：tailcat 密钥文件路径（~/ 开头亦可）"
                  />
                  <Button size="sm" variant="outline" type="submit"><span>保存路径</span></Button>
                  {status?.custom_key_file && (
                    <Button size="sm" variant="outline" type="button" onClick={() => saveKeyFile("")}>
                      <span>恢复内置托管</span>
                    </Button>
                  )}
                </form>
                <span className="text-xs text-muted-foreground">
                  导出文件包含完整私钥，请安全保存；导入会覆盖内置托管密钥并更新 token，失败时自动回滚。也可用 CLI：proxyd remote keyfile export/import。
                </span>
              </div>
</section></TabsContent>
 </Tabs></>);
}
