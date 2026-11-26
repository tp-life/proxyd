/**
 * Badge 组件模块。
 *
 * 功能说明：
 * 提供新粗野主义标签（墨色描边、全大写），适合状态、标签、计数等轻量展示场景。
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
 * 对应参考设计的 .tag：实色底 + 2px 墨色描边，状态只用绿/红/薄荷三种底色区分。
 */
export const badgeVariants = cva(
  [
    // 参考设计的 .tag：2px 墨色描边、11px 半粗、全大写、0.06em 字距，零圆角。
    "inline-flex items-center gap-1.5 border-2 border-ink px-2.5 py-1 text-[11px] font-semibold uppercase leading-4 tracking-[0.06em] whitespace-nowrap",
    "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ink",
  ].join(" "),
  {
    variants: {
      variant: {
        default: "bg-mint text-ink",
        secondary: "bg-secondary text-ink",
        success: "bg-success text-white",
        destructive: "bg-destructive text-white",
        outline: "bg-card text-ink",
      },
    },
    defaultVariants: {
      variant: "default",
    },
  },
);

export const Badge = forwardRef(function Badge({ className, variant, ...props }, ref) {
  return <span ref={ref} className={cn(badgeVariants({ variant }), className)} {...props} />;
});

Badge.displayName = "Badge";
