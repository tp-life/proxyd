/**
 * Input 组件模块。
 *
 * 功能说明：
 * 提供贴近 shadcn/new-york 风格的文本输入框，保持 Tailwind 4 本地源码可维护性，
 * 并为表单页保留一致的 focus、disabled、placeholder 表现。
 *
 * 参数说明：
 * 无模块级入参；对外导出 `Input`。
 *
 * 返回值说明：
 * 返回可复用的 React 输入组件。
 *
 * 可能的异常/错误情况：
 * 无；校验与业务规则由上层表单负责。
 */
import { forwardRef } from "react";
import { cn } from "../../lib/utils";

/**
 * `Input` 渲染基础文本输入框。
 *
 * 功能说明：
 * 组件只处理视觉和可访问性层面的通用行为，不在底层注入业务校验，
 * 这样可以避免 domain/application 规则泄漏到基础 UI 组件。
 *
 * 参数说明：
 * - className: string | undefined，额外样式类。
 * - type: string | undefined，原生 input 类型。
 * - props: React.InputHTMLAttributes<HTMLInputElement>，其余输入属性。
 *
 * 返回值说明：
 * - ReactElement，输入框节点。
 *
 * 可能的异常/错误情况：
 * - 原生浏览器输入异常按默认行为处理；本组件不主动抛错。
 */
export const Input = forwardRef(function Input({ className, type = "text", ...props }, ref) {
  return (
    <input
      ref={ref}
      type={type}
      className={cn(
        [
          "flex h-10 w-full min-w-0 rounded-md border border-neutral-200 bg-white px-3 py-2 text-sm text-neutral-950 shadow-xs",
          "transition-[border-color,box-shadow] duration-200",
          "placeholder:text-neutral-400",
          "focus-visible:border-neutral-400 focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-black/10",
          "disabled:cursor-not-allowed disabled:opacity-50",
          "aria-invalid:border-red-500 aria-invalid:ring-red-500/20",
        ].join(" "),
        className,
      )}
      {...props}
    />
  );
});

Input.displayName = "Input";
