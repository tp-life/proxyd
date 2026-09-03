import { Copy } from "lucide-react";
import { classNames } from "@/lib/format";

/**
 * PortChip 渲染可复制端口 chip。
 *
 * 参数说明：
 * - label: string，端口说明。
 * - port: number，端口号。
 * - tone: string，视觉类型。
 * - onCopy: Function，复制回调。
 *
 * 返回值说明：
 * 返回端口 chip React 元素。
 *
 * 可能的异常/错误情况：
 * 复制失败由父组件处理。
 */
export function PortChip({ label, port, tone, onCopy }) {
  return (
    <button className={classNames("port-chip", tone)} type="button" onClick={() => onCopy(port)}>
      <span>{port}</span>
      <small>{label}</small>
      <Copy size={14} />
    </button>
  );
}
