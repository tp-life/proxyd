/**
 * 通用 UI 工具模块。
 *
 * 功能说明：
 * 为 Radix UI 适配层提供 class 合并、事件串联与 ref 组合能力。保留这些小型工具，
 * 是为了让 primitive 能接入现有 React 页面，而不把业务状态写入基础组件内部。
 *
 * 参数说明：
 * 模块本身无入参；各导出函数分别接收 class、事件处理器或 React ref。
 *
 * 返回值说明：
 * 返回可复用的工具函数。
 *
 * 可能的异常/错误情况：
 * class 与 ref 的空值会被忽略；事件处理器抛出的异常会继续向上冒泡。
 */
import { clsx } from "clsx";
import { twMerge } from "tailwind-merge";

/**
 * cn 合并条件 class 与冲突的 Tailwind class。
 *
 * 参数说明：
 * - inputs: Array<unknown>，任意可被 clsx 接受的 class 输入。
 *
 * 返回值说明：
 * 返回最终 className 字符串。
 *
 * 可能的异常/错误情况：
 * 无；空值和不生效的条件项会被 clsx 忽略。
 */
export function cn(...inputs) {
  return twMerge(clsx(inputs));
}

/**
 * composeRefs 把多个 React ref 写入同一个真实 DOM 节点。
 *
 * 参数说明：
 * - refs: Array<Function | {current: unknown} | null>，需要同步的 ref。
 *
 * 返回值说明：
 * 返回接收 DOM 节点的 ref 回调函数。
 *
 * 可能的异常/错误情况：
 * 无；无法识别或为空的 ref 会被安全忽略。
 */
export function composeRefs(...refs) {
  return function setComposedRef(node) {
    refs.forEach((ref) => {
      if (typeof ref === "function") {
        ref(node);
        return;
      }
      if (ref && typeof ref === "object") {
        ref.current = node;
      }
    });
  };
}

/**
 * callAll 把多个事件处理器组合成一个按顺序执行的处理器。
 *
 * 参数说明：
 * - handlers: Array<Function | null>，需要串联的事件处理器。
 *
 * 返回值说明：
 * 返回透传原始事件参数的组合函数。
 *
 * 可能的异常/错误情况：
 * 某个处理器抛错时不会吞掉异常，后续行为遵循 React 默认错误处理流程。
 */
export function callAll(...handlers) {
  return function invokeAllHandlers(...args) {
    handlers.forEach((handler) => {
      if (typeof handler === "function") {
        handler(...args);
      }
    });
  };
}
