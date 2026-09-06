/** RemoteForwardsPanel 负责forwards任务面板，数据加载与配置事务由上层用例提供。 */

import { Plus } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Table } from "@/components/ui/data-table";

import { Select } from "@/components/ui/select";

import { EmptyState } from "@/components/EmptyState";
import { Field } from "@/components/Field";

import { PanelTitle } from "@/components/PanelTitle";

import { tableViewportHeight } from "@/lib/format";

/**
 * RemoteForwardsPanel 渲染独立任务视图。
 * 参数：解构字段为状态对象、表单值或事件回调，沿用 RemotePage 的接口约定。
 * 返回：React 元素；网络错误由用例回调处理，本组件不直接写配置。
 */
export function RemoteForwardsPanel({ forwardColumns, forwardForm, forwards, remotes, setForwardForm, submitForward }) {
 return (<>            <section className="panel">
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
            </section></>);
}
