/** 通用设置的数据适配器独立读取系统接口，不获取节点、隧道 token 或其他业务配置。 */
import { useCallback, useEffect, useState } from "react";
import { requestJSON } from "@/lib/api";

/**
 * useSystemSettings 读取公共开关，写入后可触发刷新。
 * 参数：无；返回 {data: object|null, error: string, reload: Function}。
 * 错误：单次读取限制 8 秒，失败保留原值但页面禁用开关；卸载取消请求，旧轮次不会覆盖新值。
 */
export function useSystemSettings() {
  const [data, setData] = useState(null);
  const [error, setError] = useState("");
  const [revision, setRevision] = useState(0);
  /** reload 触发立即刷新；参数无，返回无；上一轮查询由 effect 清理，无同步错误。 */
  const reload = useCallback(() => setRevision((value) => value + 1), []);
  useEffect(() => {
    let stopped = false;
    let timer;
    let controller;
    /** poll 读取一次系统设置；参数无，返回 Promise<void>；异常转为可见提示，完成后再安排轮询避免重叠。 */
    async function poll() {
      controller = new AbortController();
      const deadline = window.setTimeout(() => controller.abort(), 8000);
      try {
        const next = await requestJSON("/api/system/settings", { signal: controller.signal });
        if (!stopped) { setData(next); setError(""); }
      } catch {
        if (!stopped) setError("系统设置暂时无法读取，请重试。");
      } finally {
        window.clearTimeout(deadline);
        if (!stopped) timer = window.setTimeout(poll, 60000);
      }
    }
    poll();
    return () => { stopped = true; controller?.abort(); window.clearTimeout(timer); };
  }, [revision]);
  return { data, error, reload };
}
