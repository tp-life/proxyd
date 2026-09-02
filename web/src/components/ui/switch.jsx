/**
 * Radix Switch 适配层。
 *
 * 功能说明：
 * 统一布尔设置的键盘语义、标签关联与视觉状态。业务层继续使用 checked 和
 * onCheckedChange，不需要感知 Radix 的 DOM 结构。
 *
 * 可能的异常/错误情况：
 * 受控模式下如果调用方没有更新 checked，开关会保持原状态，这是 React 受控组件
 * 的预期行为。
 */
import React, { forwardRef, useId } from "react";
import { Switch as RadixSwitch } from "radix-ui";
import { cn } from "../../lib/utils";

/**
 * Switch 渲染带可选文字标签的布尔开关。
 *
 * 参数说明：
 * - checked: boolean，当前开关值。
 * - onCheckedChange: (checked: boolean) => void，值变化回调。
 * - label: ReactNode，可见标签；省略时必须提供 ariaLabel。
 * - ariaLabel: string，无可见标签时给读屏软件使用的名称。
 * - disabled: boolean，是否禁止交互。
 * - className: string，根标签附加样式。
 *
 * 返回值说明：
 * 返回 label 与 Radix Switch.Root 的组合。
 *
 * 可能的异常/错误情况：
 * 同时省略 label 与 ariaLabel 会产生无障碍名称缺失，调用方必须至少提供一个。
 */
const Switch = forwardRef(function Switch(
  { checked, onCheckedChange, label, ariaLabel, disabled = false, className, id, ...props },
  ref,
) {
  const generatedId = useId();
  const controlId = id || generatedId;
  return (
    <label className={cn("switch-row", className)} htmlFor={controlId}>
      {label ? <span>{label}</span> : null}
      <RadixSwitch.Root
        ref={ref}
        id={controlId}
        className="switch radix-switch"
        checked={checked}
        onCheckedChange={onCheckedChange}
        disabled={disabled}
        aria-label={ariaLabel || (typeof label === "string" ? label : undefined)}
        {...props}
      >
        <RadixSwitch.Thumb className="switch-thumb radix-switch-thumb" />
      </RadixSwitch.Root>
    </label>
  );
});

export { Switch };
