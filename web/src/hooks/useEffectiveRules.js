import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { requestJSON } from "@/lib/api";

/** EFFECTIVE_RULES_PAGE_SIZE 是「当前生效规则」每次追加的条数。 */
const EFFECTIVE_RULES_PAGE_SIZE = 200;

/**
 * useEffectiveRules 管理规则页「当前生效规则」面板的加载、筛选与分页。
 *
 * 功能说明：
 * - 只在 `activeView === "rules"` 时工作，进入页面加载第一页，避免其它页面白打请求。
 * - 数据源是只读接口 `/api/rules/effective`；订阅自带规则可能上万条，接口按 offset/limit
 *   分页，页面通过 `loadMore` 追加，绝不一次性把全量规则拉进浏览器。
 * - 关键字筛选会抖动，因此做了 250ms 防抖；来源筛选是离散选择，立即生效。
 *
 * 参数说明：
 * - activeView: string，当前页面 id。
 *
 * 返回值说明：
 * 返回面板所需的只读状态（available/total/matched/sources/rows/loading/error）、筛选值
 * 与 setQuery/setSource/reload/loadMore/hasMore 操作。
 *
 * 可能的异常/错误情况：
 * - 网络失败或后端返回 400 时保留已有行，把错误文本写入 `error` 由面板展示。
 * - 请求被下一次筛选或页面卸载中断时按 AbortError 静默退出。
 */
export function useEffectiveRules(activeView) {
  const [rows, setRows] = useState([]);
  const [summary, setSummary] = useState({
    available: false,
    total: 0,
    matched: 0,
    sources: [],
  });
  const [loading, setLoading] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [query, setQuery] = useState("");
  const [source, setSource] = useState("");
  const controllerRef = useRef(null);
  const tokenRef = useRef(0);

  /**
   * buildEffectiveRulesURL 拼装一次分页查询地址。
   *
   * 参数说明：offset 为筛选结果里的起始下标。
   * 返回值说明：返回 string 形式的相对 API 路径。
   * 可能的异常/错误情况：无；参数经 URLSearchParams 转义。
   */
  const buildURL = useCallback(
    (offset) => {
      const params = new URLSearchParams({
        offset: String(offset),
        limit: String(EFFECTIVE_RULES_PAGE_SIZE),
      });
      if (source) params.set("source", source);
      if (query.trim()) params.set("q", query.trim());
      return `/api/rules/effective?${params.toString()}`;
    },
    [query, source],
  );

  /**
   * fetchPage 拉取一页生效规则。
   *
   * 参数说明：
   * - offset: number，起始下标；0 表示重新加载第一页。
   * - append: boolean，true 时把结果追加到现有行，false 时替换。
   *
   * 返回值说明：返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 用单调递增的 token 丢弃过期响应，避免快速输入关键字时旧响应覆盖新结果；请求失败
   * 时保留上一份数据并展示错误文本。
   */
  const fetchPage = useCallback(
    async (offset, append) => {
      const token = tokenRef.current + 1;
      tokenRef.current = token;
      controllerRef.current?.abort();
      const controller = new AbortController();
      controllerRef.current = controller;
      setLoading(true);
      try {
        const payload = await requestJSON(buildURL(offset), {
          signal: controller.signal,
        });
        if (tokenRef.current !== token) return;
        setSummary({
          available: Boolean(payload?.available),
          total: payload?.total || 0,
          matched: payload?.matched || 0,
          sources: payload?.sources || [],
        });
        setRows((current) =>
          append ? [...current, ...(payload?.rules || [])] : payload?.rules || [],
        );
        setError("");
        setLoaded(true);
      } catch (fetchError) {
        if (fetchError?.name === "AbortError") return;
        if (tokenRef.current !== token) return;
        setError(fetchError.message || "生效规则加载失败");
        setLoaded(true);
      } finally {
        if (tokenRef.current === token) setLoading(false);
      }
    },
    [buildURL],
  );

  useEffect(() => {
    if (activeView !== "rules") {
      controllerRef.current?.abort();
      return undefined;
    }
    // 关键字防抖；来源切换与首次进入立即加载。
    const delay = query.trim() ? 250 : 0;
    const timer = setTimeout(() => {
      fetchPage(0, false);
    }, delay);
    return () => clearTimeout(timer);
  }, [activeView, fetchPage, query]);

  /** reload 重新拉取第一页；参数无；返回 Promise<void>；失败由内部错误状态承接。 */
  const reload = useCallback(() => fetchPage(0, false), [fetchPage]);

  /** loadMore 追加下一页；参数无；返回 Promise<void>；失败由内部错误状态承接。 */
  const loadMore = useCallback(
    () => fetchPage(rows.length, true),
    [fetchPage, rows.length],
  );

  const hasMore = useMemo(
    () => summary.available && rows.length < summary.matched,
    [rows.length, summary.available, summary.matched],
  );

  return {
    available: summary.available,
    error,
    hasMore,
    loadMore,
    loaded,
    loading,
    matched: summary.matched,
    query,
    reload,
    rows,
    setQuery,
    setSource,
    source,
    sources: summary.sources,
    total: summary.total,
  };
}
