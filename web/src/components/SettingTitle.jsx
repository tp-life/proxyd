/** 设置卡片共享的帮助展示组件，不持有任何业务状态。 */
import { CircleHelp } from "lucide-react";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";

/**
 * SettingTitle 渲染带详细帮助入口的设置标题。
 *
 * 功能说明：
 * 保留设置卡片原有的标题与摘要层级，并使用 Radix Tooltip 提供更完整的用途、
 * 优先级和风险边界说明。触发按钮同时支持 hover、键盘聚焦和触屏点击。
 *
 * 参数说明：
 * - title: string，设置卡片标题。
 * - detail: string，卡片内常驻显示的简短摘要。
 * - help: object，详细帮助模型，包含 heading、paragraphs、items 和 note。
 *
 * 返回值说明：
 * 返回包含标题、帮助触发按钮、摘要和 TooltipContent 的 React 元素。
 *
 * 可能的异常/错误情况：
 * help 缺失时仅渲染普通标题与摘要；帮助段落或列表为空时自动省略对应区块，
 * 不影响设置控件的提交和状态更新。
 */
export function SettingTitle({ title, detail, help }) {
  return (
    <div className="panel-heading setting-heading">
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
