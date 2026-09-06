/** 导航状态模块使用 hash 历史，兼容静态资源部署及浏览器前进/后退。 */
import { useCallback, useEffect, useState } from "react";
import { normalizeView, resolveModuleView } from "@/lib/navigation";

/**
 * useNavigation 在 URL 与页面状态间建立单一来源。
 * 参数说明：modules 为 Array<object>，后端模块快照。
 * 返回值说明：[string, Function]，当前页面和跳转函数。
 * 错误情况：未知 hash 规范化到总览，旧链接及禁用模块地址使用 replace 修正，避免后退时反复落到隐藏页面。
 */
export function useNavigation(modules) {
  const [view, setView] = useState(() => normalizeView(window.location.hash));
  useEffect(() => {
    /**
     * synchronize 响应地址变化并修正未知地址。
     * 参数说明：无；返回值：无。
     * 错误情况：只使用同源 hash，避免对服务端路由提出要求。
     */
    function synchronize() {
      const next = resolveModuleView(window.location.hash, modules);
      const hash = `#/${next}`;
      if (window.location.hash !== hash) window.history.replaceState(null, "", hash);
      setView(next);
    }
    synchronize();
    window.addEventListener("hashchange", synchronize);
    return () => window.removeEventListener("hashchange", synchronize);
  }, [modules]);
  /**
   * navigate 通过地址变更创建可回退的页面历史。
   * 参数说明：next 为 string，允许旧 remote 别名；返回值：无。
   * 错误情况：未知 id 规范化，重复点击不添加历史条目。
   */
  const navigate = useCallback((next) => {
    const normalized = resolveModuleView(next, modules);
    window.location.hash = `/${normalized}`;
    setView(normalized);
  }, [modules]);
  return [resolveModuleView(view, modules), navigate];
}
