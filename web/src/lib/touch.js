/**
 * 触摸手势基础能力模块。
 * iOS 与 iPadOS 的长按菜单和文本选择会抢占页面指针事件，因此拥有拖拽手势的控件需要显式关闭这些系统行为。
 * `-webkit-touch-callout` 只负责 WebKit 长按菜单，`user-select` 负责跨浏览器文本选择；
 * Android 链接菜单仍需调用方在 `onContextMenu` 中阻止默认行为，图片或链接的原生拖拽则需在子元素设置
 * `draggable={false}`，这里不会扩大控制范围。
 */

/**
 * 控件自身拥有手势时使用的样式类，例如滑块、拖拽手柄或长按按钮。
 * 它会始终禁止文本选择；只有控件同时接管滚动轴时，调用方才应额外组合 `touch-none`。
 */
export const TOUCH_GESTURE_CLASS = "select-none [-webkit-touch-callout:none]";

/**
 * 包裹业务内容的手势表面使用的样式类，例如滚动区、上下文菜单触发器或列表行。
 * 仅在粗指针设备上禁止选择，使鼠标用户仍能复制内容。混合输入设备无法只靠媒体查询准确判断当前手势，
 * 因此真正开始长按或拖拽时，应再配合 `holdSelection` 临时锁定选择。
 */
export const TOUCH_GESTURE_CONTENT_CLASS =
  "[-webkit-touch-callout:none] pointer-coarse:select-none";

/**
 * 在一次手势期间临时禁止目标元素的文本选择。
 *
 * @param {HTMLElement} element 需要在当前手势内锁定选择行为的元素。
 * @returns {() => void} 恢复元素原有选择行为的清理函数。
 * @throws 不主动抛出异常；调用方必须传入可写入 `style` 的 DOM 元素。
 *
 * 这里使用行内样式是为了覆盖设备级样式，并在手势结束时立即恢复复制能力。
 */
export function holdSelection(element) {
  element.style.setProperty("user-select", "none");
  element.style.setProperty("-webkit-user-select", "none");
  return () => {
    element.style.removeProperty("user-select");
    element.style.removeProperty("-webkit-user-select");
  };
}

/**
 * 尽力捕获指针，保证拖拽离开元素边界后仍能继续收到事件。
 *
 * @param {HTMLElement} element 发起指针捕获的元素。
 * @param {number} pointerId 浏览器分配的指针标识。
 * @returns {void} 无返回值。
 * @throws 不向外抛出异常；WebKit 中指针已失效时会静默降级，因为触摸指针通常已有隐式捕获。
 */
export function capturePointer(element, pointerId) {
  try {
    element.setPointerCapture(pointerId);
  } catch {
    // 指针可能已被系统接管；触摸输入仍可依赖浏览器的隐式捕获继续工作。
  }
}

/**
 * 释放先前建立的指针捕获。
 *
 * @param {HTMLElement} element 持有指针捕获的元素。
 * @param {number} pointerId 浏览器分配的指针标识。
 * @returns {void} 无返回值。
 * @throws 不向外抛出异常；捕获已由浏览器释放时会静默忽略。
 */
export function releasePointer(element, pointerId) {
  try {
    if (element.hasPointerCapture(pointerId)) {
      element.releasePointerCapture(pointerId);
    }
  } catch {
    // 浏览器可能已经释放捕获，此时无需再次处理。
  }
}

/**
 * 判断指针事件是否来自真实悬停，而非触摸或按压中的触控笔。
 *
 * @param {PointerEvent} event 待判断的指针事件。
 * @returns {boolean} 非触摸且当前没有按键按下时返回 `true`。
 * @throws 不主动抛出异常；调用方需传入包含 `pointerType` 与 `buttons` 的指针事件。
 *
 * 混合输入设备不能只依据媒体查询判断本次交互，因此这里直接使用事件数据作分流。
 */
export const isHoveringPointer = (event) => event.pointerType !== "touch" && event.buttons === 0;
