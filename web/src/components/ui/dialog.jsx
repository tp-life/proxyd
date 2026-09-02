/**
 * Radix 对话框适配层。
 *
 * 功能说明：
 * 提供普通对话框与破坏性操作确认框。焦点锁定、Esc 关闭、背景点击、焦点归还和
 * ARIA 关联全部交给 Radix 处理，业务页面只需要声明标题、描述与操作按钮。
 *
 * 可能的异常/错误情况：
 * DialogTitle 与 DialogDescription 不应同时省略，否则读屏软件无法解释弹层用途。
 */
import React, { forwardRef } from "react";
import { AlertDialog as RadixAlertDialog, Dialog as RadixDialog } from "radix-ui";
import { X } from "lucide-react";
import { cn } from "../../lib/utils";
import { Button } from "./button";

const Dialog = RadixDialog.Root;
const DialogTrigger = RadixDialog.Trigger;
const DialogClose = RadixDialog.Close;
const DialogTitle = RadixDialog.Title;
const DialogDescription = RadixDialog.Description;

/**
 * DialogOverlay 渲染模态背景并承载打开/关闭状态动画。
 *
 * 参数说明：
 * - className: string，调用方追加样式。
 * - 其余参数透传给 Radix Dialog.Overlay。
 *
 * 返回值说明：
 * 返回 Radix Overlay 元素。
 *
 * 可能的异常/错误情况：
 * 必须位于 Dialog Root 的上下文内使用，否则 Radix 无法读取打开状态。
 */
const DialogOverlay = forwardRef(function DialogOverlay({ className, ...props }, ref) {
  return <RadixDialog.Overlay ref={ref} className={cn("dialog-backdrop radix-dialog-overlay", className)} {...props} />;
});

/**
 * DialogContent 渲染带 Portal 的对话框主体。
 *
 * 参数说明：
 * - className: string，主体附加样式。
 * - children: ReactNode，对话框内容。
 * - showClose: boolean，是否显示右上角关闭按钮，默认显示。
 * - 其余参数透传给 Radix Dialog.Content。
 *
 * 返回值说明：
 * 返回 Portal、遮罩和对话框主体的组合。
 *
 * 可能的异常/错误情况：
 * 内容过高时由 CSS 限制视口高度并启用滚动；调用方仍需避免在弹层内嵌套另一套
 * 非 Radix 的焦点锁，以免形成键盘焦点冲突。
 */
const DialogContent = forwardRef(function DialogContent(
  { className, children, showClose = false, ...props },
  ref,
) {
  return (
    <RadixDialog.Portal>
      <DialogOverlay />
      <RadixDialog.Content ref={ref} className={cn("dialog radix-dialog-content", className)} {...props}>
        {children}
        {showClose ? (
          <RadixDialog.Close className="dialog-close" aria-label="关闭对话框">
            <X size={17} aria-hidden="true" />
          </RadixDialog.Close>
        ) : null}
      </RadixDialog.Content>
    </RadixDialog.Portal>
  );
});

/**
 * DialogHeader 提供标题区的稳定布局。
 *
 * 参数说明：className 为附加样式，children 为标题和说明。
 * 返回值说明：返回语义化 header 元素。
 * 可能的异常/错误情况：无。
 */
function DialogHeader({ className, children, ...props }) {
  return <header className={cn("dialog-header", className)} {...props}>{children}</header>;
}

/**
 * DialogFooter 提供操作区布局。
 *
 * 参数说明：className 为附加样式，children 为操作按钮。
 * 返回值说明：返回页脚容器。
 * 可能的异常/错误情况：无。
 */
function DialogFooter({ className, children, ...props }) {
  return <footer className={cn("dialog-footer", className)} {...props}>{children}</footer>;
}

/**
 * ConfirmDialog 渲染需要用户明确确认的操作。
 *
 * 参数说明：
 * - open: boolean，是否打开。
 * - onOpenChange: (open: boolean) => void，打开状态回调。
 * - title: ReactNode，操作标题。
 * - description: ReactNode，风险或后果说明。
 * - confirmLabel/cancelLabel: string，确认与取消按钮文案。
 * - onConfirm: () => void | Promise<void>，确认后的业务动作。
 * - destructive: boolean，是否使用危险操作视觉。
 * - loading: boolean，是否正在提交。
 *
 * 返回值说明：
 * 返回 Radix AlertDialog，确保危险操作不会因点击背景被意外确认或关闭。
 *
 * 可能的异常/错误情况：
 * onConfirm 内的业务错误由调用方捕获并反馈；组件不会吞掉异常。加载时会禁用操作，
 * 防止一次确认被重复提交。
 */
function ConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmLabel = "确认",
  cancelLabel = "取消",
  onConfirm,
  destructive = false,
  loading = false,
}) {
  return (
    <RadixAlertDialog.Root open={open} onOpenChange={onOpenChange}>
      <RadixAlertDialog.Portal>
        <RadixAlertDialog.Overlay className="dialog-backdrop radix-dialog-overlay" />
        <RadixAlertDialog.Content className="dialog radix-dialog-content" aria-busy={loading || undefined}>
          <header className="dialog-header">
            <RadixAlertDialog.Title>{title}</RadixAlertDialog.Title>
            {description ? <RadixAlertDialog.Description>{description}</RadixAlertDialog.Description> : null}
          </header>
          <footer className="dialog-footer">
            <RadixAlertDialog.Cancel asChild>
              <Button variant="secondary" disabled={loading}>{cancelLabel}</Button>
            </RadixAlertDialog.Cancel>
            <RadixAlertDialog.Action asChild>
              <Button
                variant={destructive ? "destructive" : "default"}
                loading={loading}
                onClick={onConfirm}
              >
                {confirmLabel}
              </Button>
            </RadixAlertDialog.Action>
          </footer>
        </RadixAlertDialog.Content>
      </RadixAlertDialog.Portal>
    </RadixAlertDialog.Root>
  );
}

export {
  ConfirmDialog,
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
};
