import { Zap } from "lucide-react";

/**
 * EmptyState 渲染空状态。
 *
 * 参数说明：
 * - title: string，主文案。
 * - detail: string，补充文案。
 * - compact: boolean，面板内嵌的列表空态使用紧凑高度（不占满视口块）。
 *
 * 返回值说明：
 * 返回空状态 React 元素。
 *
 * 可能的异常/错误情况：
 * 无。
 */
export function EmptyState({ title, detail, compact = false }) {
  return (
    <section className={compact ? "empty-state compact" : "empty-state"}>
      <Zap size={compact ? 18 : 28} />
      <h2>{title}</h2>
      <p>{detail}</p>
    </section>
  );
}
