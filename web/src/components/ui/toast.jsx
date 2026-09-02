/**
 * Radix Toast 通知模块。
 *
 * 功能说明：
 * 提供轻量通知队列与展示视口。自动关闭、键盘热键、滑动关闭和无障碍播报由 Radix
 * 负责，业务层只维护消息数据并决定成功或错误语义。
 *
 * 可能的异常/错误情况：
 * Toast 不适合展示必须确认的错误；不可恢复或破坏性操作仍应使用 AlertDialog。
 */
import React, { useCallback, useState } from "react";
import { Toast as RadixToast } from "radix-ui";
import { X } from "lucide-react";
import { cn } from "../../lib/utils";

let toastSequence = 0;

/**
 * buildToastId 生成当前浏览器会话内稳定唯一的通知标识。
 *
 * 参数说明：无。
 * 返回值说明：返回带递增序号的字符串。
 * 可能的异常/错误情况：理论上仅在超长会话导致数字溢出时可能重复，实际不可达。
 */
function buildToastId() {
  toastSequence += 1;
  return `radix-toast-${toastSequence}`;
}

/**
 * useToastQueue 管理通知数据。
 *
 * 参数说明：
 * - defaultDuration: number，默认展示毫秒数。
 * - limit: number，队列最多保留条数。
 *
 * 返回值说明：
 * 返回 `{toasts, showToast, dismissToast}`，供应用根组件统一挂载。
 *
 * 可能的异常/错误情况：
 * 非法 duration 会回退默认值；limit 小于 1 时仍至少保留一条消息。
 */
function useToastQueue({ defaultDuration = 4000, limit = 5 } = {}) {
  const [toasts, setToasts] = useState([]);

  /**
   * dismissToast 从队列移除指定通知。
   *
   * 参数说明：id 为通知标识。
   * 返回值说明：无。
   * 可能的异常/错误情况：id 不存在时静默忽略，保证重复关闭是幂等的。
   */
  const dismissToast = useCallback(function dismissToast(id) {
    setToasts((current) => current.filter((toast) => toast.id !== id));
  }, []);

  /**
   * showToast 新增或替换一条通知。
   *
   * 参数说明：input 可为标题字符串，或包含 title、description、status、duration 的对象。
   * 返回值说明：返回通知 id，调用方可据此主动关闭。
   * 可能的异常/错误情况：空对象会生成没有正文的通知；业务层应至少提供 title 或
   * description，避免无意义的无障碍播报。
   */
  const showToast = useCallback(function showToast(input) {
    const payload = typeof input === "string" ? { title: input } : { ...(input || {}) };
    const id = payload.id || buildToastId();
    const duration = Number.isFinite(Number(payload.duration)) ? Number(payload.duration) : defaultDuration;
    const nextToast = { status: "default", ...payload, id, duration };
    setToasts((current) => [nextToast, ...current.filter((toast) => toast.id !== id)].slice(0, Math.max(1, limit)));
    return id;
  }, [defaultDuration, limit]);

  return { dismissToast, showToast, toasts };
}

/**
 * ToastViewport 渲染通知队列。
 *
 * 参数说明：
 * - toasts: Array<object>，当前消息。
 * - onDismiss: (id: string) => void，关闭回调。
 * - maxVisible: number，屏幕上最多同时显示条数。
 *
 * 返回值说明：
 * 返回 Radix Toast.Provider、Root 列表和固定定位 Viewport。
 *
 * 可能的异常/错误情况：
 * toasts 非数组时按空数组处理；onDismiss 缺失时通知仍会视觉关闭，但业务队列无法
 * 及时移除，因此调用方应始终提供回调。
 */
function ToastViewport({ toasts, onDismiss, maxVisible = 3 }) {
  const visibleToasts = Array.isArray(toasts) ? toasts.slice(0, Math.max(1, maxVisible)) : [];
  return (
    <RadixToast.Provider swipeDirection="right">
      {visibleToasts.map((toast) => (
        <RadixToast.Root
          key={toast.id}
          className={cn("radix-toast", `radix-toast-${toast.status || toast.tone || "default"}`)}
          duration={toast.duration}
          onOpenChange={(open) => { if (!open) onDismiss?.(toast.id); }}
        >
          <div className="radix-toast-copy">
            {toast.title ? <RadixToast.Title>{toast.title}</RadixToast.Title> : null}
            {toast.description ? <RadixToast.Description>{toast.description}</RadixToast.Description> : null}
          </div>
          <RadixToast.Close className="radix-toast-close" aria-label="关闭通知">
            <X size={16} aria-hidden="true" />
          </RadixToast.Close>
        </RadixToast.Root>
      ))}
      <RadixToast.Viewport className="radix-toast-viewport" hotkey={["F8"]} label="通知" />
    </RadixToast.Provider>
  );
}

export { ToastViewport, useToastQueue };
