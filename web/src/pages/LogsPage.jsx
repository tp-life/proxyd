import { useCallback, useEffect, useMemo, useState } from "react";
import { Copy, Download, Pause, Play, RefreshCw, Search } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { EmptyState } from "@/components/EmptyState";
import { Field } from "@/components/Field";
import { PageHeader } from "@/components/PageHeader";
import { PanelTitle } from "@/components/PanelTitle";
import { requestJSON } from "@/lib/api";
import { classNames } from "@/lib/format";

/**
 * LogsPage 渲染运行日志面板。
 *
 * 参数说明：
 * 无；组件内部读取 `/api/logs`，用户可调整 tail 与 level。
 *
 * 返回值说明：
 * 返回日志筛选工具栏与日志行列表。
 *
 * 可能的异常/错误情况：
 * API 不可达时在面板内显示错误，避免日志页空白。
 */
export function LogsPage() {
  const [tail, setTail] = useState("200");
  const [level, setLevel] = useState("");
  const [logs, setLogs] = useState([]);
  const [error, setError] = useState("");
  const [query, setQuery] = useState("");
  const [paused, setPaused] = useState(false);

  /**
   * loadLogs 拉取日志尾部。
   *
   * 参数说明：
   * - silent: boolean，轮询时是否静默。
   *
   * 返回值说明：
   * 返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * tail 非数字时由后端按默认值处理；网络错误会写入 error 状态。
   */
  const loadLogs = useCallback(async (silent = false) => {
    try {
      const query = new URLSearchParams({ tail: tail || "200" });
      if (level) query.set("level", level);
      const data = await requestJSON(`/api/logs?${query.toString()}`);
      setLogs(data?.entries || []);
      setError("");
    } catch (err) {
      if (!silent) setError(err.message);
    }
  }, [tail, level]);

  useEffect(() => {
    loadLogs();
    const timer = paused ? 0 : window.setInterval(() => loadLogs(true), 4000);
    return () => { if (timer) window.clearInterval(timer); };
  }, [loadLogs, paused]);

  const visibleLogs = useMemo(() => {
    const normalized = query.trim().toLowerCase();
    return normalized ? logs.filter((entry) => entry.line.toLowerCase().includes(normalized)) : logs;
  }, [logs, query]);

  /**
   * copyLogs 把当前筛选结果复制到剪贴板。
   *
   * 参数说明：无。
   *
   * 返回值说明：返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 浏览器拒绝剪贴板权限时不修改页面；该诊断便捷操作不影响日志轮询。
   */
  async function copyLogs() {
    try {
      await navigator.clipboard.writeText(visibleLogs.map((entry) => entry.line).join("\n"));
    } catch (copyError) {
      setError(`复制日志失败：${copyError.message}`);
    }
  }

  /**
   * downloadLogs 下载当前筛选结果。
   *
   * 功能说明：
   * 使用临时 Blob URL 生成纯文本文件，并在点击后立即释放 URL，避免重复下载造成内存泄漏。
   *
   * 参数说明：无。
   *
   * 返回值说明：无。
   *
   * 可能的异常/错误情况：
   * 浏览器禁用下载时可能无响应，但不会影响日志数据与自动刷新。
   */
  function downloadLogs() {
    const blob = new Blob([visibleLogs.map((entry) => entry.line).join("\n")], { type: "text/plain;charset=utf-8" });
    const href = URL.createObjectURL(blob);
    const anchor = document.createElement("a");
    anchor.href = href;
    anchor.download = `proxyd-logs-${new Date().toISOString().replaceAll(":", "-")}.txt`;
    anchor.click();
    URL.revokeObjectURL(href);
  }

  return (
    <div className="stack">
      <PageHeader eyebrow="运行监测" title="运行日志" detail="按等级筛选最近日志，用于定位订阅、节点和本机集成问题。" />
      <section className="panel log-panel">
        <PanelTitle title="日志筛选与输出" detail={`${visibleLogs.length} 条结果${paused ? " · 自动刷新已暂停" : " · 每 4 秒自动刷新"}`} />
        <div className="toolbar logs-toolbar">
        <Field compact label="搜索日志"><div className="input-with-icon"><Search size={15} aria-hidden="true" /><Input value={query} placeholder="关键词" onChange={(event) => setQuery(event.target.value)} /></div></Field>
        <Field compact label="日志等级">
          <Select
            ariaLabel="日志等级"
            value={level}
            onValueChange={setLevel}
            options={[
              { value: "", label: "全部等级" },
              { value: "debug", label: "调试" },
              { value: "info", label: "信息" },
              { value: "warning", label: "警告" },
              { value: "error", label: "错误" },
            ]}
          />
        </Field>
        <Field compact label="显示条数"><input aria-label="显示日志条数" type="number" min="1" max="1000" value={tail} onChange={(event) => setTail(event.target.value)} /></Field>
        <Button className="toolbar-action" type="button" onClick={() => loadLogs(false)}>
          <RefreshCw size={16} aria-hidden="true" />
          <span>刷新</span>
        </Button>
        <Button variant="outline" type="button" onClick={() => setPaused((current) => !current)}>{paused ? <Play size={16} aria-hidden="true" /> : <Pause size={16} aria-hidden="true" />}{paused ? "继续" : "暂停"}</Button>
        <Button disabled={visibleLogs.length === 0} size="icon" variant="outline" type="button" aria-label="复制筛选后的日志" onClick={copyLogs}><Copy size={16} aria-hidden="true" /></Button>
        <Button disabled={visibleLogs.length === 0} size="icon" variant="outline" type="button" aria-label="下载筛选后的日志" onClick={downloadLogs}><Download size={16} aria-hidden="true" /></Button>
        </div>
        {error ? (
          <EmptyState title="日志加载失败" detail={error} />
        ) : (
          <div className="log-lines">
            {visibleLogs.length === 0 ? (
              <pre>暂无日志</pre>
            ) : (
              visibleLogs.map((entry) => (
                <pre className={classNames("log-line", `level-${entry.level || "info"}`)} key={`${entry.time}:${entry.line}`}>{entry.line}</pre>
              ))
            )}
          </div>
        )}
      </section>
    </div>
  );
}
