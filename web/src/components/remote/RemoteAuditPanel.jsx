/** RemoteAuditPanel 负责audit任务面板，数据加载与配置事务由上层用例提供。 */

import { RefreshCw } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Table } from "@/components/ui/data-table";

import { EmptyState } from "@/components/EmptyState";

import { PanelTitle } from "@/components/PanelTitle";

import { tableViewportHeight } from "@/lib/format";

/**
 * RemoteAuditPanel 渲染独立任务视图。
 * 参数：解构字段为状态对象、表单值或事件回调，沿用 RemotePage 的接口约定。
 * 返回：React 元素；网络错误由用例回调处理，本组件不直接写配置。
 */
export function RemoteAuditPanel({ auditColumns, auditEntries, refreshAudit }) {
 return (<>            <section className="panel">
              <PanelTitle
                title="连接记录"
                detail="最近 100 条入站连接审计，独立于代理运行日志"
                help={{
                  heading: "连接审计说明",
                  paragraphs: ["记录建立、授权拒绝与断开事件，可复盘客户端、目标端口、持续时间和收发字节。"],
                  note: "记录只保存在内存环形缓冲中，最多约 500 条；服务重启后清空。",
                }}
              />
              <div className="mb-3 flex justify-end">
                <Button size="sm" variant="outline" type="button" onClick={refreshAudit}>
                  <RefreshCw size={14} aria-hidden="true" />
                  <span>刷新记录</span>
                </Button>
              </div>
              {(auditEntries || []).length === 0 ? (
                <EmptyState compact title="暂无连接记录" detail="有客户端连接、断开或被授权策略拒绝后，事件会显示在这里。" />
              ) : (
                <Table
                  className="data-table mt-3"
                  columns={auditColumns}
                  data={auditEntries}
                  emptyState="暂无连接记录"
                  getRowId={(row, index) => `${row.time}-${row.client_key || "unknown"}-${row.target_port}-${row.action}-${index}`}
                  height={tableViewportHeight(auditEntries.length, 420)}
                  minColumnWidth={68}
                  resizable
                />
              )}
            </section></>);
}
