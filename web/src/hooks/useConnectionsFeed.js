import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { requestJSON } from "@/lib/api";
import { normalizeConnectionsResponse } from "@/lib/connections";

/**
 * useConnectionsFeed 管理活动连接页的加载、筛选和关闭操作。
 *
 * 功能说明：
 * 这个 hook 只在 `activeView === "connections"` 时工作，并且只请求 `/api/connections`
 * 一个接口。页面进入时加载一次，之后完全由手动刷新按钮和关闭动作触发重新拉取，
 * 不做自动轮询，避免长连接页持续占用请求与重绘。
 *
 * 参数说明：
 * - activeView: string，当前页面 id。
 * - requestConfirmation: Function，复用全局确认对话框的请求函数。
 * - showToast: Function，统一 toast 提示函数。
 *
 * 返回值说明：
 * 返回连接列表、摘要、筛选状态与关闭动作。
 *
 * 可能的异常/错误情况：
 * - 网络失败或后端返回错误时会保留上一份数据，并把错误文本写入 `error`。
 * - 单条或全部关闭失败时只影响当前动作，不会破坏列表。
 */
export function useConnectionsFeed(activeView, requestConfirmation, showToast) {
  const [rows, setRows] = useState([]);
  const [summary, setSummary] = useState({
    activeCount: 0,
    uploadBytes: 0,
    downloadBytes: 0,
    memory: null,
  });
  const [loading, setLoading] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState("");
  const [hasLoaded, setHasLoaded] = useState(false);
  const [updatedAt, setUpdatedAt] = useState(null);
  const [query, setQuery] = useState("");
  const [transport, setTransport] = useState("all");
  const [pendingIds, setPendingIds] = useState(() => new Set());
  const [closingAll, setClosingAll] = useState(false);
  const requestControllerRef = useRef(null);
  const requestTokenRef = useRef(0);
  const hasLoadedRef = useRef(false);

  /**
   * loadConnections 拉取并归一化活动连接列表。
   *
   * 功能说明：
   * 该接口是这个页面唯一的实时数据源。每次拉取前会取消上一轮尚未结束的请求，
   * 这样在自动轮询和手动重试同时发生时，始终以最后一次请求为准，避免旧响应把
   * 新状态覆盖回去。
   *
   * 参数说明：
   * 无。
   *
   * 返回值说明：
   * 返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * - 请求被新的轮询或页面卸载中断时，使用 AbortError 静默退出。
   * - 上游 4xx/5xx 或网络失败时保留上一份列表，并把错误文本展示到错误条带。
   */
  const loadConnections = useCallback(async () => {
    const requestToken = requestTokenRef.current + 1;
    requestTokenRef.current = requestToken;

    if (requestControllerRef.current) {
      requestControllerRef.current.abort();
    }

    const controller = new AbortController();
    requestControllerRef.current = controller;
    const initialLoad = !hasLoadedRef.current;

    if (initialLoad) {
      setLoading(true);
    } else {
      setRefreshing(true);
    }

    try {
      const payload = await requestJSON("/api/connections", { signal: controller.signal });
      if (requestTokenRef.current !== requestToken) {
        return;
      }
      const normalized = normalizeConnectionsResponse(payload);
      setRows(normalized.items);
      setSummary(normalized.summary);
      setError("");
      setUpdatedAt(new Date());
      hasLoadedRef.current = true;
      setHasLoaded(true);
      setPendingIds((current) => {
        const next = new Set();
        for (const pendingId of current) {
          if (normalized.items.some((item) => item.closeId === pendingId)) {
            next.add(pendingId);
          }
        }
        return next;
      });
    } catch (error) {
      if (error?.name === "AbortError") {
        return;
      }
      if (requestTokenRef.current !== requestToken) {
        return;
      }
      hasLoadedRef.current = true;
      setHasLoaded(true);
      setError(error.message || "连接列表加载失败");
    } finally {
      if (requestTokenRef.current === requestToken) {
        setLoading(false);
        setRefreshing(false);
      }
    }
  }, []);

  useEffect(() => {
    if (activeView !== "connections") {
      requestControllerRef.current?.abort();
      return undefined;
    }

    // 该页只做手动刷新：进入页面时加载一次，之后由刷新按钮或关闭动作触发，
    // 不再自动轮询，避免长连接页持续占用请求与重绘。
    loadConnections();
    return () => {
      requestControllerRef.current?.abort();
    };
  }, [activeView, loadConnections]);

  /**
   * closeConnection 关闭单条活动连接。
   *
   * 功能说明：
   * 删除动作会直接调用后端的 `/api/connections/{id}`，成功后立即 toast 并重新拉取
   * 列表，保证摘要数字和表格都尽快回到最新状态。
   *
   * 参数说明：
   * - connection: object，已经标准化的连接模型。
   *
   * 返回值说明：
   * 返回 Promise<boolean>；成功为 `true`，失败为 `false`。
   *
   * 可能的异常/错误情况：
   * - 连接缺少可删除 id 时直接返回 false。
   * - 网络失败或上游拒绝时会 toast 错误，并在结束后清除 pending 状态。
   */
  const closeConnection = useCallback(
    async (connection) => {
      if (!connection?.closeId) {
        return false;
      }

      setPendingIds((current) => {
        const next = new Set(current);
        next.add(connection.closeId);
        return next;
      });

      try {
        await requestJSON(`/api/connections/${encodeURIComponent(connection.closeId)}`, {
          method: "DELETE",
        });
        showToast(`已关闭连接：${connection.targetLabel}`);
        await loadConnections();
        return true;
      } catch (error) {
        showToast(`关闭连接失败：${error.message}`, "err");
        return false;
      } finally {
        setPendingIds((current) => {
          const next = new Set(current);
          next.delete(connection.closeId);
          return next;
        });
      }
    },
    [loadConnections, showToast],
  );

  /**
   * closeAllConnections 关闭当前全部活动连接。
   *
   * 功能说明：
   * 这是一个二次确认的危险操作，必须经过全局 ConfirmDialog。确认后会调用
   * `/api/connections` 的 DELETE 接口，并在成功后立即刷新列表。
   *
   * 参数说明：
   * 无。
   *
   * 返回值说明：
   * 返回 Promise<boolean>；确认并成功关闭时为 `true`。
   *
   * 可能的异常/错误情况：
   * - 用户取消确认时返回 false，不会发请求。
   * - 后端拒绝或网络失败时会 toast 错误，不会清空列表状态。
   */
  const closeAllConnections = useCallback(async () => {
    if (closingAll || rows.length === 0) {
      return false;
    }

    const accepted = await requestConfirmation({
      title: "关闭全部活动连接？",
      description: `当前有 ${rows.length} 条活动连接。关闭后客户端会重新建立连接，页面会立即刷新为最新状态。`,
      confirmLabel: "关闭全部",
      destructive: true,
    });
    if (!accepted) {
      return false;
    }

    setClosingAll(true);
    try {
      await requestJSON("/api/connections", { method: "DELETE" });
      showToast("已关闭全部活动连接");
      await loadConnections();
      return true;
    } catch (error) {
      showToast(`关闭全部失败：${error.message}`, "err");
      return false;
    } finally {
      setClosingAll(false);
    }
  }, [closingAll, loadConnections, requestConfirmation, rows.length, showToast]);

  /**
   * retryConnections 重新拉取活动连接列表。
   *
   * 功能说明：
   * 给错误条带与手动刷新按钮复用的轻量封装，避免页面层直接依赖内部加载实现。
   *
   * 参数说明：
   * 无。
   *
   * 返回值说明：
   * 返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 无；错误已经由连接页状态条带承接。
   */
  const retryConnections = useCallback(() => loadConnections(), [loadConnections]);

  const visibleRows = useMemo(() => {
    const normalizedQuery = query.trim().toLowerCase();
    return rows.filter((row) => {
      if (transport !== "all" && row.protocolKey !== transport) {
        return false;
      }
      if (!normalizedQuery) {
        return true;
      }
      return row.searchText.includes(normalizedQuery);
    });
  }, [query, rows, transport]);

  return {
    activeCount: summary.activeCount,
    closingAll,
    error,
    hasLoaded,
    loading,
    pendingIds,
    query,
    refreshing,
    retryConnections,
    rows,
    summary,
    transport,
    updatedAt,
    visibleRows,
    setQuery,
    setTransport,
    closeAllConnections,
    closeConnection,
  };
}
