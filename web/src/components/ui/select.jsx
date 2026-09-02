/**
 * Radix Select 适配层。
 *
 * 功能说明：
 * 替代所有原生下拉框，统一键盘导航、Portal 浮层、焦点状态和选中标记。业务层通过
 * options 数组声明选项，避免每个页面重复拼装 Radix 的复合组件。
 *
 * 可能的异常/错误情况：
 * Radix 不允许 Select.Item 使用空字符串值，因此本模块用内部哨兵值映射空选项；
 * 对业务层仍保持空字符串输入输出，避免改变已有 API 参数语义。
 */
import React, { forwardRef } from "react";
import { Select as RadixSelect } from "radix-ui";
import { Check, ChevronDown, ChevronUp } from "lucide-react";
import { cn } from "../../lib/utils";

const EMPTY_VALUE = "__proxyd_radix_empty__";

/**
 * toRadixValue 把业务值转换为 Radix 可接受的非空值。
 *
 * 参数说明：value 为 string/number/null/undefined 的业务选项值。
 * 返回值说明：空值返回内部哨兵，其他值返回字符串。
 * 可能的异常/错误情况：若业务值恰好等于内部哨兵会产生冲突；该保留值不应作为
 * 用户可见配置值使用。
 */
function toRadixValue(value) {
  return value === "" || value == null ? EMPTY_VALUE : String(value);
}

/**
 * fromRadixValue 把 Radix 值还原为业务值。
 *
 * 参数说明：value 为 Radix 返回的字符串。
 * 返回值说明：内部哨兵返回空字符串，其他值保持不变。
 * 可能的异常/错误情况：无。
 */
function fromRadixValue(value) {
  return value === EMPTY_VALUE ? "" : value;
}

/**
 * SelectItem 渲染单个下拉选项。
 *
 * 参数说明：option 包含 value、label 与可选 disabled，className 为附加样式。
 * 返回值说明：返回带选中指示器的 Radix Select.Item。
 * 可能的异常/错误情况：重复 value 会让 Radix 无法稳定区分选项。
 */
function SelectItem({ option, className }) {
  return (
    <RadixSelect.Item
      value={toRadixValue(option.value)}
      disabled={option.disabled}
      className={cn("radix-select-item", className)}
    >
      <RadixSelect.ItemText>{option.label}</RadixSelect.ItemText>
      <RadixSelect.ItemIndicator className="radix-select-indicator">
        <Check size={15} aria-hidden="true" />
      </RadixSelect.ItemIndicator>
    </RadixSelect.Item>
  );
}

/**
 * Select 渲染控制台标准下拉选择器。
 *
 * 参数说明：
 * - value: string，当前业务值，可为空字符串。
 * - onValueChange: (value: string) => void，选择变化回调。
 * - options: Array<{value: string, label: ReactNode, disabled?: boolean}>，选项列表。
 * - placeholder: string，没有值时的占位文案。
 * - ariaLabel: string，读屏名称。
 * - disabled: boolean，是否禁用。
 * - className/triggerClassName/contentClassName: string，各层附加样式。
 *
 * 返回值说明：
 * 返回完整的 Radix Select Root、Trigger、Portal 与 Content。
 *
 * 可能的异常/错误情况：
 * onValueChange 内的请求错误由页面负责显示；选项列表为空时只会显示 placeholder。
 */
const Select = forwardRef(function Select(
  {
    value,
    onValueChange,
    options = [],
    placeholder = "请选择",
    ariaLabel,
    disabled = false,
    className,
    triggerClassName,
    contentClassName,
  },
  ref,
) {
  return (
    <div className={cn("radix-select", className)}>
      <RadixSelect.Root
        value={toRadixValue(value)}
        onValueChange={(nextValue) => onValueChange?.(fromRadixValue(nextValue))}
        disabled={disabled}
      >
        <RadixSelect.Trigger
          ref={ref}
          className={cn("radix-select-trigger", triggerClassName)}
          aria-label={ariaLabel}
        >
          <RadixSelect.Value placeholder={placeholder} />
          <RadixSelect.Icon className="radix-select-icon">
            <ChevronDown size={16} aria-hidden="true" />
          </RadixSelect.Icon>
        </RadixSelect.Trigger>
        <RadixSelect.Portal>
          <RadixSelect.Content
            position="popper"
            sideOffset={6}
            className={cn("radix-select-content", contentClassName)}
          >
            <RadixSelect.ScrollUpButton className="radix-select-scroll-button">
              <ChevronUp size={16} aria-hidden="true" />
            </RadixSelect.ScrollUpButton>
            <RadixSelect.Viewport className="radix-select-viewport">
              {options.map((option) => <SelectItem key={toRadixValue(option.value)} option={option} />)}
            </RadixSelect.Viewport>
            <RadixSelect.ScrollDownButton className="radix-select-scroll-button">
              <ChevronDown size={16} aria-hidden="true" />
            </RadixSelect.ScrollDownButton>
          </RadixSelect.Content>
        </RadixSelect.Portal>
      </RadixSelect.Root>
    </div>
  );
});

export { Select };
