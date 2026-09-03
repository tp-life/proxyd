import { useCallback } from "react";
import { useToastQueue } from "@/components/ui/toast";

/**
 * useToast 管理短暂展示的 toast 消息。
 *
 * 参数说明：
 * 无。
 *
 * 返回值说明：
 * 返回 toast 队列、展示方法和关闭方法，供页面统一挂载通知栈。
 *
 * 可能的异常/错误情况：
 * 无；重复调用会按时间倒序入队，并受队列长度上限约束。
 */
export function useToast() {
  const { toasts, showToast: showRadixToast, dismissToast } = useToastQueue({
    defaultDuration: 3600,
    limit: 4,
  });
  const showToast = useCallback((message, type = "ok") => {
    showRadixToast({
      status: type === "err" ? "error" : "success",
      title: type === "err" ? "操作未完成" : "操作完成",
      description: message,
    });
  }, [showRadixToast]);
  return { dismissToast, showToast, toasts };
}
