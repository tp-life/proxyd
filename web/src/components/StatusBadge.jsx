import { Badge } from "@/components/ui/badge";

/**
 * StatusBadge 渲染状态徽标。
 *
 * 参数说明：
 * - ok: boolean，是否为正常状态。
 * - text: string，展示文本。
 *
 * 返回值说明：
 * 返回状态徽标 React 元素。
 *
 * 可能的异常/错误情况：
 * 无。
 */
export function StatusBadge({ ok, text }) {
  return <Badge variant={ok ? "success" : "destructive"}>{text}</Badge>;
}
