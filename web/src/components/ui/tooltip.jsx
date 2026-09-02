/**
 * Radix Tooltip 适配层。
 *
 * 功能说明：
 * 为图标按钮和被截断文本提供键盘、鼠标一致的补充说明，并把浮层 Portal 到文档
 * 根节点，避免被卡片的 overflow 裁切。
 *
 * 可能的异常/错误情况：
 * Tooltip 只适合补充信息，不能承载必须操作或必须阅读的业务内容。
 */
import React, { forwardRef } from "react";
import { Tooltip as RadixTooltip } from "radix-ui";
import { cn } from "../../lib/utils";

const TooltipProvider = RadixTooltip.Provider;
const Tooltip = RadixTooltip.Root;
const TooltipTrigger = RadixTooltip.Trigger;

/**
 * TooltipContent 渲染提示内容和指向箭头。
 *
 * 参数说明：
 * - className: string，附加样式。
 * - sideOffset: number，提示框与触发器之间的像素距离。
 * - children: ReactNode，提示文本。
 * - 其余参数透传给 Radix Tooltip.Content。
 *
 * 返回值说明：
 * 返回 Portal 化的 Tooltip 内容。
 *
 * 可能的异常/错误情况：
 * 内容过长会降低可读性；长篇说明应改用页面内帮助块或对话框。
 */
const TooltipContent = forwardRef(function TooltipContent(
  { className, sideOffset = 8, children, ...props },
  ref,
) {
  return (
    <RadixTooltip.Portal>
      <RadixTooltip.Content
        ref={ref}
        sideOffset={sideOffset}
        className={cn("tooltip-content radix-tooltip-content", className)}
        {...props}
      >
        {children}
        <RadixTooltip.Arrow className="radix-tooltip-arrow" />
      </RadixTooltip.Content>
    </RadixTooltip.Portal>
  );
});

export { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger };
