import { classNames } from "@/lib/format";

/**
 * Field 为表单控件提供不会随输入内容消失的标签和辅助状态。
 *
 * 参数说明：
 * - label: string，字段的固定中文名称。
 * - hint: string，可选的格式、边界或当前状态说明。
 * - compact: boolean，是否使用工具栏中的紧凑排版。
 * - children: React.ReactNode，实际输入、选择器或其他表单控件。
 *
 * 返回值说明：
 * 返回包含可点击标签、控件和辅助文本的 React 元素。
 *
 * 可能的异常/错误情况：
 * 无；输入校验和提交错误仍由对应业务表单及 API 调用处理。
 */
export function Field({ label, hint, compact = false, children }) {
  return (
    <label className={classNames("field", compact && "compact")}>
      <span>{label}</span>
      {children}
      {hint && <small>{hint}</small>}
    </label>
  );
}
