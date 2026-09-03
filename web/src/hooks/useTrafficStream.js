import { useEffect, useState } from "react";

/**
 * useTrafficStream 消费后端代理的 mihomo `/traffic` NDJSON 流。
 *
 * 参数说明：
 * - showToast: Function，首次连接失败时展示错误。
 *
 * 返回值说明：
 * 返回 `{ up, down, upTotal, downTotal, connected, error, history }` 实时状态，
 * history 最多保留最近 60 个采样点，供概览页绘制轻量趋势图。
 *
 * 可能的异常/错误情况：
 * 流中断时会自动延迟重连；浏览器不支持 ReadableStream 时显示错误状态。
 */
export function useTrafficStream(showToast) {
  const [traffic, setTraffic] = useState({
    up: 0,
    down: 0,
    upTotal: 0,
    downTotal: 0,
    connected: false,
    error: "",
    technicalError: "",
    history: [],
  });

  useEffect(() => {
    let stopped = false;
    let warned = false;
    let retryTimer = 0;
    let controller = null;

    /**
     * connect 建立一次流量流连接并逐行解析 JSON。
     *
     * 参数说明：无。
     * 返回值说明：返回 Promise<void>，连接结束后由 finally 决定是否重连。
     * 可能的异常/错误情况：网络错误、HTTP 错误和 JSON 行损坏都会进入错误状态。
     */
    async function connect() {
      controller = new AbortController();
      try {
        const response = await fetch("/api/traffic", { signal: controller.signal });
        if (!response.ok) throw new Error(await response.text() || `HTTP ${response.status}`);
        if (!response.body) throw new Error("浏览器不支持流式响应");
        setTraffic((current) => ({ ...current, connected: true, error: "", technicalError: "" }));
        const reader = response.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });
          const lines = buffer.split("\n");
          buffer = lines.pop() || "";
          for (const line of lines) {
            const trimmed = line.trim();
            if (!trimmed) continue;
            const next = JSON.parse(trimmed);
            setTraffic((current) => {
              const sample = {
                at: Date.now(),
                up: Number(next.up) || 0,
                down: Number(next.down) || 0,
              };
              return {
                up: sample.up,
                down: sample.down,
                upTotal: next.upTotal || 0,
                downTotal: next.downTotal || 0,
                connected: true,
                error: "",
                technicalError: "",
                // 仅保存 60 个点，既能表达短期趋势，也避免长时间打开页面后持续占用内存。
                history: [...(current.history || []), sample].slice(-60),
              };
            });
          }
        }
      } catch (error) {
        if (stopped || error.name === "AbortError") return;
        setTraffic((current) => ({
          ...current,
          connected: false,
          error: "实时速率暂不可用，正在自动重连",
          technicalError: error.message.trim(),
        }));
        if (!warned) {
          warned = true;
          showToast("实时速率暂不可用，页面会自动重试", "err");
        }
      } finally {
        if (!stopped) retryTimer = window.setTimeout(connect, 3000);
      }
    }

    connect();
    return () => {
      stopped = true;
      window.clearTimeout(retryTimer);
      if (controller) controller.abort();
    };
  }, [showToast]);

  return traffic;
}
