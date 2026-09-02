/**
 * Radix 按钮适配层。
 *
 * 功能说明：
 * 统一控制台按钮的尺寸、视觉变体、加载状态与组合能力。Radix Primitives 没有
 * 预设 Button，因此这里使用 Slot 提供 `asChild` 语义，让链接等原生元素能够继承
 * 按钮样式，同时避免嵌套交互元素造成可访问性问题。
 *
 * 可能的异常/错误情况：
 * `asChild` 为 true 时必须只传入一个可接收 ref 的 React 元素；否则 React 会在
 * 开发环境中报告 Slot 组合错误。
 */
import React, { forwardRef } from "react";
import { Slot } from "radix-ui";
import { LoaderCircle } from "lucide-react";
import { cn } from "../../lib/utils";

const VARIANT_CLASS = {
  default: "btn-primary",
  secondary: "btn-secondary",
  outline: "btn-outline",
  ghost: "btn-ghost",
  destructive: "btn-danger",
  "destructive-ghost": "btn-danger-ghost",
};

const SIZE_CLASS = {
  sm: "btn-sm",
  md: "",
  lg: "btn-lg",
  icon: "btn-icon",
};

/**
 * Button 渲染控制台标准按钮。
 *
 * 参数说明：
 * - variant: string，视觉语义，可选 default/secondary/outline/ghost/destructive。
 * - size: string，尺寸语义，可选 sm/md/lg/icon。
 * - asChild: boolean，是否通过 Radix Slot 把行为与样式合并到唯一子元素。
 * - loading: boolean，是否显示加载图标并阻止重复提交。
 * - disabled: boolean，是否由业务逻辑禁用按钮。
 * - className: string，调用方追加的样式类。
 * - children: ReactNode，按钮正文。
 *
 * 返回值说明：
 * 返回原生 button，或在 `asChild` 模式下返回被 Slot 增强的唯一子元素。
 *
 * 可能的异常/错误情况：
 * `asChild` 模式不负责把 disabled 语义传递给不支持该属性的元素；链接禁用状态
 * 应由调用方避免提供可导航 href。
 */
const Button = forwardRef(function Button(
  {
    variant = "default",
    size = "md",
    asChild = false,
    loading = false,
    disabled = false,
    className,
    children,
    type = "button",
    ...props
  },
  ref,
) {
  const classes = cn("btn", VARIANT_CLASS[variant] || VARIANT_CLASS.default, SIZE_CLASS[size] || "", className);

  /*
   * Radix Slot 必须只接收一个可被克隆的 React 元素。加载图标若与链接子元素并列，
   * 即使条件为 false 也会让 Slot 看到多个 children 并抛错，所以组合模式单独返回，
   * 且不在 Slot 外层注入额外节点。链接型按钮当前不承担异步提交，loading 仍保留
   * aria-busy 供调用方表达状态，但不会破坏唯一子元素约束。
   */
  if (asChild) {
    return (
      <Slot.Root
        ref={ref}
        className={classes}
        aria-busy={loading || undefined}
        aria-disabled={disabled || undefined}
        {...props}
      >
        {children}
      </Slot.Root>
    );
  }

  return (
    <button
      ref={ref}
      className={classes}
      aria-busy={loading || undefined}
      type={type}
      disabled={disabled || loading}
      {...props}
    >
      {loading ? <LoaderCircle className="btn-spinner" aria-hidden="true" /> : null}
      {children}
    </button>
  );
});

/**
 * ButtonLink 把外部或下载链接渲染成按钮外观。
 *
 * 参数说明：
 * - href: string，目标地址。
 * - target: string，可选浏览上下文；`_blank` 会自动补充安全 rel。
 * - rel: string，可选链接关系。
 * - 其余参数与 Button 一致。
 *
 * 返回值说明：
 * 返回由 Radix Slot 组合的 `<a>` 元素。
 *
 * 可能的异常/错误情况：
 * href 无效时浏览器无法导航；组件不会自行请求或验证外部资源。
 */
const ButtonLink = forwardRef(function ButtonLink({ href, target, rel, children, ...props }, ref) {
  const safeRel = rel || (target === "_blank" ? "noreferrer noopener" : undefined);
  return (
    <Button ref={ref} asChild {...props}>
      <a href={href} target={target} rel={safeRel}>
        {children}
      </a>
    </Button>
  );
});

export { Button, ButtonLink };
