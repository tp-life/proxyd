import { ModulesPage } from "@/pages/ModulesPage";
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
  ChevronDown,
  Gauge,
  Laptop,
  Layers,
  Link2,
  ListFilter,
  Menu,
  Monitor,
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
import { useDesktopFeed } from "@/hooks/useDesktopFeed";
import { useRemoteFeed } from "@/hooks/useRemoteFeed";
import { useGatewayFeed } from "@/hooks/useGatewayFeed";
import { useToast } from "@/hooks/useToast";
import { useTrafficStream } from "@/hooks/useTrafficStream";
import { ConnectionsPage } from "@/pages/ConnectionsPage";
import { GroupsPage } from "@/pages/GroupsPage";
import { LogsPage } from "@/pages/LogsPage";
import { NodesPage, OpenVPNPage, TailscalePage } from "@/pages/NodesPage";
import { OverviewPage } from "@/pages/OverviewPage";
import { ProxyOverviewPage } from "@/pages/ProxyOverviewPage";
import { useDashboardFeed } from "@/hooks/useDashboardFeed";
import { PortsPage } from "@/pages/PortsPage";
import { RulesPage } from "@/pages/RulesPage";
import { SettingsPage } from "@/pages/SettingsPage";
import { ProxySettingsPage } from "@/pages/ProxySettingsPage";
import { SubscriptionsPage } from "@/pages/SubscriptionsPage";
import { requestJSON, requestText } from "@/lib/api";
import { MODE_LABELS } from "@/lib/constants";
import { classNames, proxyEnvCommands, proxyURL } from "@/lib/format";
import { NAV_GROUPS, NAV_ITEMS, isRemoteView, visibleNavigation } from "@/lib/navigation";
import { useNavigation } from "@/hooks/useNavigation";
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

/**
 * mapDesktopPageModule 把 DesktopPage 命名导出适配为 React.lazy 默认导出。
 *
 * 参数说明：module 为动态加载后的桌面页面 ES 模块。
 * 返回值说明：返回 `{default: React.ComponentType}`。
 * 可能的异常/错误情况：构建产物缺少 DesktopPage 时，React 会报告无效组件。
 */
function mapDesktopPageModule(module) {
  return { default: module.DesktopPage };
}

/**
 * loadDesktopPage 按需加载独立远程桌面页面。
 *
 * 参数说明：无。
 * 返回值说明：返回动态 import Promise，首次进入页面时才下载对应代码。
 * 可能的异常/错误情况：静态资源加载失败时由现有 React 错误处理路径呈现。
 */
function loadDesktopPage() {
  return import("@/pages/DesktopPage").then(mapDesktopPageModule);
}

// Remote 与 Desktop 页面分别懒加载；Remote 内更大的 xterm 运行时继续保持第二级懒加载。
const RemotePage = lazy(loadRemotePage);
/** 网关页面按需加载；回调无参数，返回模块 Promise，加载错误由 React 页面边界接管。 */
const GatewayPage = lazy(() => import("@/pages/GatewayPage").then((module) => ({ default: module.GatewayPage })));
/** 系统任务页面按需加载；回调无参数，返回模块 Promise，加载错误由 React 页面边界接管。 */
const ConfigHistoryPage = lazy(() => import("@/pages/ConfigHistoryPage").then((module) => ({ default: module.ConfigHistoryPage })));
const DiagnosticsPage = lazy(() => import("@/pages/DiagnosticsPage").then((module) => ({ default: module.DiagnosticsPage })));
// 只有用户创建会话时才加载全局终端宿主。
const TerminalDialog = lazy(() => import("@/components/TerminalDialog"));
const DesktopPage = lazy(loadDesktopPage);



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
  // 会话归属应用根节点，切换业务页面只改变布局，不释放 WebSocket 与 PTY。
  const [terminalSession, setTerminalSession] = useState(null);
  const [terminalMinimized, setTerminalMinimized] = useState(false);
  /** 打开或恢复当前会话。参数 session 为目标对象；返回无；函数式更新避免旧页面回调替换活动连接。 */
  const openTerminal = useCallback((session) => {
    setTerminalSession((current) => current || session);
    setTerminalMinimized(false);
  }, []);
  const { dismissToast, showToast, toasts } = useToast();
  const [overview, setOverview] = useState(null);
  const [modules, setModules] = useState([]);
  const [snapshotError, setSnapshotError] = useState("");
  const traffic = useTrafficStream(showToast, modules.some((module) => module.id === "proxy" && module.enabled));
  const [activeView, setActiveView] = useNavigation(modules);
  const dashboard = useDashboardFeed(activeView === "overview", modules);
  const navigation = useMemo(() => visibleNavigation(modules), [modules]);
  const [moduleBusy, setModuleBusy] = useState("");
  const [ruleUrls, setRuleUrls] = useState([]);
  const [adblock, setAdblock] = useState(null);
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState("");
  // fastPollUntil 是测速加密轮询的截止时刻（epoch 毫秒）。手动触发后开启，用来覆盖
  // 后端还没置位 testing 的空窗：同步订阅要先下载数秒才进入健康检测。
  const [fastPollUntil, setFastPollUntil] = useState(0);
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
    adblockURL: "",
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
   * applyOverview 把一份概览响应应用到页面状态。
   *
   * 参数说明：nextOverview 为 /api/overview 的响应对象。
   *
   * 返回值说明：返回无。
   *
   * 可能的异常/错误情况：无；首次填充的表单默认值只在为空时写入，不覆盖用户输入。
   */
  const applyOverview = useCallback((nextOverview) => {
    setOverview(nextOverview);
    setForms((current) => ({
      ...current,
      mainPort: current.mainPort || String(nextOverview.mixed_port || ""),
      rangeLo: current.rangeLo || String(nextOverview.port_range?.[0] || ""),
      rangeHi: current.rangeHi || String(nextOverview.port_range?.[1] || ""),
      autoPort: current.autoPort || String(nextOverview.auto_port || 41998),
    }));
  }, []);

  /**
   * loadOverview 只读取并应用代理概览，供测速期间的高频轮询使用。
   *
   * 参数说明：signal 为 AbortSignal，限时取消单次请求。
   *
   * 返回值说明：返回 Promise<void>。
   *
   * 可能的异常/错误情况：后端不可达或 JSON 解析失败时抛出，由调用方决定是否提示；
   * 该函数刻意不触碰 loading，顶栏「正在同步状态」与同步图标不会因此反复闪烁。
   */
  const loadOverview = useCallback(
    async (signal) => {
      applyOverview(await requestJSON("/api/overview", { signal }));
    },
    [applyOverview],
  );

  /**
   * load 独立更新代理概览、模块和规则源，单一来源失败不阻断系统与远程页面。
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
        // 各来源独立提交结果，规则源/广告拦截失败不会阻断模块开关或总览；8 秒超时限制积压。
        const signal = AbortSignal.timeout(8000);
        const results = await Promise.allSettled([
          requestJSON("/api/overview", { signal }),
          requestJSON("/api/rule-urls", { signal }),
          requestJSON("/api/modules", { signal }),
          requestJSON("/api/adblock", { signal }),
        ]);
        const [overviewResult, rulesResult, modulesResult, adblockResult] = results;
        if (modulesResult.status === "fulfilled") setModules(modulesResult.value || []);
        if (rulesResult.status === "fulfilled") setRuleUrls(rulesResult.value || []);
        if (adblockResult.status === "fulfilled") {
          const nextAdblock = adblockResult.value;
          setAdblock(nextAdblock);
          setForms((current) => ({ ...current, adblockURL: current.adblockURL || nextAdblock.rule_url || "" }));
        }
        if (overviewResult.status === "fulfilled") applyOverview(overviewResult.value);
        // 禁用代理时，其明细接口故障不应成为全局总览的待办或阻断远程模块状态。
        const currentModules = modulesResult.status === "fulfilled" ? modulesResult.value || [] : [];
        const proxyVisible = currentModules.some((module) => module.id === "proxy" && module.enabled);
        const unavailable = [proxyVisible && overviewResult.status === "rejected" && "代理概览", modulesResult.status === "rejected" && "模块状态"].filter(Boolean);
        setSnapshotError(unavailable.length ? `${unavailable.join("、")}暂不可用` : "");
        if (!silent && results.some((result) => result.status === "rejected")) showToast("部分状态暂不可用，页面会自动重试", "err");
      } catch (error) {
        if (!silent) showToast(`加载失败：${error.message}`, "err");
      } finally {
        setLoading(false);
      }
    },
    [applyOverview, showToast],
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
  /** useGatewayFeed 接入网关页的状态/预检/设备表管理；参数为激活标记与全局提示回调；返回网关页所需状态与操作。 */
  const gateway = useGatewayFeed(activeView === "gateway", requestConfirmation, showToast);
  /** retryModule 重试已启用模块；参数 module 为状态对象；返回 Promise<void>，失败由 postJSON 提示并刷新状态。 */
  async function retryModule(module) {
    if (moduleBusy) return;
    setModuleBusy(module.id);
    try { await postJSON(`/api/modules/${module.id}/retry`, {}, "模块已重试"); setModules(await requestJSON("/api/modules")); }
    catch (e) { showToast(e.message, "err"); }
    finally { setModuleBusy(""); }
  }

  /**
   * toggleModule 执行模块启停并刷新相关页面。
   * 参数：module 为当前模块快照；返回 Promise<void>；失败由 postJSON 展示，始终释放忙状态。
   * 禁用确认说明连接影响，避免把服务停止误认为单纯隐藏菜单。
   */
  async function toggleModule(module) {
    if (moduleBusy) return;
    if (module.enabled && !(await requestConfirmation({ title: `禁用${module.name}模块？`, description: "将停止此模块的服务并结束活动连接，现有配置会保留。管理控制台仍可访问。", confirmLabel: "禁用模块", destructive: true }))) return;
    setModuleBusy(module.id);
    try {
      if (await postJSON(`/api/modules/${module.id}`, { enabled: !module.enabled }, `${module.name}模块已${module.enabled ? "禁用" : "启用"}`)) {
        if (module.id === "remote") { if (module.enabled) setTerminalSession(null); await remote.reload(); }
      }
    } finally { setModuleBusy(""); }
  }


  /**
   * useDesktopFeed 接入独立远程桌面页的数据与会话生命周期。
   *
   * 功能说明：
   * 只有 desktop 视图可见时才探测本机桌面端口和轮询临时会话；远程连接页不会因此
   * 增加额外请求。服务配置、保存档案与临时隧道操作统一由 Hook 提供。
   *
   * 参数说明：activeView 控制启停；requestConfirmation 处理删除确认；showToast 呈现结果。
   * 返回值说明：返回 DesktopPage 所需的状态和操作集合。
   * 可能的异常/错误情况：接口失败由 Hook 保留旧快照并通过 error/toast 呈现。
   */
  const desktop = useDesktopFeed(activeView, requestConfirmation, showToast);

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
        // 同步订阅要先下载，测速要等后端取锁与调度；宽限期内先加密轮询，等 testing 接力。
        setFastPollUntil(Date.now() + 120000);
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
    () => buildCommands(overview, setActiveView, runCommandAction, triggerOperation, postJSON, navigation.items),
    [overview, triggerOperation, postJSON, setActiveView, navigation.items],
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
      setMobileOpen(false);
      setQuery("");
      await action();
    } catch (error) {
      showToast(`命令失败：${error.message}`, "err");
    }
  }

  useEffect(() => {
    load();
    const timer = window.setInterval(() => load(true), 60000);
    return () => window.clearInterval(timer);
  }, [load]);

  // 测速期间加密轮询概览：延迟是逐节点落定的，60s 的常规周期看不到这个过程。
  // 只读概览、只更新 overview（不动 loading），请求完成后再排下一轮避免请求堆叠。
  const fastPolling = Boolean(busy) || Boolean(overview?.testing) || fastPollUntil > Date.now();
  useEffect(() => {
    if (!fastPolling) return undefined;
    let stopped = false;
    let timer;
    const startedAt = Date.now();

    /** tick 读取一次概览；网络抖动只跳过本轮，不打断正在进行的测速展示。 */
    async function tick() {
      try {
        await loadOverview(AbortSignal.timeout(8000));
      } catch {
        // 概览暂时不可读时静默重试，错误提示由常规轮询负责。
      }
      // 单次激活最多持续 10 分钟：后端长时间不可达时不会把高频请求一直打下去，
      // 超时后回落到 60s 常规轮询，测速结束时下一轮响应也会让本效果自行收尾。
      if (!stopped && Date.now() - startedAt < 600000) timer = window.setTimeout(tick, 1500);
    }

    timer = window.setTimeout(tick, 1500);
    return () => {
      stopped = true;
      window.clearTimeout(timer);
    };
  }, [fastPolling, loadOverview]);

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
      if (event.key === "Escape") { setPaletteOpen(false); setMobileOpen(false); }
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
   * selectOverviewExit 保存规则模式的默认出口（内置 PROXY 组选中项）。
   *
   * 功能说明：
   * 出口取值是节点名、AUTO 或 DIRECT（不是节点 key），与 groupstate 的持久化口径
   * 一致；后端会校验节点仍然可用、AUTO 有可用节点、DIRECT 恒可用。AUTO 需要至少
   * 一个可用节点，节点按名字匹配，避免同名节点的 key 差异导致选中态对不上。
   *
   * 参数说明：
   * - value: string，默认出口值（节点名 / "AUTO" / "DIRECT"）。
   *
   * 返回值说明：
   * 返回 Promise<boolean>；接口成功后概览会通过 postJSON 自动重新加载，失败返回 false。
   *
   * 可能的异常/错误情况：
   * 出口已失效时本地拒绝写入；接口失败或 15 秒超时由 postJSON 提示。
   */
  async function selectOverviewExit(value) {
    if (value === "AUTO") {
      if (!overview.nodes.some((node) => node.alive && !node.tunnel)) {
        showToast("当前没有可用节点，无法选择自动最快", "err");
        return false;
      }
    } else if (value !== "DIRECT" && !overview.nodes.some((node) => node.name === value && node.alive)) {
      showToast("该节点当前不可用，请选择其他出口", "err");
      return false;
    }
    const label = value === "AUTO" ? "自动最快" : value === "DIRECT" ? "直连" : value;
    return postJSON("/api/groups/PROXY/select", { node: value }, `默认出口已切换为「${label}」`, "POST", AbortSignal.timeout(15000));
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
    `${command.label} ${command.group} ${command.searchText || ""}`.toLowerCase().includes(query.toLowerCase()),
  );

  return (
    <TooltipProvider delayDuration={250}>
      <div className={classNames("app-shell", activeView === "overview" && "dashboard-layout")}>
      <nav className="domain-topbar" aria-label="业务大类">
        <b className="domain-brand">proxyd</b>
        {navigation.groups.map((group) => <button key={group.id} type="button" aria-current={NAV_ITEMS.find((item) => item.id === activeView)?.group === group.id ? "true" : undefined} onClick={() => { setActiveView(navigation.items.find((item) => item.group === group.id).id); setMobileOpen(false); }}>{group.label}</button>)}
      </nav>
      {activeView !== "overview" && <Sidebar navItems={navigation.items} modules={modules} moduleBusy={moduleBusy} onToggleModule={toggleModule} activeView={activeView} connected={Boolean(overview)} mobileOpen={mobileOpen} theme={theme} onNavigate={setActiveView} onClose={() => setMobileOpen(false)} onPalette={() => setPaletteOpen(true)} onToggleTheme={() => setTheme((current) => (current === "light" ? "dark" : "light"))} />}
      <main className={classNames("workspace", activeView === "proxy/overview" && "overview-workspace")}>
        {activeView !== "overview" && activeView !== "proxy/overview" && <div className="mobile-only mb-3 items-center gap-2" aria-label="页面导航工具">
          <Button size="sm" variant="outline" type="button" aria-label="打开导航" aria-expanded={mobileOpen} aria-controls="app-sidebar" onClick={() => setMobileOpen(true)}><Menu size={16} aria-hidden="true" />菜单</Button>
          <Button size="sm" variant="outline" type="button" aria-label="搜索页面" onClick={() => setPaletteOpen(true)}><Search size={16} aria-hidden="true" />搜索</Button>
        </div>}
        {!overview && (NAV_ITEMS.find((item) => item.id === activeView)?.group === "proxy") ? (
          <EmptyState title="正在连接 proxyd" detail="等待 /api/overview 返回运行状态。" />
        ) : (
          <div className="view-stage view-enter" key={isRemoteView(activeView) ? "remote" : activeView}>
            {activeView === "overview" && <OverviewPage modules={modules} overview={overview} traffic={traffic} feed={dashboard} snapshotError={snapshotError} theme={theme} onNavigate={setActiveView} onRefresh={() => { load(); dashboard.reload(); }} onPalette={() => setPaletteOpen(true)} onToggleTheme={() => setTheme((current) => current === "light" ? "dark" : "light")} />}
            {activeView === "proxy/overview" && (
              <ProxyOverviewPage
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
                onSelectNode={selectOverviewExit}
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
                onSelectExit={(node) => postJSON("/api/groups/PROXY/select", { node }, `默认出口已切换为「${node}」`)}
                onPost={postJSON}
                onSourceChange={setNodeSourceFilter}
                onTest={() => triggerOperation("/api/test", "测速")}
              />
            )}
            {activeView === "proxy/tailscale" && (
              <TailscalePage
                overview={overview}
                onDelete={deleteJSON}
                onPost={postJSON}
              />
            )}
            {activeView === "proxy/openvpn" && (
              <OpenVPNPage
                overview={overview}
                onDelete={deleteJSON}
                onPost={postJSON}
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
                adblock={adblock}
                onDelete={deleteJSON}
                onForm={updateForm}
                onPost={postJSON}
                onViewContent={submitRuleURLContent}
              />
            )}
            <Suspense fallback={<EmptyState title="正在加载系统管理页面" detail="首次进入时按需加载。" />}>
              {activeView === "diagnostics" && <DiagnosticsPage />}
              {activeView === "config-history" && <ConfigHistoryPage requestConfirmation={requestConfirmation} showToast={showToast} onRestart={restartApp} />}
            </Suspense>
            {activeView === "modules" && <ModulesPage modules={modules} busy={moduleBusy} onToggle={toggleModule} onRetry={retryModule} />}
            {activeView === "logs" && <LogsPage />}
            {activeView === "connections" && <ConnectionsPage {...connections} />}
            {isRemoteView(activeView) && (
              <Suspense fallback={<EmptyState title="正在加载远程连接页面" detail="首次进入时按需加载远程管理与终端入口。" />}>
                <RemotePage view={activeView} {...remote} onOpenTerminal={openTerminal} />
              </Suspense>
            )}
            {activeView === "gateway" && (
              <Suspense fallback={<EmptyState title="正在加载网关页面" detail="首次进入时按需加载网关状态与设备管理入口。" />}>
                <GatewayPage {...gateway} />
              </Suspense>
            )}
            {activeView === "desktop" && (
              <Suspense fallback={<EmptyState title="正在加载远程桌面页面" detail="首次进入时按需加载桌面服务与连接管理入口。" />}>
                <DesktopPage {...desktop} onNavigateRemote={() => setActiveView("remote")} />
              </Suspense>
            )}
            {activeView === "proxy/settings" && <ProxySettingsPage forms={forms} overview={overview} onForm={updateForm} onPost={postJSON} />}
            {activeView === "settings" && <SettingsPage onImportConfig={importConfig} onPost={postJSON} onRestart={restartApp} />}
          </div>
        )}
      </main>
      {terminalSession && <Suspense fallback={null}><TerminalDialog open command={terminalSession.command || ""} target={terminalSession.target || ""} sessionUser={remote.status?.session_user || ""} minimized={terminalMinimized} onMinimizedChange={setTerminalMinimized} onOpenChange={(open) => { if (!open) setTerminalSession(null); }} /></Suspense>}
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
   * triggerSubscriptionAction 执行单个订阅同步或测速。
   *
   * 参数说明：
   * - name: string，订阅名称。
   * - action: string，`refresh` 或 `test`。
   *
   * 返回值说明：
   * 返回 Promise<object|null>；成功返回后端 JSON，失败返回 null。
   *
   * 可能的异常/错误情况：
   * 订阅不存在、后端超时或网络失败时展示错误。
   */
  async function triggerSubscriptionAction(name, action) {
    const encodedName = encodeURIComponent(name);
    const actions = {
      refresh: { method: "POST", path: `/api/subscriptions/${encodedName}/refresh`, label: "同步" },
      test: { method: "POST", path: `/api/subscriptions/${encodedName}/test`, label: "测速" },
    };
    const operation = actions[action];
    if (!operation) {
      showToast(`${name} 操作失败：未知订阅动作 ${action}`, "err");
      return null;
    }
    try {
      setBusy(`${name} ${operation.label}`);
      // 单订阅接口会等待下载、测速和热更新全部完成，期间保持加密轮询才能逐节点看到延迟。
      setFastPollUntil(Date.now() + 180000);
      const result = await requestJSON(operation.path, { method: operation.method });
      // 单订阅接口会等待下载、解析、测速和热更新全部完成，因此这里展示的是完成
      // 通知而非仅“已开始”；同步过程中不增加确认对话框，保持一次点击即可执行。
      showToast(`${name} ${operation.label}完成`);
      await load(true);
      return result || {};
    } catch (error) {
      showToast(`${name} ${operation.label}失败：${error.message}`, "err");
      return null;
    } finally {
      setBusy("");
    }
  }
}

/**
 * buildCommands 生成命令面板命令。
 *
 * 参数说明：
 * - navItems: Array<object>，已过滤禁用模块的导航项；代理禁用时同时隐藏其快捷操作。
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
function buildCommands(overview, setActiveView, runCommandAction, triggerOperation, postJSON, navItems) {
  const navCommands = navItems.map((item) => ({
    group: NAV_GROUPS.find((group) => group.id === item.group)?.label || "概况",
    label: item.label,
    searchText: item.keywords || item.detail || "",
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
  return overview && navItems.some((item) => item.group === "proxy") ? [...navCommands, ...operationCommands, ...modeCommands] : navCommands;
}

/**
 * Sidebar 渲染桌面侧边栏与移动端抽屉导航。
 *
 * 参数说明：
 * - navItems: Array<object>，统一筛选后的菜单，适用于桌面和移动端。
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
function Sidebar({ navItems, modules, moduleBusy, onToggleModule, activeView, connected, mobileOpen, theme, onNavigate, onClose, onPalette, onToggleTheme }) {
  const currentGroup = NAV_ITEMS.find((item) => item.id === activeView)?.group;
  const module = modules.find((item) => item.id === currentGroup);
  const currentItems = navItems.filter((item) => item.group === currentGroup);
  const sectionNames = [...new Set(currentItems.map((item) => item.section || ""))];

  /**
   * renderItem 渲染任务入口，桌面和移动端共用同一注册项。
   * 参数说明：item 为 object，导航元数据；返回值：React 元素。
   * 错误情况：无，跳转后关闭移动端抽屉。
   */
  function renderItem(item) {
    const Icon = item.icon;
    return <button key={item.id} aria-current={activeView === item.id ? "page" : undefined} className={classNames("nav-item", activeView === item.id && "active")} type="button" onClick={() => { onNavigate(item.id); onClose(); }}><Icon size={18} aria-hidden="true" /><span>{item.label}</span></button>;
  }
  return (
    <>
      <aside id="app-sidebar" className={classNames("sidebar", mobileOpen && "open")}>
        <div className="brand">
          <span className="brand-mark"><Shield size={18} /></span>
          <span className="brand-copy"><b>proxyd</b><small>服务控制台</small></span>
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
        <nav className="nav-list" aria-label="当前大类子菜单">
          <div className="nav-section-title">{NAV_GROUPS.find((group) => group.id === currentGroup)?.label}</div>
          {sectionNames.map((section) => (
            <div className="nav-section" key={section || currentGroup}>
              {section && <div className="nav-heading">{section}</div>}
              {currentItems.filter((item) => (item.section || "") === section).map(renderItem)}
            </div>
          ))}
        </nav>
        {module && <div className="module-sidebar-control"><span>{module.name} · {module.enabled ? "已启用" : "已禁用"}</span><Button size="sm" variant="outline" disabled={Boolean(moduleBusy)} onClick={() => onToggleModule(module)}>{moduleBusy === module.id ? "应用中…" : module.enabled ? "禁用模块" : "启用模块"}</Button></div>}
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
