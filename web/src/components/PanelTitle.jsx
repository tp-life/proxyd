/**
 * PanelTitle 渲染面板标题。
 *
 * 参数说明：
 * - title: string，标题文本。
 * - detail: string，可选的当前区块说明，用于补充状态或业务边界。
 *
 * 返回值说明：
 * 返回标题 React 元素。
 *
 * 可能的异常/错误情况：
 * 无。
 */
export function PanelTitle({ title, detail }) {
  return (
    <div className="panel-heading">
      <h2 className="panel-title">{title}</h2>
      {detail && <p>{detail}</p>}
    </div>
  );
}
