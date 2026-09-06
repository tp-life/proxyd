/** RemoteServicesPanel 负责services任务面板，数据加载与配置事务由上层用例提供。 */

import { Copy, Plus, Terminal, X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";

import { Switch as UISwitch } from "@/components/ui/switch";

import { EmptyState } from "@/components/EmptyState";
import { Field } from "@/components/Field";

import { PanelTitle } from "@/components/PanelTitle";

import { StatusBadge } from "@/components/StatusBadge";

/**
 * RemoteServicesPanel 渲染独立任务视图。
 * 参数：解构字段为状态对象、表单值或事件回调，沿用 RemotePage 的接口约定。
 * 返回：React 元素；网络错误由用例回调处理，本组件不直接写配置。
 */
export function RemoteServicesPanel({ copyText, copyToken, onOpenTerminal, openSSHPort, removeServePort, serve, serveInput, setBuiltinSSH, setServeInput, setWebTerminal, status, submitServePort, terminalAvailable, toggleEnabled }) {
 return (<>            <section className="panel">
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
                    "内嵌 SSH：开启后隧道 22 端口由 proxyd 进程内 SSH 服务直接处理，无需系统 sshd（macOS 远程登录）。默认通过隧道认证后即可登录 proxyd 运行用户的 shell；可在「访问授权」额外启用 SSH 公钥认证，并配合「允许的客户端」白名单限制来源",
                    "允许的客户端：添加对端的客户端公钥后，只有列表内的机器能连入本机（token+私钥双重校验）；清空则恢复放行所有。可给每个公钥起别名方便管理（CLI：proxyd remote allow add <公钥> [别名]，del 按别名或公钥删除）",
                    "临时身份：给「客户端」使用的应急 nodekey（本机是服务端，它不是本机 token，不要填进远程设备）。公钥自动叠加进白名单；私钥复制后存密码管理器，没带电脑时在别的机器用 PROXYD_CLIENT_KEY=<私钥> 连入本机；重置只换这一对，不影响手动添加的白名单",
                  ],
                  note: "token 即连接凭据，泄露等于端口暴露，请只发给可信对端。",
                }}
              />
              <div className="flex flex-wrap items-center gap-3">
                <UISwitch checked={Boolean(status?.enabled)} className="mt-0 border-0 pt-0" label="启用远程连接服务（同时开启内嵌 SSH）" onCheckedChange={toggleEnabled} />
                {status?.running ? (
                  <StatusBadge ok text="运行中" />
                ) : status?.enabled ? (
                  <StatusBadge ok={false} text="配置已开启但未运行" />
                ) : (
                  <Badge variant="secondary">已停止</Badge>
                )}
              </div>
              <div className="mt-2 flex flex-wrap items-center gap-2">
                <UISwitch checked={Boolean(status?.builtin_ssh)} className="mt-0 border-0 pt-0" label="内嵌 SSH 服务（隧道 22 端口，无需系统 sshd）" onCheckedChange={setBuiltinSSH} />
                <span className="text-xs text-muted-foreground">{status?.ssh_auth_required ? "隧道认证 + SSH 公钥认证" : "隧道免密模式；在「访问授权」可额外启用 SSH 公钥认证"}</span>
              </div>
              <div className="mt-2 flex flex-wrap items-center gap-2">
                <UISwitch
                  checked={Boolean(status?.web_terminal)}
                  className="mt-0 border-0 pt-0"
                  label="Web Terminal（浏览器本机 shell）"
                  onCheckedChange={setWebTerminal}
                />
                {terminalAvailable && (
                  <Button size="sm" type="button" onClick={() => onOpenTerminal({})}>
                    <Terminal size={14} aria-hidden="true" />
                    <span>打开终端</span>
                  </Button>
                )}
                <span className="text-xs text-muted-foreground">默认关闭；会话以 proxyd 进程用户权限运行，独立于远程连接服务端</span>
              </div>
              {status?.web_terminal && status?.api_loopback === false && (
                <p className="permission-note warn">
                  高风险：API 当前监听 {status?.api_listen || "非回环地址"}，能访问控制台的客户端可能获得本机 shell。建议仅绑定 127.0.0.1 或置于可信鉴权边界后。
                </p>
              )}
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
            </section>            <section className="panel">
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
            </section></>);
}
