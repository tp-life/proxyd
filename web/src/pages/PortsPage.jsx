import { useMemo } from "react";
import { Copy, Terminal } from "lucide-react";
import { Table } from "@/components/ui/data-table";
import { Select } from "@/components/ui/select";
import { Switch as UISwitch } from "@/components/ui/switch";
import { Field } from "@/components/Field";
import { PageHeader } from "@/components/PageHeader";
import { StatusBadge } from "@/components/StatusBadge";
import { classNames, delayClass, formatDelay, sortByDelay, tableViewportHeight } from "@/lib/format";

/**
 * PortsPage 渲染端口映射表。
 *
 * 参数说明：
 * - overview: object，概览数据。
 * - portSort: string，当前排序模式。
 * - onCopy/onSort/onToggle: Function，复制、排序与映射开关回调。
 *
 * 返回值说明：
 * 返回端口页 React 元素。
 *
 * 可能的异常/错误情况：
 * 剪贴板错误由父组件处理。
 */
export function PortsPage({ overview, portSort, onCopy, onCopyEnv, onSort, onToggle }) {
  const sourcePorts = overview.port_mapping_enabled ? overview.ports : (overview.port_assignments || []);
  const ports = portSort === "delay" ? sortByDelay(sourcePorts, "delay") : sourcePorts;
  const columns = useMemo(
    () => [
      {
        key: "port",
        header: "端口",
        sortable: true,
        width: "180px",
        cell: (port) => (
          <span className="port-cell">
            <button className="copy-link" disabled={!overview.port_mapping_enabled} type="button" onClick={() => onCopy(port.port)}>
              {port.port}<Copy size={14} aria-hidden="true" />
            </button>
            <button
              aria-label={`复制端口 ${port.port} 的环境变量`}
              className="copy-link"
              disabled={!overview.port_mapping_enabled}
              title="复制 http_proxy/https_proxy/all_proxy 环境变量"
              type="button"
              onClick={() => onCopyEnv(port.port)}
            >
              <Terminal size={14} aria-hidden="true" />
            </button>
          </span>
        ),
      },
      { key: "node", header: "节点", sortable: true, width: "36%", cell: (port) => <span className="node-name" title={port.node}>{port.node}</span> },
      { key: "subscription", header: "订阅", sortable: true, width: "24%" },
      {
        key: "delay",
        header: "延迟",
        sortable: true,
        width: "110px",
        cell: (port) => <span className={delayClass(port)}>{formatDelay(port)}</span>,
        sortValue: (port) => port.alive && port.delay > 0 ? port.delay : Number.POSITIVE_INFINITY,
      },
      {
        key: "alive",
        header: "状态",
        sortable: true,
        width: "110px",
        cell: (port) => <StatusBadge ok={overview.port_mapping_enabled && port.alive} text={!overview.port_mapping_enabled ? "未监听" : port.alive ? "可用" : "失效"} />,
        sortValue: (port) => port.alive ? 1 : 0,
      },
    ],
    [onCopy, onCopyEnv, overview.port_mapping_enabled],
  );
  return (
    <div className="stack">
      <PageHeader eyebrow="网络入口" title="代理入口" detail="管理健康节点的一对一监听，关闭后仍保留稳定端口分配。">
        <UISwitch checked={Boolean(overview.port_mapping_enabled)} label="启用节点端口映射" onCheckedChange={onToggle} />
      </PageHeader>
      <section className="panel compact-panel">
      <div className="toolbar ports-toolbar">
        <div className={classNames("port-mapping-state", overview.port_mapping_enabled ? "enabled" : "disabled")}>
          <span>{overview.port_mapping_enabled ? "正在监听" : "已停止监听"}</span>
          <b>{ports.length} 个稳定分配</b>
        </div>
        <Field compact label="排列方式">
          <Select
            ariaLabel="代理入口排列方式"
            value={portSort}
            onValueChange={onSort}
            options={[{ value: "default", label: "按端口" }, { value: "delay", label: "按延迟" }]}
          />
        </Field>
      </div>
      <Table
        className="data-table mt-3"
        columns={columns}
        data={ports}
        emptyState={overview.port_mapping_enabled ? "暂无健康节点映射" : "暂无可保留的稳定分配"}
        getRowId={(port, index) => String(port.port || index)}
        height={tableViewportHeight(ports.length, 880)}
        minColumnWidth={88}
        resizable
      />
      {!overview.port_mapping_enabled && <p className="panel-footnote">主代理端口、自动选优端口与策略分组端口不受此开关影响。</p>}
      {overview.port_mapping_enabled && (overview.subscriptions || []).some((sub) => sub.port_mapping === false) && (
        <p className="panel-footnote">部分订阅已在「订阅管理」页关闭端口映射：其节点不在监听列表中，但保留稳定端口分配并继续参与选路。</p>
      )}
      </section>
    </div>
  );
}
