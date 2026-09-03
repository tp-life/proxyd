/**
 * proxyd Web 控制台入口模块。
 *
 * 功能说明：
 * 渲染 React 版控制台，并通过现有 `/api/*` REST 合约管理模式、订阅、节点、
 * 端口映射、规则、规则源、节点分组与本机设置。
 *
 * 参数说明：
 * 无外部入参；运行时从浏览器当前 origin 调用 proxyd API。
 *
 * 返回值说明：
 * 无显式返回值；模块加载后把 React 应用挂载到 `#root`。
 *
 * 可能的异常/错误情况：
 * 如果 `#root` 不存在、静态资源损坏、或后端 API 不可达，页面会显示加载/错误反馈。
 */

import React, { lazy, Suspense, useCallback, useEffect, useMemo, useState } from "react";
import { createRoot } from "react-dom/client";
import {
  Activity,
  Gauge,
  Laptop,
  Layers,
  Link2,
  ListFilter,
  Menu,
  Moon,
  Network,
  RefreshCw,
  Rss,
  Search,
  Settings,
  Shield,
  Sun,
  Terminal,
  X,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  ConfirmDialog,
  Dialog,
  DialogContent,
  DialogTitle,
} from "@/components/ui/dialog";
import { ToastViewport } from "@/components/ui/toast";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { EmptyState } from "@/components/EmptyState";
import { useConnectionsFeed } from "@/hooks/useConnectionsFeed";
import { useRemoteFeed } from "@/hooks/useRemoteFeed";
import { useToast } from "@/hooks/useToast";
import { useTrafficStream } from "@/hooks/useTrafficStream";
import { ConnectionsPage } from "@/pages/ConnectionsPage";
import { GroupsPage } from "@/pages/GroupsPage";
import { LogsPage } from "@/pages/LogsPage";
import { NodesPage } from "@/pages/NodesPage";
import { OverviewPage } from "@/pages/OverviewPage";
import { PortsPage } from "@/pages/PortsPage";
import { RulesPage } from "@/pages/RulesPage";
import { SettingsPage } from "@/pages/SettingsPage";
import { SubscriptionsPage } from "@/pages/SubscriptionsPage";
import { requestJSON, requestText } from "@/lib/api";
import { MODE_LABELS } from "@/lib/constants";
import { classNames, proxyEnvCommands, proxyURL } from "@/lib/format";
import "./styles.css";

/**
 * mapRemotePageModule 把 RemotePage 的命名导出适配为 React.lazy 所需的默认导出结构。
 *
 * 参数说明：module 为动态加载后的 RemotePage ES 模块。
 * 返回值说明：返回 `{default: React.ComponentType}`。
 * 可能的异常/错误情况：模块缺少 RemotePage 导出时，React 渲染阶段会报告无效组件。
 */
function mapRemotePageModule(module) {
  return { default: module.RemotePage };
}

/**
 * loadRemotePage 按需加载远程连接页面。
 *
 * 参数说明：无。
 * 返回值说明：返回 Promise，仅在用户进入“远程连接”页时下载对应页面代码。
 * 可能的异常/错误情况：静态资源加载失败时 Promise 拒绝，由 React 错误边界按现有策略处理。
 */
function loadRemotePage() {
  return import("@/pages/RemotePage").then(mapRemotePageModule);
}

// Remote 页面与更大的 xterm 运行时形成两级懒加载，首页不会携带远程终端相关实现。
const RemotePage = lazy(loadRemotePage);

const NAV_ITEMS = [
  { id: "overview", label: "运行概况", shortLabel: "概况", group: "概览", icon: Activity },
  { id: "nodes", label: "代理节点", shortLabel: "节点", group: "代理资源", icon: Network },
  { id: "subscriptions", label: "订阅管理", shortLabel: "订阅", group: "代理资源", icon: Rss },
  { id: "ports", label: "代理入口", shortLabel: "入口", group: "代理入口", icon: Gauge },
  { id: "groups", label: "策略分组", shortLabel: "分组", group: "代理入口", icon: Layers },
  { id: "rules", label: "访问规则", shortLabel: "规则", group: "代理入口", icon: ListFilter },
  { id: "connections", label: "活动连接", shortLabel: "连接", group: "连接与日志", icon: Link2 },
  { id: "logs", label: "运行日志", shortLabel: "日志", group: "连接与日志", icon: Terminal },
  { id: "remote", label: "远程连接", shortLabel: "远程", group: "远程访问", icon: Laptop },
  { id: "settings", label: "系统设置", shortLabel: "设置", group: "系统", icon: Settings },
];

/**
 * App 渲染 proxyd 控制台根组件。
 *
 * 参数说明：
 * 无。
 *
 * 返回值说明：
 * 返回 React 元素树。
 *
 * 可能的异常/错误情况：
 * API 不可达时保留旧数据并通过 toast 报错；写操作失败时展示后端错误文本。
 */
function App() {
  const { dismissToast, showToast, toasts } = useToast();
  const traffic = useTrafficStream(showToast);
  const [activeView, setActiveView] = useState("overview");
  const [overview, setOverview] = useState(null);
  const [ruleUrls, setRuleUrls] = useState([]);
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState("");
  const [mobileOpen, setMobileOpen] = useState(false);
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [portSort, setPortSort] = useState("default");
  const [groupSort, setGroupSort] = useState("default");
  const [nodeSourceFilter, setNodeSourceFilter] = useState("all");
  const [selectedNodes, setSelectedNodes] = useState(new Set());
  const [ruleContent, setRuleContent] = useState({});
  const [confirmation, setConfirmation] = useState(null);
  // 主题手动切换：默认深色，明亮模式选择持久化在 localStorage；
  // index.html 的内联脚本会在首屏前恢复，React 这里负责后续切换与写回。
  const [theme, setTheme] = useState(() => {
    try {
      return localStorage.getItem("proxyd-theme") === "light" ? "light" : "dark";
    } catch {
      return "dark";
    }
  });
  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    try {
      localStorage.setItem("proxyd-theme", theme);
    } catch {
      // 隐私模式等场景下放弃持久化，不影响本次切换
    }
  }, [theme]);
  const [forms, setForms] = useState({
    subscriptionURL: "",
    manualURL: "",
    manualName: "",
    rule: "",
    ruleURLName: "",
    ruleURL: "",
    groupName: "",
    groupPort: "",
    groupType: "fallback",
    groupSubscription: "",
    mainPort: "",
    rangeLo: "",
    rangeHi: "",
    autoPort: "",
  });

  const aliveCount = useMemo(
    () => (overview?.nodes || []).filter((node) => node.alive).length,
    [overview],
  );

  /**
   * load 拉取概览与规则源状态。
   *
   * 参数说明：
   * - silent: boolean，是否静默失败；轮询时设为 true，避免频繁打扰。
   *
   * 返回值说明：
   * 返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 后端不可达或 JSON 解析失败时，非静默调用会展示 toast。
   */
  const load = useCallback(
    async (silent = false) => {
      try {
        setLoading(true);
        const [nextOverview, nextRuleUrls] = await Promise.all([
          requestJSON("/api/overview"),
          requestJSON("/api/rule-urls"),
        ]);
        setOverview(nextOverview);
        setRuleUrls(nextRuleUrls || []);
        setForms((current) => ({
          ...current,
          mainPort: current.mainPort || String(nextOverview.mixed_port || ""),
          rangeLo: current.rangeLo || String(nextOverview.port_range?.[0] || ""),
          rangeHi: current.rangeHi || String(nextOverview.port_range?.[1] || ""),
          autoPort: current.autoPort || String(nextOverview.auto_port || 41998),
        }));
      } catch (error) {
        if (!silent) showToast(`加载失败：${error.message}`, "err");
      } finally {
        setLoading(false);
      }
    },
    [showToast],
  );

  /**
   * postJSON 执行写入类 API。
   *
   * 参数说明：
   * - url: string，API 路径。
   * - body: object，要发送的 JSON 请求体。
   * - message: string，成功后的提示。
   * - method: string，HTTP 方法，默认 POST；编辑资源时传 PUT。
   * - signal: AbortSignal | undefined，可选取消信号；订阅保存等长耗时请求由调用方传入，
   *   用户点击取消时中断等待（后端事务可能仍在收尾，但 UI 立即解锁）。
   *
   * 返回值说明：
   * 成功返回 true，失败或被取消返回 false。
   *
   * 可能的异常/错误情况：
   * 后端校验失败、网络失败或 JSON 解析失败时 toast 错误并返回 false；AbortError 视为用户主动取消，toast 提示「已取消」。
   */
  const postJSON = useCallback(
    async (url, body, message, method = "POST", signal) => {
      try {
        await requestJSON(url, { method, body: JSON.stringify(body), signal });
        if (message) showToast(message);
        await load(true);
        return true;
      } catch (error) {
        if (error?.name === "AbortError") {
          showToast("已取消本次操作", "err");
          return false;
        }
        showToast(`操作失败：${error.message}`, "err");
        return false;
      }
    },
    [load, showToast],
  );

  /**
   * requestConfirmation 打开统一确认对话框并等待用户选择。
   *
   * 参数说明：
   * - options: {title: string, description: string, confirmLabel?: string, destructive?: boolean}，确认操作的可见信息。
   *
   * 返回值说明：
   * 返回 Promise<boolean>；确认返回 true，取消或关闭返回 false。
   *
   * 可能的异常/错误情况：
   * 无；同一时刻只允许一个确认请求，后发请求会替换前一个请求，因此调用方应在用户完成选择前避免重复触发。
   */
  const requestConfirmation = useCallback((options) => {
    if (confirmation) return Promise.resolve(false);
    return new Promise((resolve) => {
      setConfirmation({ ...options, resolve });
    });
  }, [confirmation]);

  /**
   * settleConfirmation 完成当前确认请求并关闭对话框。
   *
   * 参数说明：
   * - accepted: boolean，用户是否确认执行。
   *
   * 返回值说明：
   * 无；通过此前保存的 Promise resolve 把结果交还给业务操作。
   *
   * 可能的异常/错误情况：
   * 当前没有确认请求时直接返回，避免重复关闭产生异常。
   */
  const settleConfirmation = useCallback((accepted) => {
    if (!confirmation) return;
    confirmation.resolve(accepted);
    setConfirmation(null);
  }, [confirmation]);

  /**
   * deleteJSON 执行删除类 API。
   *
   * 参数说明：
   * - url: string，API 路径。
   * - message: string，成功后的提示。
   * - target: string，供确认框识别待删除对象的中文名称。
   *
   * 返回值说明：
   * 成功返回 true，失败返回 false。
   *
   * 可能的异常/错误情况：
   * 后端返回非 2xx 或网络失败时 toast 错误并返回 false。
   */
  const deleteJSON = useCallback(
    async (url, message, target = "该项目") => {
      const accepted = await requestConfirmation({
        title: `删除${target}？`,
        description: "删除后无法从控制台恢复，请确认当前配置不再需要它。",
        confirmLabel: "确认删除",
        destructive: true,
      });
      if (!accepted) return false;
      try {
        await requestJSON(url, { method: "DELETE" });
        if (message) showToast(message);
        await load(true);
        return true;
      } catch (error) {
        showToast(`删除失败：${error.message}`, "err");
        return false;
      }
    },
    [load, requestConfirmation, showToast],
  );

  /**
   * useConnectionsFeed 接入活动连接页的加载与关闭能力。
   *
   * 功能说明：
   * 这个 hook 只在活动连接页可见时工作，避免后台页签继续打 `/api/connections`。
   * 它把页面所需的列表、摘要、筛选、单条关闭和关闭全部操作集中起来，App 层只
   * 需要挂一次数据源即可。
   *
   * 参数说明：
   * - activeView: string，当前激活视图名称，用于控制是否加载。
   * - requestConfirmation: Function，全局确认对话框请求函数。
   * - showToast: Function，全局 toast 展示函数。
   *
   * 返回值说明：
   * 返回活动连接页所需的状态与操作方法对象。
   *
   * 可能的异常/错误情况：
   * 接口失败时由 hook 内部承接到错误条带；调用方无需额外捕获。
   */
  const connections = useConnectionsFeed(activeView, requestConfirmation, showToast);

  /**
   * useRemoteFeed 接入远程连接页的加载与管理能力。
   *
   * 功能说明：
   * 与活动连接页相同，这个 hook 只在远程连接页可见时工作，避免后台页签继续请求
   * `/api/remote*`。App 层只挂一次数据源，页面通过展开 props 消费。
   *
   * 参数说明：
   * - activeView: string，当前激活视图名称，用于控制是否加载。
   * - requestConfirmation: Function，全局确认对话框请求函数。
   * - showToast: Function，全局 toast 展示函数。
   *
   * 返回值说明：
   * 返回远程连接页所需的状态与操作方法对象。
   *
   * 可能的异常/错误情况：
   * 接口失败时由 hook 内部承接到错误条带；调用方无需额外捕获。
   */
  const remote = useRemoteFeed(activeView, requestConfirmation, showToast);

  /**
   * importConfig 上传 YAML 配置，后端校验并原子替换配置文件。
   *
   * 参数说明：
   * - file: File，用户从文件选择器选中的 YAML 文件。
   *
   * 返回值说明：
   * 返回 Promise<void>；成功后提示必须重启，当前页面不伪装成已热更新。
   *
   * 可能的异常/错误情况：
   * 文件超过 1 MiB、读取失败、预检失败、摘要内容变化或写盘失败时通过 toast 展示原因。
   */
  const importConfig = useCallback(async (file) => {
    if (!file) return;
    if (file.size > 1024 * 1024) {
      showToast("导入失败：配置文件不能超过 1 MiB", "err");
      return;
    }
    try {
      const body = await file.text();
      const preview = await requestJSON("/api/config/import/preview", {
        method: "POST",
        headers: { "Content-Type": "application/yaml" },
        body,
      });
      const labels = {
        subscriptions: "订阅",
        manual_nodes: "手动节点",
        groups: "策略分组",
        custom_rules: "自定义规则",
        rule_urls: "远程规则源",
      };
      const countSummary = Object.entries(preview?.counts || {})
        .map(([key, value]) => `${labels[key] || key} ${value.before}→${value.after}`)
        .join("；");
      const fieldSummary = (preview?.changed_fields || []).length
        ? `关键变更：${preview.changed_fields.join("、")}。`
        : "关键运行字段未变化。";
      const warningSummary = (preview?.warnings || []).length
        ? ` 注意：${preview.warnings.join("；")}。`
        : "";
      const accepted = await requestConfirmation({
        title: "确认导入预检结果？",
        description: `${countSummary || "对象数量无变化"}。${fieldSummary}${warningSummary} 导入后需要重启 proxyd。`,
        confirmLabel: "确认替换配置",
        destructive: true,
      });
      if (!accepted) return;
      const result = await requestJSON("/api/config/import", {
        method: "POST",
        headers: {
          "Content-Type": "application/yaml",
          "X-Proxyd-Config-Digest": preview.digest,
        },
        body,
      });
      showToast(result?.message || "配置已导入，请重启 proxyd");
    } catch (error) {
      showToast(`导入失败：${error.message}`, "err");
    }
  }, [requestConfirmation, showToast]);

  /**
   * restartApp 经确认后请求后端重启进程，并轮询 /healthz 等待服务恢复。
   *
   * 参数说明：无；复用全局确认框、请求封装和 toast。
   *
   * 返回值说明：
   * 返回 Promise<void>；健康检查恢复后刷新页面，让全部状态从新进程重新加载。
   *
   * 可能的异常/错误情况：
   * 请求被拒绝时 toast 错误；重启超时（例如导入配置修改了 API 监听地址）时提示
   * 用户手动访问新地址，不做无意义的无限等待。
   */
  const restartApp = useCallback(async () => {
    const accepted = await requestConfirmation({
      title: "重启 proxyd？",
      description: "重启期间代理入口与控制台会短暂中断，通常几秒内自动恢复。",
      confirmLabel: "立即重启",
      destructive: true,
    });
    if (!accepted) return;
    try {
      await requestJSON("/api/restart", { method: "POST" });
    } catch (error) {
      showToast(`重启失败：${error.message}`, "err");
      return;
    }
    showToast("proxyd 正在重启，等待服务恢复…");
    const deadline = Date.now() + 20000;
    while (Date.now() < deadline) {
      await new Promise((resolve) => setTimeout(resolve, 800));
      try {
        const resp = await fetch("/healthz", { cache: "no-store" });
        if (resp.ok) {
          showToast("重启完成，正在刷新页面");
          window.location.reload();
          return;
        }
      } catch {
        // 进程尚未恢复，继续等待
      }
    }
    showToast("重启超时：若导入的配置修改了 API 监听地址，请手动访问新地址", "err");
  }, [requestConfirmation, showToast]);

  /**
   * triggerOperation 触发刷新或测速后台任务。
   *
   * 参数说明：
   * - url: string，API 路径。
   * - label: string，操作名称。
   *
   * 返回值说明：
   * 返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 后端拒绝请求时展示错误；后台任务本身的异步失败由服务端日志记录。
   */
  const triggerOperation = useCallback(
    async (url, label) => {
      try {
        setBusy(label);
        await requestJSON(url, { method: "POST" });
        showToast(`${label}已开始`);
        await load(true);
      } catch (error) {
        showToast(`${label}失败：${error.message}`, "err");
      } finally {
        window.setTimeout(() => setBusy(""), 1800);
      }
    },
    [load, showToast],
  );

  const commands = useMemo(
    () => buildCommands(overview, setActiveView, runCommandAction, triggerOperation, postJSON),
    [overview, triggerOperation, postJSON],
  );

  /**
   * runCommandAction 执行命令面板动作。
   *
   * 参数说明：
   * - action: Function，命令动作回调。
   *
   * 返回值说明：
   * 返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 回调异常会被 toast 展示，避免命令面板静默失败。
   */
  async function runCommandAction(action) {
    try {
      setPaletteOpen(false);
      setQuery("");
      await action();
    } catch (error) {
      showToast(`命令失败：${error.message}`, "err");
    }
  }

  useEffect(() => {
    load();
    const timer = window.setInterval(() => load(true), 10000);
    return () => window.clearInterval(timer);
  }, [load]);

  useEffect(() => {
    /**
     * onKeyDown 打开或关闭命令面板。
     *
     * 参数说明：
     * - event: KeyboardEvent，浏览器键盘事件。
     *
     * 返回值说明：
     * 无。
     *
     * 可能的异常/错误情况：
     * 无；仅处理 Meta/Ctrl+K 与 Escape。
     */
    function onKeyDown(event) {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        setPaletteOpen(true);
      }
      if (event.key === "Escape") setPaletteOpen(false);
    }
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, []);

  /**
   * updateForm 更新表单字段。
   *
   * 参数说明：
   * - key: string，字段名。
   * - value: string，字段值。
   *
   * 返回值说明：
   * 无。
   *
   * 可能的异常/错误情况：
   * 无；未知字段仍会写入 forms，用于保持组件简单。
   */
  function updateForm(key, value) {
    setForms((current) => ({ ...current, [key]: value }));
  }

  /**
   * toggleNodeSelection 切换分组待选节点。
   *
   * 参数说明：
   * - name: string，节点名。
   *
   * 返回值说明：
   * 无。
   *
   * 可能的异常/错误情况：
   * 无；如果节点随后被订阅刷新移除，提交时后端会再次校验。
   */
  function toggleNodeSelection(name) {
    setSelectedNodes((current) => {
      const next = new Set(current);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });
  }

  /**
   * copyProxyURL 复制代理地址。
   *
   * 参数说明：
   * - port: number，目标端口。
   *
   * 返回值说明：
   * 返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 浏览器剪贴板权限被拒绝时展示错误。
   */
  async function copyProxyURL(port) {
    try {
      const text = proxyURL(overview?.listen, port);
      await navigator.clipboard.writeText(text);
      showToast(`已复制 ${text}`);
    } catch (error) {
      showToast(`复制失败：${error.message}`, "err");
    }
  }

  /**
   * copyProxyEnv 复制指定端口的 shell 代理环境变量。
   *
   * 参数说明：
   * - port: number，目标端口。
   *
   * 返回值说明：返回 Promise<void>。
   *
   * 可能的异常/错误情况：浏览器剪贴板权限被拒绝时展示错误。
   */
  async function copyProxyEnv(port) {
    try {
      await navigator.clipboard.writeText(proxyEnvCommands(overview?.listen, port));
      showToast(`已复制端口 ${port} 的环境变量（http_proxy/https_proxy/all_proxy）`);
    } catch (error) {
      showToast(`复制失败：${error.message}`, "err");
    }
  }

  /**
   * applyMainPolicy 把界面中的互斥“主入口策略”翻译为现有后端配置。
   *
   * 功能说明：
   * 后端为了兼容配置文件，仍使用 `main_auto` 与 `main_node` 两个字段表达优先级；
   * 界面则只允许用户看到规则、自动最快、固定节点三个互斥选择。切换时先清除会与
   * 目标策略冲突的状态，避免关闭自动选择后意外恢复一个旧的固定节点。
   *
   * 参数说明：
   * - policy: "rule" | "auto" | "fixed"，用户选择的主入口策略。
   *
   * 返回值说明：
   * 返回 Promise<void>；接口全部成功后概览会通过 postJSON 自动重新加载。
   *
   * 可能的异常/错误情况：
   * 任一步接口写入失败时由 postJSON 展示错误并停止后续切换；尚未配置固定节点时
   * 跳转到系统设置，不替用户擅自选择节点。
   */
  async function applyMainPolicy(policy) {
    /*
     * “规则分流”在持久化层不是独立字段，而是 main_auto=false 且 main_node 为空的
     * 组合状态。必须先关闭自动选优再清空固定节点：main_auto 的优先级最高，如果
     * 只清空 main_node，主入口仍会继续自动选节点，界面与实际流量会不一致。
     */
    if (policy === "rule") {
      if (overview?.main_auto && !(await postJSON("/api/main-auto", { enabled: false }, "已关闭主端口自动选优"))) return;
      if (overview?.main_node) await postJSON("/api/main-node", { node: "" }, "主端口已恢复规则分流");
      return;
    }
    /*
     * 自动选优需要先清除旧固定节点。虽然 main_auto=true 时后端会覆盖 main_node，
     * 但保留旧值会导致用户以后关闭自动模式时悄悄恢复过期节点，所以这里主动清理
     * 配置意图，使一次切换只对应一个明确策略。
     */
    if (policy === "auto") {
      if (overview?.main_node && !(await postJSON("/api/main-node", { node: "" }, "已清除固定节点"))) return;
      if (!overview?.main_auto) await postJSON("/api/main-auto", { enabled: true }, "主端口已切换为自动最快");
      return;
    }
    /*
     * 固定节点策略不能凭空推断目标节点：没有 main_node 时跳到设置页，让用户明确
     * 选择。已经保存节点但自动选优仍开启时只关闭 main_auto，保留 main_node 作为
     * 用户选择。若该节点暂时不可用，后端会运行时回退到由 mode 决定的顶层主入口，
     * 但不会删除配置，节点恢复后仍可继续使用原意图。
     */
    if (policy === "fixed") {
      if (!overview?.main_node) {
        setActiveView("settings");
        return;
      }
      if (overview.main_auto) await postJSON("/api/main-auto", { enabled: false }, "主端口已切换为固定节点");
    }
  }

  /**
   * submitGroup 创建节点分组。
   *
   * 参数说明：
   * - currentName: string，现有分组名；空值表示新增，非空表示原位编辑。
   *
   * 返回值说明：
   * 返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 表单缺失时本地拦截；端口冲突等由后端返回。
   */
  async function submitGroup(currentName = "") {
    const port = Number.parseInt(forms.groupPort, 10);
    const nodes = [...selectedNodes];
    const subscription = forms.groupSubscription.trim();
    if (!forms.groupName.trim() || !port || (!subscription && nodes.length === 0)) {
      showToast("请填写分组名、端口，并选择节点或订阅来源", "err");
      return false;
    }
    if (await postJSON(currentName ? `/api/groups/${encodeURIComponent(currentName)}` : "/api/groups", {
      name: forms.groupName.trim(),
      port,
      type: forms.groupType || "fallback",
      subscription,
      nodes: subscription ? [] : nodes,
    }, currentName ? "分组已更新" : "分组已添加", currentName ? "PUT" : "POST")) {
      updateForm("groupName", "");
      updateForm("groupPort", "");
      updateForm("groupType", "fallback");
      updateForm("groupSubscription", "");
      setSelectedNodes(new Set());
      return true;
    }
    return false;
  }

  /**
   * submitRuleURLContent 展开或收起规则源内容。
   *
   * 参数说明：
   * - name: string，规则源名称。
   *
   * 返回值说明：
   * 返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 规则源不存在或拉取失败时展示错误文本并保留展开态。
   */
  async function submitRuleURLContent(name) {
    const current = ruleContent[name] || { open: false, text: "" };
    if (current.open) {
      setRuleContent((state) => ({ ...state, [name]: { ...current, open: false } }));
      return;
    }
    setRuleContent((state) => ({ ...state, [name]: { ...current, open: true, text: current.text || "加载中..." } }));
    if (current.text) return;
    try {
      const text = await requestText(`/api/rule-urls/${encodeURIComponent(name)}/content`);
      setRuleContent((state) => ({ ...state, [name]: { open: true, text } }));
    } catch (error) {
      setRuleContent((state) => ({ ...state, [name]: { open: true, text: `加载失败：${error.message}` } }));
    }
  }

  const filteredCommands = commands.filter((command) =>
    `${command.label} ${command.group}`.toLowerCase().includes(query.toLowerCase()),
  );

  return (
    <TooltipProvider delayDuration={250}>
      <div className="app-shell">
      <Sidebar activeView={activeView} connected={Boolean(overview)} mobileOpen={mobileOpen} theme={theme} onNavigate={setActiveView} onClose={() => setMobileOpen(false)} onPalette={() => setPaletteOpen(true)} onToggleTheme={() => setTheme((current) => (current === "light" ? "dark" : "light"))} />
      <main className={classNames("workspace", activeView === "overview" && "overview-workspace")}>
        {!overview ? (
          <EmptyState title="正在连接 proxyd" detail="等待 /api/overview 返回运行状态。" />
        ) : (
          <div className="view-stage view-enter" key={activeView}>
            {activeView === "overview" && (
              <OverviewPage
                aliveCount={aliveCount}
                busy={busy}
                loading={loading}
                overview={overview}
                traffic={traffic}
                onCopy={copyProxyURL}
                onCopyEnv={copyProxyEnv}
                onMenu={() => setMobileOpen(true)}
                onMode={(mode) => postJSON("/api/mode", { mode }, `已切换到${MODE_LABELS[mode]}模式`)}
                onNavigate={setActiveView}
                onPalette={() => setPaletteOpen(true)}
                onPolicy={applyMainPolicy}
                onPortMapping={(enabled) => postJSON("/api/port-mapping", { enabled }, enabled ? "节点端口映射已开启" : "节点端口映射已关闭")}
                onRefresh={() => triggerOperation("/api/refresh", "刷新订阅")}
                onSystemProxy={(enabled) => postJSON("/api/system-proxy", { enabled }, enabled ? "系统代理已开启" : "系统代理已关闭")}
                onTest={() => triggerOperation("/api/test", "测速")}
                onTun={(enabled) => postJSON("/api/tun", { enabled }, enabled ? "TUN 已开启" : "TUN 已关闭")}
              />
            )}
            {activeView === "nodes" && (
              <NodesPage
                busy={busy}
                forms={forms}
                initialSource={nodeSourceFilter}
                overview={overview}
                onDelete={deleteJSON}
                onForm={updateForm}
                onMainNode={(node) => postJSON("/api/main-node", { node }, node ? "主端口已固定到该节点" : "主端口已恢复规则模式")}
                onPost={postJSON}
                onSourceChange={setNodeSourceFilter}
                onTest={() => triggerOperation("/api/test", "测速")}
              />
            )}
            {activeView === "subscriptions" && (
              <SubscriptionsPage
                overview={overview}
                onDelete={deleteJSON}
                onNavigateNodes={(source) => {
                  setNodeSourceFilter(source);
                  setActiveView("nodes");
                }}
                onSubAction={triggerSubscriptionAction}
                onWrite={postJSON}
              />
            )}
            {activeView === "ports" && (
              <PortsPage
                overview={overview}
                portSort={portSort}
                onCopy={copyProxyURL}
                onCopyEnv={copyProxyEnv}
                onSort={setPortSort}
                onToggle={(enabled) => postJSON("/api/port-mapping", { enabled }, enabled ? "节点端口映射已开启" : "节点端口映射已关闭")}
              />
            )}
            {activeView === "groups" && (
              <GroupsPage
                forms={forms}
                groupSort={groupSort}
                overview={overview}
                selectedNodes={selectedNodes}
                onCopy={copyProxyURL}
                onDelete={deleteJSON}
                onForm={updateForm}
                onPost={postJSON}
                onSort={setGroupSort}
                onSubmit={submitGroup}
                onToggleNode={toggleNodeSelection}
              />
            )}
            {activeView === "rules" && (
              <RulesPage
                forms={forms}
                ruleContent={ruleContent}
                ruleUrls={ruleUrls}
                overview={overview}
                onDelete={deleteJSON}
                onForm={updateForm}
                onPost={postJSON}
                onViewContent={submitRuleURLContent}
              />
            )}
            {activeView === "logs" && <LogsPage />}
            {activeView === "connections" && <ConnectionsPage {...connections} />}
            {activeView === "remote" && (
              <Suspense fallback={<EmptyState title="正在加载远程连接页面" detail="首次进入时按需加载远程管理与终端入口。" />}>
                <RemotePage {...remote} />
              </Suspense>
            )}
            {activeView === "settings" && (
              <SettingsPage
                forms={forms}
                overview={overview}
                onForm={updateForm}
                onImportConfig={importConfig}
                onPost={postJSON}
                onRestart={restartApp}
              />
            )}
          </div>
        )}
      </main>
      {paletteOpen && (
        <CommandPalette
          commands={filteredCommands}
          query={query}
          onQuery={setQuery}
          onClose={() => setPaletteOpen(false)}
        />
      )}
      <ConfirmDialog
        open={Boolean(confirmation)}
        title={confirmation?.title || "确认操作"}
        description={confirmation?.description || ""}
        confirmLabel={confirmation?.confirmLabel || "确认"}
        destructive={Boolean(confirmation?.destructive)}
        onConfirm={() => settleConfirmation(true)}
        onOpenChange={(open) => {
          if (!open) settleConfirmation(false);
        }}
      />
      <ToastViewport
        maxVisible={3}
        onDismiss={dismissToast}
        toasts={toasts}
      />
      </div>
    </TooltipProvider>
  );

  /**
   * triggerSubscriptionAction 执行单个订阅刷新或测速。
   *
   * 参数说明：
   * - name: string，订阅名称。
   * - action: string，`refresh` 或 `test`。
   *
   * 返回值说明：
   * 返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 订阅不存在、后端超时或网络失败时展示错误。
   */
  async function triggerSubscriptionAction(name, action) {
    const label = action === "refresh" ? "刷新" : "测速";
    try {
      setBusy(`${name} ${label}`);
      await requestJSON(`/api/subscriptions/${encodeURIComponent(name)}/${action}`, { method: "POST" });
      showToast(`${name} ${label}完成`);
      await load(true);
    } catch (error) {
      showToast(`${name} ${label}失败：${error.message}`, "err");
    } finally {
      setBusy("");
    }
  }
}

/**
 * buildCommands 生成命令面板命令。
 *
 * 参数说明：
 * - overview: object | null，当前概览数据。
 * - setActiveView: Function，切换页面的 setter。
 * - runCommandAction: Function，统一执行命令的包装器。
 * - triggerOperation: Function，刷新/测速动作包装器。
 * - postJSON: Function，模式切换写入包装器。
 *
 * 返回值说明：
 * 返回命令对象数组。
 *
 * 可能的异常/错误情况：
 * 无；真正的命令错误由 runCommandAction 处理。
 */
function buildCommands(overview, setActiveView, runCommandAction, triggerOperation, postJSON) {
  const navCommands = NAV_ITEMS.map((item) => ({
    group: "跳转",
    label: item.label,
    run: () => runCommandAction(() => setActiveView(item.id)),
  }));
  const modeCommands = Object.entries(MODE_LABELS).map(([mode, label]) => ({
    group: "模式",
    label: `切换到${label}`,
    run: () =>
      runCommandAction(() =>
        postJSON("/api/mode", { mode }, `已切换到${label}模式`),
      ),
  }));
  const operationCommands = [
    {
      group: "操作",
      label: "刷新订阅",
      run: () => runCommandAction(() => triggerOperation("/api/refresh", "刷新订阅")),
    },
    {
      group: "操作",
      label: "测速",
      run: () => runCommandAction(() => triggerOperation("/api/test", "测速")),
    },
  ];
  return overview ? [...navCommands, ...operationCommands, ...modeCommands] : navCommands;
}

/**
 * Sidebar 渲染桌面侧边栏与移动端抽屉导航。
 *
 * 参数说明：
 * - activeView: string，当前页面 id。
 * - connected: boolean，是否已取得后端运行状态。
 * - mobileOpen: boolean，移动端抽屉是否展开。
 * - theme: string，当前主题（dark/light）。
 * - onNavigate: Function，切换页面回调。
 * - onClose: Function，关闭移动端抽屉回调。
 * - onPalette: Function，打开全局命令菜单。
 * - onToggleTheme: Function，切换深色/明亮主题。
 *
 * 返回值说明：
 * 返回导航 React 元素。
 *
 * 可能的异常/错误情况：
 * 无。
 */
function Sidebar({ activeView, connected, mobileOpen, theme, onNavigate, onClose, onPalette, onToggleTheme }) {
  return (
    <>
      <aside className={classNames("sidebar", mobileOpen && "open")}>
        <div className="brand">
          <span className="brand-mark"><Shield size={18} /></span>
          <span className="brand-copy"><b>proxyd</b><small>代理控制台</small></span>
          <Tooltip>
            <TooltipTrigger asChild>
              <Button className="sidebar-command" size="icon" variant="ghost" type="button" onClick={onPalette} aria-label="打开命令菜单">
                <Search size={16} aria-hidden="true" />
              </Button>
            </TooltipTrigger>
            <TooltipContent>命令菜单 · ⌘K</TooltipContent>
          </Tooltip>
          <Tooltip>
            <TooltipTrigger asChild>
              <Button size="icon" variant="ghost" type="button" onClick={onToggleTheme} aria-label={theme === "light" ? "切换到深色模式" : "切换到明亮模式"}>
                {theme === "light" ? <Moon size={16} aria-hidden="true" /> : <Sun size={16} aria-hidden="true" />}
              </Button>
            </TooltipTrigger>
            <TooltipContent>{theme === "light" ? "切换到深色模式" : "切换到明亮模式"}</TooltipContent>
          </Tooltip>
          <Button className="mobile-only nav-close" size="icon" variant="ghost" type="button" onClick={onClose} aria-label="关闭导航">
            <X size={18} aria-hidden="true" />
          </Button>
        </div>
        <nav className="nav-list" aria-label="主导航">
          {NAV_ITEMS.map((item, index) => {
            const Icon = item.icon;
            const previous = NAV_ITEMS[index - 1];
            const startsSection = !previous || previous.group !== item.group;
            return (
              <React.Fragment key={item.id}>
                {/*
                  分组标题直接呈现用户的任务层级，而不是只依赖一条无文案的分隔线。
                  首组“概览”无需额外标题，避免品牌区下方出现重复的运行概况文案。
                */}
                {startsSection && item.group !== "概览" && <span className="nav-heading">{item.group}</span>}
                <button
                  aria-current={activeView === item.id ? "page" : undefined}
                  className={classNames("nav-item", activeView === item.id && "active")}
                  type="button"
                  onClick={() => {
                    onNavigate(item.id);
                    onClose();
                  }}
                >
                  <Icon size={18} aria-hidden="true" />
                  <span>{item.label}</span>
                </button>
              </React.Fragment>
            );
          })}
        </nav>
        <div className={classNames("sidebar-status", !connected && "pending")}><i aria-hidden="true" /><span>本机服务</span><b>{connected ? "运行中" : "连接中"}</b></div>
      </aside>
      {mobileOpen && <button className="scrim" type="button" aria-label="关闭导航遮罩" onClick={onClose} />}
    </>
  );
}

/**
 * Topbar 渲染顶部操作栏。
 *
 * 参数说明：
 * - activeView: string，当前页面 id。
 * - busy: string，正在执行的操作名称。
 * - loading: boolean，是否正在轮询加载。
 * - onMenu/onPalette/onRefresh/onTest: Function，按钮动作回调。
 *
 * 返回值说明：
 * 返回顶部栏 React 元素。
 *
 * 可能的异常/错误情况：
 * 无；动作失败由父组件处理。
 */
function Topbar({ activeView, busy, loading, onMenu, onPalette, onRefresh, onTest }) {
  const current = NAV_ITEMS.find((item) => item.id === activeView);
  const status = busy || (loading ? "正在同步状态" : "状态已同步");
  return (
    <header className="topbar">
      <div className="title-row">
        <Button className="mobile-only" size="icon" variant="outline" type="button" onClick={onMenu} aria-label="打开导航">
          <Menu size={18} aria-hidden="true" />
        </Button>
        <div>
          <h1>{current?.label || "运行概况"}</h1>
          <span className={classNames("top-status", (busy || loading) && "working")}><i aria-hidden="true" />{status}</span>
        </div>
      </div>
      <div className="top-actions">
        <Tooltip>
          <TooltipTrigger asChild>
            <Button className="command-button" size="icon" variant="outline" type="button" onClick={onPalette} aria-label="打开命令菜单">
              <Search size={16} aria-hidden="true" />
            </Button>
          </TooltipTrigger>
          <TooltipContent>命令菜单 · ⌘K</TooltipContent>
        </Tooltip>
        <Button disabled={Boolean(busy)} loading={busy === "测速"} variant="outline" type="button" onClick={onTest}>
          {busy !== "测速" && <Gauge size={16} aria-hidden="true" />}
          <span className="desktop-action-label">{busy === "测速" ? "测速中…" : "测试节点"}</span>
          <span className="mobile-action-label">{busy === "测速" ? "测速中" : "测速"}</span>
        </Button>
        <Button disabled={Boolean(busy)} loading={busy === "刷新订阅"} type="button" onClick={onRefresh}>
          {busy !== "刷新订阅" && <RefreshCw className={classNames((busy || loading) && "animate-spin")} size={16} aria-hidden="true" />}
          <span className="desktop-action-label">{busy === "刷新订阅" ? "同步中…" : "同步订阅"}</span>
          <span className="mobile-action-label">{busy === "刷新订阅" ? "同步中" : "同步"}</span>
        </Button>
      </div>
    </header>
  );
}

/**
 * CommandPalette 渲染命令面板。
 *
 * 参数说明：
 * - commands: Array<object>，可执行命令列表。
 * - query: string，搜索词。
 * - onQuery/onClose: Function，输入与关闭回调。
 *
 * 返回值说明：
 * 返回命令面板 React 元素。
 *
 * 可能的异常/错误情况：
 * 单个命令失败由命令自身包装处理。
 */
function CommandPalette({ commands, query, onQuery, onClose }) {
  return (
    <Dialog open onOpenChange={(open) => { if (!open) onClose(); }}>
      <DialogContent aria-labelledby="command-title" className="palette" showClose={false}>
        <DialogTitle className="sr-only" id="command-title">命令菜单</DialogTitle>
        <div className="palette-input">
          <Search size={18} />
          <input autoFocus value={query} onChange={(event) => onQuery(event.target.value)} placeholder="搜索命令或页面" />
        </div>
        <div className="palette-list">
          {commands.map((command) => (
            <button key={`${command.group}:${command.label}`} type="button" onClick={command.run}>
              <span>{command.label}</span>
              <small>{command.group}</small>
            </button>
          ))}
        </div>
      </DialogContent>
    </Dialog>
  );
}

const root = document.getElementById("root");
createRoot(root).render(<App />);
