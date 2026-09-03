import { Zap } from "lucide-react";

/**
 * EmptyState 渲染空状态。
 *
 * 参数说明：
 * - title: string，主文案。
 * - detail: string，补充文案。
 *
 * 返回值说明：
 * 返回空状态 React 元素。
 *
 * 可能的异常/错误情况：
 * 无。
 */
export function EmptyState({ title, detail }) {
  return (
    <section className="empty-state">
      <Zap size={28} />
      <h2>{title}</h2>
      <p>{detail}</p>
    </section>
  );
}
