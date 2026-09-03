/**
 * Radix 标签页组件。
 *
 * 功能说明：
 * 使用 Tabs 表达「同一区域内在多组内容面板之间切换」的关系，非激活面板默认卸载，
 * 需要保留面板内部状态（如未提交的表单输入）时给 TabsContent 传 forceMount。
 * 与 SegmentedControl 的区别：后者是立即改变后端策略的互斥命令，这里只是切换
 * 页面内容的可见性，不产生任何副作用。
 *
 * 可能的异常/错误情况：
 * TabsTrigger 的 value 必须对应一个 TabsContent，否则该标签永远无内容可显示。
 */
import React, { forwardRef } from "react";
import { Tabs as RadixTabs } from "radix-ui";
import { cn } from "../../lib/utils";

/**
 * Tabs 标签页根组件。
 *
 * 参数说明：
 * - value/onValueChange: 受控选中项与切换回调（受控用法）。
 * - defaultValue: string，非受控用法下的初始选中项。
 * - className: string，附加样式。
 *
 * 返回值说明：返回 Radix Tabs.Root。
 * 可能的异常/错误情况：受控与非受控混用时以 Radix 自身警告为准。
 */
const Tabs = forwardRef(function Tabs({ className, ...props }, ref) {
  return <RadixTabs.Root ref={ref} className={cn("tabs", className)} {...props} />;
});

/**
 * TabsList 标签列表容器。
 *
 * 参数说明：
 * - className: string，附加样式。
 *
 * 返回值说明：返回 Radix Tabs.List，键盘左右方向键在标签间移动焦点。
 * 可能的异常/错误情况：无。
 */
const TabsList = forwardRef(function TabsList({ className, ...props }, ref) {
  return <RadixTabs.List ref={ref} className={cn("tabs-list", className)} {...props} />;
});

/**
 * TabsTrigger 单个标签按钮。
 *
 * 参数说明：
 * - value: string，与 TabsContent 对应的面板标识。
 * - className: string，附加样式。
 *
 * 返回值说明：返回 Radix Tabs.Trigger，选中态由 data-state="active" 驱动样式。
 * 可能的异常/错误情况：重复 value 会让多个标签指向同一面板，调用方应保证唯一。
 */
const TabsTrigger = forwardRef(function TabsTrigger({ className, ...props }, ref) {
  return <RadixTabs.Trigger ref={ref} className={cn("tabs-trigger", className)} {...props} />;
});

/**
 * TabsContent 标签页内容面板。
 *
 * 参数说明：
 * - value: string，与 TabsTrigger 对应的面板标识。
 * - forceMount: boolean，为 true 时非激活面板保留挂载（Radix 自动加 hidden），
 *   用于保留面板内的未提交输入等本地状态。
 * - className: string，附加样式。
 *
 * 返回值说明：返回 Radix Tabs.Content。
 * 可能的异常/错误情况：无。
 */
const TabsContent = forwardRef(function TabsContent({ className, ...props }, ref) {
  return <RadixTabs.Content ref={ref} className={cn("tabs-content", className)} {...props} />;
});

export { Tabs, TabsList, TabsTrigger, TabsContent };
