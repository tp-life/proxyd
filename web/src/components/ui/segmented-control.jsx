/**
 * Radix 单选分段控制器。
 *
 * 功能说明：
 * 使用 ToggleGroup 表达“多个互斥策略中只能选择一个”的关系。这里不使用 Tabs，
 * 因为规则/全局/直连不是切换内容面板，而是立即改变后端策略的单选命令。
 *
 * 可能的异常/错误情况：
 * Radix ToggleGroup 允许再次点击已选项得到空值；组件会忽略该空值，避免系统进入
 * 后端不支持的“没有模式”状态。
 */
import React, { forwardRef } from "react";
import { ToggleGroup } from "radix-ui";
import { cn } from "../../lib/utils";

/**
 * SegmentedControl 渲染互斥选项。
 *
 * 参数说明：
 * - value: string，当前值。
 * - onChange: (value: string) => void，选中有效新值后的回调。
 * - options: Array<{value: string, label: ReactNode, disabled?: boolean}>，候选项。
 * - ariaLabel: string，整组控件的无障碍名称。
 * - disabled: boolean，是否禁用整组控件。
 * - className: string，附加样式。
 *
 * 返回值说明：
 * 返回 Radix ToggleGroup.Root 及其 Item。
 *
 * 可能的异常/错误情况：
 * options 中重复 value 会导致 React key 与选择状态冲突，调用方应保证 value 唯一。
 */
const SegmentedControl = forwardRef(function SegmentedControl(
  { value, onChange, onValueChange, options, ariaLabel, disabled = false, className },
  ref,
) {
  /**
   * handleValueChange 过滤 Radix 的空选择。
   *
   * 参数说明：nextValue 为用户交互后的字符串值。
   * 返回值说明：无；仅在值非空且不同于当前值时调用 onChange。
   * 可能的异常/错误情况：onChange 内的异步错误由业务页面负责反馈。
   */
  function handleValueChange(nextValue) {
    if (!nextValue || nextValue === value) return;
    (onValueChange || onChange)?.(nextValue);
  }

  return (
    <ToggleGroup.Root
      ref={ref}
      type="single"
      value={value}
      onValueChange={handleValueChange}
      className={cn("segmented", "radix-segmented", className)}
      aria-label={ariaLabel}
      disabled={disabled}
    >
      {options.map((option) => (
        <ToggleGroup.Item
          key={option.value}
          value={option.value}
          disabled={disabled || option.disabled}
          className="segmented-item"
        >
          {option.label}
        </ToggleGroup.Item>
      ))}
    </ToggleGroup.Root>
  );
});

export { SegmentedControl };
