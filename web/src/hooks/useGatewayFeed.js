/** 网关页数据适配层：状态、预检、设备表与写操作；只在页面激活时加载，不做后台轮询。 */
import { useCallback, useEffect, useRef, useState } from "react";
import { requestJSON } from "@/lib/api";

/**
 * useGatewayFeed 接入 LAN 网关页的加载与管理能力。
 *
 * 功能说明：
 * 页面激活时拉取 `/api/gateway`（模块相位、执行层状态、设备表）、`/api/gateway/precheck`
 * （启用前特权检查）与 `/api/groups`（设备策略的分组选项）。写操作（启停、设备增删改）
 * 成功后重新拉取，保持与后端事务结果一致；加载失败保留旧数据并展示错误条带。
 *
 * 参数说明：
 * - active: boolean，当前是否处于网关页。
 * - requestConfirmation: Function，全局确认对话框请求函数（删除设备用）。
 * - showToast: Function，全局 toast 展示函数。
 *
 * 返回值说明：
 * 返回网关页所需的状态与操作方法对象。
 *
 * 可能的异常/错误情况：
 * 接口失败时保留上一份数据并把错误文本交给页面错误条带；写操作失败 toast 后端
 * 返回的纯文本错误。
 */
export function useGatewayFeed(active, requestConfirmation, showToast) {
  const [overview, setOverview] = useState(null);
  const [precheck, setPrecheck] = useState(null);
  const [groups, setGroups] = useState([]);
  const [loading, setLoading] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState("");
  const [hasLoaded, setHasLoaded] = useState(false);
  const requestControllerRef = useRef(null);
  const requestTokenRef = useRef(0);
  const hasLoadedRef = useRef(false);

  /**
   * reload 拉取网关状态、预检结果与分组选项。
   *
   * 参数说明：无。
   *
   * 返回值说明：返回 Promise<void>。
   *
   * 可能的异常/错误情况：请求被新加载或页面切换中断时静默退出；上游 4xx/5xx 或网络
   * 失败时保留旧数据并展示错误条带。预检失败不阻断状态展示（其本身只是辅助信息）。
   */
  const reload = useCallback(async () => {
    const requestToken = requestTokenRef.current + 1;
    requestTokenRef.current = requestToken;
    requestControllerRef.current?.abort();
    const controller = new AbortController();
    requestControllerRef.current = controller;
    if (hasLoadedRef.current) setRefreshing(true);
    else setLoading(true);
    try {
      const [overviewResult, precheckResult, groupsResult] = await Promise.allSettled([
        requestJSON("/api/gateway", { signal: controller.signal }),
        requestJSON("/api/gateway/precheck", { signal: controller.signal }),
        requestJSON("/api/groups", { signal: controller.signal }),
      ]);
      if (requestTokenRef.current !== requestToken) return;
      if (overviewResult.status === "fulfilled") {
        setOverview(overviewResult.value);
        setError("");
      } else if (overviewResult.reason?.name !== "AbortError") {
        setError(overviewResult.reason?.message || "网关状态加载失败");
      }
      if (precheckResult.status === "fulfilled") setPrecheck(precheckResult.value);
      if (groupsResult.status === "fulfilled") setGroups(groupsResult.value || []);
      hasLoadedRef.current = true;
      setHasLoaded(true);
    } finally {
      if (requestTokenRef.current === requestToken) {
        setLoading(false);
        setRefreshing(false);
      }
    }
  }, []);

  useEffect(() => {
    if (!active) {
      requestControllerRef.current?.abort();
      return undefined;
    }
    reload();
    return () => requestControllerRef.current?.abort();
  }, [active, reload]);

  /**
   * toggleEnabled 启停网关模块（POST /api/gateway，响应与 GET 同构）。
   *
   * 参数说明：enabled 为 boolean 目标状态。
   * 返回值说明：返回 Promise<void>。
   * 可能的异常/错误情况：平台不支持、代理模块停用中启用或事务失败时 toast 错误，
   * 开关保持原状态；macOS helper 未安装不算失败（degraded 体现在相位中）。
   */
  const toggleEnabled = useCallback(async (enabled) => {
    try {
      const payload = await requestJSON("/api/gateway", { method: "POST", body: JSON.stringify({ enabled }) });
      if (payload) setOverview(payload);
      showToast(enabled ? "网关已启用（登记设备并指向本机后开始分流）" : "网关已停用，转发规则已清除");
    } catch (toggleError) {
      showToast(`操作失败：${toggleError.message}`, "err");
    }
  }, [showToast]);

  /**
   * addDevice 登记一台下游设备。
   *
   * 参数说明：form 为 {name, ip, mac, policy} 表单原始值。
   * 返回值说明：返回 Promise<boolean>，事务成功为 true。
   * 可能的异常/错误情况：重名/IP 重复/字段非法或调和失败时 toast 后端错误并返回 false。
   */
  const addDevice = useCallback(async (form) => {
    try {
      await requestJSON("/api/gateway/devices", { method: "POST", body: JSON.stringify(form) });
      showToast(`设备 ${form.name} 已登记`);
      await reload();
      return true;
    } catch (addError) {
      showToast(`添加失败：${addError.message}`, "err");
      return false;
    }
  }, [reload, showToast]);

  /**
   * updateDevice 原位更新一台设备（名称锁定，与后端一致不改名）。
   *
   * 参数说明：name 为现有设备名；device 为目标完整设备对象。
   * 返回值说明：返回 Promise<boolean>，事务成功为 true。
   * 可能的异常/错误情况：设备不存在或校验失败时 toast 后端错误并返回 false。
   */
  const updateDevice = useCallback(async (name, device) => {
    try {
      await requestJSON(`/api/gateway/devices/${encodeURIComponent(name)}`, { method: "PUT", body: JSON.stringify(device) });
      showToast(`设备 ${name} 已更新`);
      await reload();
      return true;
    } catch (updateError) {
      showToast(`更新失败：${updateError.message}`, "err");
      return false;
    }
  }, [reload, showToast]);

  /**
   * removeDevice 经确认后删除一台设备；其分流规则随下次生成消失。
   *
   * 参数说明：name 为设备名。
   * 返回值说明：返回 Promise<boolean>；用户取消返回 false。
   * 可能的异常/错误情况：删除失败时 toast 错误并保留现有设备表。
   */
  const removeDevice = useCallback(async (name) => {
    const accepted = await requestConfirmation({
      title: `删除网关设备 ${name}？`,
      description: "删除后该设备的流量不再经过本机分流，请把它的网关/DNS 改回路由器或原有配置。",
      confirmLabel: "确认删除",
      destructive: true,
    });
    if (!accepted) return false;
    try {
      await requestJSON(`/api/gateway/devices/${encodeURIComponent(name)}`, { method: "DELETE" });
      showToast(`设备 ${name} 已删除`);
      await reload();
      return true;
    } catch (removeError) {
      showToast(`删除失败：${removeError.message}`, "err");
      return false;
    }
  }, [reload, requestConfirmation, showToast]);

  return {
    overview,
    precheck,
    groups,
    loading,
    refreshing,
    error,
    hasLoaded,
    reload,
    toggleEnabled,
    addDevice,
    updateDevice,
    removeDevice,
  };
}
