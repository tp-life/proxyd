/**
 * PageHeader 为所有桌面业务页面提供统一的标题、说明和操作区。
 *
 * 功能说明：
 * 使用固定的信息层级替代各页面自行拼装的标题栏，避免标题字号、上下间距和
 * 操作按钮位置在页面切换时跳动。eyebrow 负责建立模块语境，children 只承载
 * 当前页面最重要的操作，次要操作继续留在对应内容面板内。
 *
 * 参数说明：
 * - eyebrow: string，页面所属功能分组的短标签。
 * - title: string，页面唯一的一级标题。
 * - detail: string，说明页面职责和数据边界的简短文本。
 * - children: React.ReactNode，可选的页面级操作控件。
 *
 * 返回值说明：
 * 返回语义化 header React 元素。
 *
 * 可能的异常/错误情况：
 * 无；缺失 eyebrow、detail 或 children 时自动省略对应区域，不影响页面主体。
 */
export function PageHeader({ eyebrow, title, detail, children }) {
  return (
    <header className="page-header">
      <div className="page-header-copy">
        {eyebrow && <span className="page-kicker">{eyebrow}</span>}
        <h1>{title}</h1>
        {detail && <p>{detail}</p>}
      </div>
      {children && <div className="page-header-actions">{children}</div>}
    </header>
  );
}
