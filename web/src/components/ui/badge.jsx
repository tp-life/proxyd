/**
 * Badge 组件模块。
 *
 * 功能说明：
 * 提供常规 shadcn/new-york 风格徽标，适合状态、标签、计数等轻量展示场景。
 *
 * 参数说明：
 * 无模块级入参；对外导出 `badgeVariants` 与 `Badge`。
 *
 * 返回值说明：
 * 返回可复用的 React 徽标组件。
 *
 * 可能的异常/错误情况：
 * 无；组件只负责表现层渲染。
 */
import { cva } from "class-variance-authority";
import { forwardRef } from "react";
import { cn } from "../../lib/utils";

/**
 * `badgeVariants` 定义徽标视觉变体。
 *
 * 功能说明：
 * 维持与全局赛博朋克深色主题一致的描边霓虹基调，让徽标可以嵌入控制台型页面而不显得突兀。
 */
export const badgeVariants = cva(
  [
    "inline-flex items-center rounded-md border px-2.5 py-0.5 text-xs font-medium whitespace-nowrap",
    "transition-colors duration-200",
    "focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-ring/25",
  ].join(" "),
  {
    variants: {
      variant: {
        default: "border-primary/40 bg-primary/10 text-primary",
        secondary: "border-border bg-secondary text-secondary-foreground",
        success: "border-success/40 bg-success/10 text-success",
        destructive: "border-destructive/50 bg-destructive/15 text-destructive",
        outline: "border-border bg-card text-foreground",
      },
    },
    defaultVariants: {
      variant: "default",
    },
  },
);

/**
 * `Badge` 渲染轻量徽标。
 *
 * 功能说明：
 * 采用 `span` 实现，满足绝大多数只读标签场景；如果将来需要更复杂语义，
 * 可以在不破坏样式协议的前提下继续扩展为 `asChild`。
 *
 * 参数说明：
 * - className: string | undefined，额外样式类。
 * - variant: Badge 视觉变体名称。
 * - children: ReactNode，徽标内容。
 * - props: React.HTMLAttributes<HTMLSpanElement>，其余原生属性。
 *
 * 返回值说明：
 * - ReactElement，徽标节点。
 *
 * 可能的异常/错误情况：
 * - 不主动抛错；非法原生属性由 React 自行提示。
 */
export const Badge = forwardRef(function Badge({ className, variant, ...props }, ref) {
  return <span ref={ref} className={cn(badgeVariants({ variant }), className)} {...props} />;
});

Badge.displayName = "Badge";
