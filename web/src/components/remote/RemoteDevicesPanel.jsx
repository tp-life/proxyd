/** RemoteDevicesPanel 负责devices任务面板，数据加载与配置事务由上层用例提供。 */

import { Plus } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Table } from "@/components/ui/data-table";

import { Switch as UISwitch } from "@/components/ui/switch";

import { EmptyState } from "@/components/EmptyState";
import { Field } from "@/components/Field";

import { PanelTitle } from "@/components/PanelTitle";

import { tableViewportHeight } from "@/lib/format";
import { RemoteProbeDetail } from "./RemoteConnectionTools";

/**
 * RemoteDevicesPanel 渲染独立任务视图。
 * 参数：解构字段为状态对象、表单值或事件回调，沿用 RemotePage 的接口约定。
 * 返回：React 元素；网络错误由用例回调处理，本组件不直接写配置。
 */
export function RemoteDevicesPanel({ expandedRemote, probeRemote, remoteColumns, remoteForm, remoteProbes, remotes, setRemoteForm, setSshSetEnvTerm, sshSetEnvTerm, submitRemote }) {
 return (<>            <section className="panel">
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
                  isRowExpanded={(row) => expandedRemote === row.name}
                  minColumnWidth={88}
                  renderExpandedRow={(row) => (
                    <RemoteProbeDetail
                      name={row.name}
                      probe={remoteProbes?.[row.name]}
                      onRefresh={() => probeRemote(row.name)}
                    />
                  )}
                  resizable
                />
              )}
            </section></>);
}
