/** 模块管理页集中展示可启停能力，子功能配置保留在各自业务上下文中。 */
import { Button } from "@/components/ui/button";
import { PageHeader } from "@/components/PageHeader";

/** MODULE_DESCRIPTIONS 按模块 id 给出影响范围说明；新增模块在此登记，避免三元表达式堆叠。 */
const MODULE_DESCRIPTIONS = {
  proxy: "控制代理入口、TUN、DNS、系统代理及周期订阅刷新。禁用会断开活动代理连接。",
  remote: "控制隧道服务、端口转发、远程桌面连接与 Web Terminal。禁用会结束现有会话。",
  gateway: "控制 LAN 网关的转发规则与 IPv4 转发开关。禁用会清除规则，下游设备恢复直连；启用要求代理模块已启用。",
};

/**
 * ModulesPage 展示模块开关、运行阶段及其影响范围。
 * 参数：modules 为状态数组；busy 为正在写入的标识；onToggle/onRetry 为异步启停和重试回调。
 * 返回：React 元素；网络与事务错误由父组件统一提示，不在本页伪造成功状态。
 */
export function ModulesPage({ modules, busy, onToggle, onRetry }) {
  return <div className="space-y-5">
    <PageHeader title="模块管理" detail="按需启用能力。禁用保留配置，管理控制台持续可用。" />
    <div className="grid gap-4 md:grid-cols-2">
      {modules.map((module) => <section className="panel p-5 space-y-4" key={module.id}>
        <div className="flex items-center justify-between gap-4"><h2 className="font-semibold">{module.name}</h2><span>{module.enabled ? "已启用" : "已禁用"}</span></div>
        <p className="text-sm text-muted-foreground">{MODULE_DESCRIPTIONS[module.id] || "控制该模块的运行时资源。禁用保留配置。"}</p>
        <div className="rounded-md border p-3 space-y-2 text-sm" role="status"><p>运行状态：{{ idle: "待命", starting: "正在启动", running: "运行中", retrying: "等待重试", degraded: "部分功能异常", failed: "启动失败", disabled: "已禁用" }[module.phase] || "待命"}</p>{module.error && <p className="text-destructive">{module.error}</p>}{module.next_retry_at && <p>下次重试：{new Date(module.next_retry_at).toLocaleTimeString()}</p>}</div>
        <div className="flex flex-wrap gap-2"><Button variant={module.enabled ? "outline" : "default"} disabled={Boolean(busy)} onClick={() => onToggle(module)}>{busy === module.id ? "正在应用…" : module.enabled ? "禁用模块" : "启用模块"}</Button>{module.enabled && <Button variant="outline" disabled={Boolean(busy) || module.phase === "starting"} onClick={() => onRetry(module)}>立即重试</Button>}</div>
      </section>)}
    </div>
    <p className="text-sm text-muted-foreground">模块启用后，各子功能继续遵循原来的开关。已结束的连接需要重新建立；独立运行的客户端命令不受此守护进程开关控制。</p>
  </div>;
}
