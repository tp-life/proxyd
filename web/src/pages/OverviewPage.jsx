/** 全局总览只呈现功能摘要与处理入口，详细操作仍由各业务页面负责。 */
import { Activity, ArrowRight, CheckCircle2, CircleAlert, Laptop, Layers, Network, RefreshCw, Search, Sun, Moon } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { PageHeader } from "@/components/PageHeader";
import { buildDashboard } from "@/lib/dashboard";
import { formatBytes } from "@/lib/format";

/**
 * formatUptime 将服务端时长格式化，浏览器刷新不重置计时。
 * 参数：seconds 为 number | undefined；返回 string；缺失数据返回未知，不推算启动时间。
 */
function formatUptime(seconds) {
  if (!Number.isFinite(seconds)) return "—";
  const minutes = Math.floor(seconds / 60);
  if (minutes < 1) return "不足 1 分钟";
  if (minutes < 60) return `${minutes} 分钟`;
  const hours = Math.floor(minutes / 60);
  return hours < 24 ? `${hours} 小时 ${minutes % 60} 分钟` : `${Math.floor(hours / 24)} 天 ${hours % 24} 小时`;
}

/**
 * OverviewPage 呈现系统状态、已启用功能摘要和待处理事项，不额外创建终端会话。
 * 参数：modules 为模块数组，overview/traffic/feed 为快照，snapshotError 为 string；
 * theme 为 string，onNavigate/onRefresh/onPalette/onToggleTheme 为 Function 回调。
 * 返回 JSX；来源失败显示未知与局部错误，操作错误由父组件处理。
 */
export function OverviewPage({ modules, overview, traffic, feed, snapshotError, theme, onNavigate, onRefresh, onPalette, onToggleTheme }) {
  const { data, errors, loading } = feed;
  const { cards, items } = buildDashboard(modules, { overview, data });
  const sourceLabels = { system: "系统状态", connections: "代理连接", remote: "远程服务", devices: "设备档案", desktop: "桌面会话" };
  const failures = Object.entries(errors).filter(([key]) => key === "system" || cards.some((card) => card.id === (key === "connections" ? "proxy" : "remote")));
  const incomplete = Boolean(snapshotError || failures.length || !data.system || !modules.length);
  const attention = items.length > 0;
  return <div className="dashboard-shell space-y-6">
    <PageHeader eyebrow="服务控制台" title="总览" detail="查看各项功能的运行情况，及时处理需要关注的问题。">
      <Button variant="outline" size="icon" aria-label="打开命令菜单" onClick={onPalette}><Search size={16} /></Button>
      <Button variant="outline" size="icon" aria-label={theme === "light" ? "切换到深色模式" : "切换到明亮模式"} onClick={onToggleTheme}>{theme === "light" ? <Moon size={16} /> : <Sun size={16} />}</Button>
      <Button variant="outline" disabled={loading} onClick={onRefresh}><RefreshCw size={16} />刷新状态</Button>
    </PageHeader>
    <section className="panel dashboard-health" aria-label="整体状态">
      <div className="flex items-center gap-3">{incomplete || attention ? <CircleAlert className="text-amber-500 shrink-0" size={28} /> : <CheckCircle2 className="text-emerald-500 shrink-0" size={28} />}<div><h2 className="font-semibold">{incomplete ? "部分状态尚未确认" : attention ? "有待处理事项" : cards.length ? "各项功能暂无异常" : "管理服务运行中"}</h2><p className="text-sm text-muted-foreground">{data.system ? "守护进程运行中" : "正在确认守护进程状态"} · 运行时长 {formatUptime(data.system?.uptime_seconds)}</p></div></div>
      <div className="flex flex-wrap gap-2"><Badge variant="outline">{modules.length ? `${cards.length} / ${modules.length} 个模块已启用` : "正在读取模块"}</Badge><Badge variant={attention ? "destructive" : "outline"}>{items.length} 项待处理</Badge></div>
    </section>
    {(snapshotError || failures.length > 0) && <div role="alert" className="panel p-4 space-y-1 text-sm text-amber-600 dark:text-amber-400">{snapshotError && <p>{snapshotError}；已显示的数据可能尚未更新。</p>}{failures.map(([key, error]) => <p key={key}>{sourceLabels[key]}：{error}</p>)}</div>}
    <section aria-label="功能摘要" className="grid gap-5 lg:grid-cols-2">
      {cards.map((card) => <article className="panel p-5 space-y-5 min-w-0" key={card.id} aria-label={`${card.name}摘要`}>
        <div className="flex items-start justify-between gap-3"><div className="flex gap-3 items-center">{card.id === "proxy" ? <Network size={23} className="text-primary" /> : card.id === "remote" ? <Laptop size={23} className="text-primary" /> : <Layers size={23} />}<div><h2 className="font-semibold">{card.name}</h2><p className="text-sm text-muted-foreground">{card.description}</p></div></div><Badge variant={card.attention ? "destructive" : "outline"}>{card.label}</Badge></div>
        <dl className="grid grid-cols-2 gap-x-4 gap-y-5">{card.metrics.map((metric) => <div key={metric.label}><dt className="text-xs text-muted-foreground">{metric.label}</dt><dd className="mt-1 text-lg font-semibold">{metric.value ?? "—"}</dd></div>)}{card.id === "proxy" && <div><dt className="text-xs text-muted-foreground">实时上传 / 下载</dt><dd className="mt-1 text-sm font-semibold">{traffic.connected ? `${formatBytes(traffic.up)}/s / ${formatBytes(traffic.down)}/s` : "暂不可用"}</dd></div>}</dl>
        {card.id === "remote" && <p className="text-xs text-muted-foreground">设备数量为保存的档案数；连接状态以设备页面的实际探测结果为准。已最小化的 Web Terminal 会话可在右下角恢复。</p>}
        <Button variant="outline" onClick={() => onNavigate(card.view)}>进入{card.id === "proxy" ? "运行概览" : card.name}<ArrowRight size={15} /></Button>
      </article>)}
    </section>
    {modules.length > 0 && !cards.length && <section className="panel p-8 text-center space-y-4"><Layers className="mx-auto text-muted-foreground" size={32} /><h2 className="font-semibold">尚未启用功能模块</h2><p className="text-sm text-muted-foreground">按需启用代理或远程访问，已有配置会保留。</p><Button onClick={() => onNavigate("modules")}>前往模块管理</Button></section>}
    <section className="panel p-5 space-y-4" aria-labelledby="dashboard-attention-title">
      <div className="flex items-center gap-2"><Activity size={18} /><h2 id="dashboard-attention-title" className="font-semibold">待处理事项</h2></div>
      {!items.length ? <p className="text-sm text-muted-foreground">{incomplete ? "状态尚未完整，暂不能确认是否存在异常。" : "暂无待处理事项。"}</p> : <ul className="divide-y">{items.map((item) => <li key={item.id} className="dashboard-attention-item py-4"><div className="min-w-0"><h3 className="font-medium">{item.title}</h3><p className="text-sm text-muted-foreground break-words">{item.detail}</p>{item.retryAt && <p className="text-xs text-muted-foreground mt-1">下次重试：{new Date(item.retryAt).toLocaleString("zh-CN")}</p>}</div><Button variant="outline" size="sm" onClick={() => onNavigate(item.view)}>前往处理<ArrowRight size={14} /></Button></li>)}</ul>}
    </section>
    <footer className="flex flex-wrap items-center justify-between gap-3 text-sm text-muted-foreground"><span>{modules.length ? `已启用 ${cards.length} / ${modules.length} 个模块` : "正在读取模块状态"}</span><Button variant="outline" onClick={() => onNavigate("modules")}>管理模块<ArrowRight size={15} /></Button></footer>
  </div>;
}
