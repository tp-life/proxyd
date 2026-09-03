/**
 * PanelTitle 渲染面板标题。
 *
 * 参数说明：
 * - title: string，标题文本。
 * - detail: string，可选的当前区块说明，用于补充状态或业务边界。
 * - help: object，可选的详细帮助模型（heading、paragraphs、items、note），
 *   提供时标题旁出现「?」触发按钮，hover/聚焦/触屏点击时浮层展示具体使用方式。
 *
 * 返回值说明：
 * 返回标题 React 元素。
 *
 * 可能的异常/错误情况：
 * 无；help 缺失时仅渲染普通标题与摘要。
 */
import { CircleHelp } from "lucide-react";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";

export function PanelTitle({ title, detail, help }) {
  return (
    <div className="panel-heading">
      <div className="setting-title-line">
        <h2 className="panel-title">{title}</h2>
        {help && (
          <Tooltip delayDuration={120}>
            <TooltipTrigger asChild>
              <button className="setting-help-trigger" type="button" aria-label={`查看“${title}”详细说明`}>
                <CircleHelp size={16} aria-hidden="true" />
              </button>
            </TooltipTrigger>
            <TooltipContent className="setting-help-content" side="right" sideOffset={8}>
              <div className="setting-help-body">
                <strong>{help.heading}</strong>
                {(help.paragraphs || []).map((paragraph) => <p key={paragraph}>{paragraph}</p>)}
                {(help.items || []).length > 0 && (
                  <ul>
                    {help.items.map((item) => <li key={item}>{item}</li>)}
                  </ul>
                )}
                {help.note && <p className="setting-help-note">{help.note}</p>}
              </div>
            </TooltipContent>
          </Tooltip>
        )}
      </div>
      {detail && <p>{detail}</p>}
    </div>
  );
}
