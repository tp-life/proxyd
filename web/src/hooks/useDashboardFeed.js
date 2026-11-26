/** 总览数据适配层只读现有管理接口，不发起远端探测或加载审计、私钥与配置正文。 */
import { useCallback, useEffect, useState } from "react";
import { requestJSON } from "@/lib/api";

/**
 * useDashboardFeed 在总览可见时独立轮询系统与已启用模块的摘要来源。
 * 参数：active 为 boolean，modules 为 Array<object>；返回数据、各源错误、刷新状态及 reload 回调。
 * 错误：单个来源失败不阻断其他来源；8 秒超时清除该来源旧值，防止把过期数据当作当前状态。
 */
export function useDashboardFeed(active, modules) {
  const [snapshot, setSnapshot] = useState({ data: {}, errors: {}, loading: true });
  const [revision, setRevision] = useState(0);
  const proxyEnabled = modules.some((module) => module.id === "proxy" && module.enabled);
  const remoteEnabled = modules.some((module) => module.id === "remote" && module.enabled);
  /** reload 触发新一轮查询；参数无，返回无；取消旧轮次由 effect 清理负责，无同步错误。 */
  const reload = useCallback(() => setRevision((current) => current + 1), []);
  useEffect(() => {
    if (!active) return;
    // 再次进入或重新启用模块时先清空旧快照，避免短暂展示上一轮服务或会话计数。
    setSnapshot({ data: {}, errors: {}, loading: true });
    let stopped = false;
    let timer;
    let controller;
    const sources = { system: "/api/system/status" };
    if (proxyEnabled) sources.connections = "/api/connections";
    if (remoteEnabled) {
      sources.remote = "/api/remote";
      sources.devices = "/api/remote/remotes";
      sources.desktop = "/api/desktop";
    }
    /** poll 并发读取独立来源；参数无，返回 Promise<void>；所有拒绝均转为局部错误，无未处理异常。 */
    async function poll() {
      controller = new AbortController();
      const deadline = window.setTimeout(() => controller.abort(), 8000);
      setSnapshot((current) => ({ ...current, loading: true }));
      const results = await Promise.allSettled(Object.values(sources).map((url) => requestJSON(url, { signal: controller.signal })));
      window.clearTimeout(deadline);
      if (stopped) return;
      const data = {};
      const errors = {};
      Object.keys(sources).forEach((key, index) => {
        const result = results[index];
        if (result.status === "fulfilled") data[key] = result.value;
        else errors[key] = "暂时无法读取，稍后自动重试";
      });
      setSnapshot({ data, errors, loading: false });
      // 请求完成后再排下一轮，慢接口不会造成重叠请求；切页或切换模块会取消整轮请求。
      timer = window.setTimeout(poll, 60000);
    }
    poll();
    return () => { stopped = true; window.clearTimeout(timer); controller?.abort(); };
  }, [active, proxyEnabled, remoteEnabled, revision]);
  return { ...snapshot, reload };
}
