/** 设置分组导航只滚动当前页面，不修改用于业务路由的 hash。 */
import { SlidersHorizontal } from "lucide-react";

/**
 * SettingsJump 渲染页内快捷定位。
 * 参数：sections 为 Array<{id: string, label: string}>；返回 JSX。
 * 错误：目标不存在时忽略；使用按钮避免锚点 hash 被应用路由误判为未知页面。
 */
export function SettingsJump({ sections }) {
  return <nav className="settings-jump" aria-label="设置分组快速跳转">
    <span><SlidersHorizontal size={16} aria-hidden="true" />快速定位</span>
    {sections.map((section) => <button type="button" key={section.id} onClick={() => document.getElementById(section.id)?.scrollIntoView({ behavior: "smooth", block: "start" })}>{section.label}</button>)}
  </nav>;
}
