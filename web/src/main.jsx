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
import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import {
  Activity,
  ArrowDown,
  ArrowRight,
  ArrowUp,
  Clock3,
  CheckCircle2,
  ChevronDown,
  CircleAlert,
  CircleHelp,
  Copy,
  Download,
  ExternalLink,
  Globe2,
  HardDrive,
  Gauge,
  Laptop,
  Layers,
  ListFilter,
  Link2,
  Menu,
  Network,
  Pause,
  Pencil,
  Play,
  Plus,
  RefreshCw,
  Search,
  Settings,
  Shield,
  ShieldCheck,
  SlidersHorizontal,
  Sparkles,
  Terminal,
  Target,
  Trash2,
  Upload,
  X,
  Zap,
  Rss,
} from "lucide-react";
import { Badge } from "./components/ui/badge";
import { Button, ButtonLink } from "./components/ui/button";
import { Table } from "./components/ui/data-table";
import {
  ConfirmDialog,
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "./components/ui/dialog";
import { Input } from "./components/ui/input";
import { SegmentedControl } from "./components/ui/segmented-control";
import { Select } from "./components/ui/select";
import { Switch as UISwitch } from "./components/ui/switch";
import { ToastViewport, useToastQueue } from "./components/ui/toast";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "./components/ui/tooltip";
import "./styles.css";

const NAV_ITEMS = [
  { id: "overview", label: "运行概况", shortLabel: "概况", group: "概览", icon: Activity },
  { id: "nodes", label: "代理节点", shortLabel: "节点", group: "代理资源", icon: Network },
  { id: "subscriptions", label: "订阅管理", shortLabel: "订阅", group: "代理资源", icon: Rss },
  { id: "ports", label: "代理入口", shortLabel: "入口", group: "代理入口", icon: Gauge },
  { id: "groups", label: "策略分组", shortLabel: "分组", group: "代理入口", icon: Layers },
  { id: "rules", label: "访问规则", shortLabel: "规则", group: "代理入口", icon: ListFilter },
  { id: "connections", label: "活动连接", shortLabel: "连接", group: "连接与日志", icon: Link2 },
  { id: "logs", label: "运行日志", shortLabel: "日志", group: "连接与日志", icon: Terminal },
  { id: "settings", label: "系统设置", shortLabel: "设置", group: "系统", icon: Settings },
];

const MODE_LABELS = {
  rule: "规则",
  global: "全局",
  direct: "直连",
};

/**
 * MODE_HELP 描述 mihomo 三种运行模式的真实选路语义。
 *
 * 功能说明：
 * 这组说明不仅是界面文案，也是防止误操作的业务边界提示。很多代理软件把
 * “规则模式”误解成整个进程的总开关，但 proxyd 的 mode 只在主入口使用规则分流
 * 策略时参与选路；节点专属端口、自动选优端口和策略分组端口都有独立出口，不读取
 * 这里的模式。把每种模式的执行路径写在界面旁边，可以让用户在切换前理解影响范围。
 *
 * 参数说明：
 * 无；对象 key 与后端 `/api/mode` 接受的 rule/global/direct 枚举保持一致。
 *
 * 返回值说明：
 * 每项包含标题与详细说明，供概览页按当前 mode 展示。
 *
 * 可能的异常/错误情况：
 * 如果后端未来新增 mode 而未同步本对象，界面会使用保守兜底文案，不会阻断切换。
 */
const MODE_HELP = {
  rule: {
    title: "按规则决定出口",
    detail: "依次匹配自定义规则、远程规则源和内置规则；首条命中立即生效，后续规则不再继续判断。",
  },
  global: {
    title: "全部交给代理组",
    detail: "主入口流量跳过访问规则，统一进入 PROXY 选择组，再由当前代理组节点负责转发。",
  },
  direct: {
    title: "全部直接连接",
    detail: "主入口流量跳过访问规则与代理节点，直接访问目标；适合临时排查代理链路问题。",
  },
};

const GROUP_TYPE_LABELS = {
  "fallback": "故障转移",
  "url-test": "自动测速",
  "load-balance": "负载均衡",
};

/**
 * classNames 组合条件 CSS class。
 *
 * 参数说明：
 * - items: Array<string | false | null | undefined>，每一项是待组合的 class。
 *
 * 返回值说明：
 * 返回过滤空值后的 class 字符串。
 *
 * 可能的异常/错误情况：
 * 无；空值会被忽略，非空值按字符串拼接。
 */
function classNames(...items) {
  return items.filter(Boolean).join(" ");
}

/**
 * requestJSON 调用 proxyd JSON API 并统一处理错误。
 *
 * 参数说明：
 * - url: string，API 路径。
 * - options: RequestInit，可选 fetch 参数。
 *
 * 返回值说明：
 * 返回解析后的 JSON；204 或空响应返回 null。
 *
 * 可能的异常/错误情况：
 * 网络失败、HTTP 非 2xx、JSON 格式错误都会抛出 Error，调用方负责 toast。
 */
async function requestJSON(url, options = {}) {
  const response = await fetch(url, {
    ...options,
    headers: {
      "Content-Type": "application/json",
      ...(options.headers || {}),
    },
  });
  const text = await response.text();
  if (!response.ok) {
    throw new Error(text.trim() || `HTTP ${response.status}`);
  }
  return text ? JSON.parse(text) : null;
}

/**
 * requestText 调用返回文本的 API。
 *
 * 参数说明：
 * - url: string，API 路径。
 * - options: RequestInit，可选 fetch 参数。
 *
 * 返回值说明：
 * 返回响应文本。
 *
 * 可能的异常/错误情况：
 * 网络失败或 HTTP 非 2xx 会抛出 Error。
 */
async function requestText(url, options = {}) {
  const response = await fetch(url, options);
  const text = await response.text();
  if (!response.ok) {
    throw new Error(text.trim() || `HTTP ${response.status}`);
  }
  return text;
}

/**
 * formatDelay 把节点延迟格式化为 UI 文案。
 *
 * 参数说明：
 * - item: {alive?: boolean, delay?: number}，节点或端口记录。
 *
 * 返回值说明：
 * 可用节点返回 `${delay}ms`，失效或未测速返回 `-`。
 *
 * 可能的异常/错误情况：
 * 无；缺失字段按不可用处理。
 */
function formatDelay(item) {
  return item.alive && item.delay > 0 ? `${item.delay}ms` : "-";
}

/**
 * formatBytes 把字节数格式化为二进制容量文本。
 *
 * 参数说明：
 * - value: number，字节数。
 *
 * 返回值说明：
 * 返回 B/KiB/MiB/GiB/TiB 文本。
 *
 * 可能的异常/错误情况：
 * 非数字或负数按 0B 处理，避免后端或订阅源异常字段撑坏 UI。
 */
function formatBytes(value) {
  let n = Number(value);
  if (!Number.isFinite(n) || n < 0) n = 0;
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let index = 0;
  while (n >= 1024 && index < units.length - 1) {
    n /= 1024;
    index += 1;
  }
  return index === 0 ? `${Math.round(n)}B` : `${n.toFixed(1)}${units[index]}`;
}

/**
 * formatExpire 把订阅 Unix 秒到期时间转成日期与风险等级。
 *
 * 参数说明：
 * - expire: number，Unix 秒时间戳；0 表示未知。
 *
 * 返回值说明：
 * 返回 `{ text, urgent }`，urgent 表示 7 天内到期或已过期。
 *
 * 可能的异常/错误情况：
 * 非法时间戳返回未知文本，不抛出异常。
 */
function formatExpire(expire) {
  const ts = Number(expire);
  if (!Number.isFinite(ts) || ts <= 0) return { text: "到期未知", urgent: false };
  const date = new Date(ts * 1000);
  if (Number.isNaN(date.getTime())) return { text: "到期未知", urgent: false };
  const days = Math.ceil((date.getTime() - Date.now()) / 86400000);
  return {
    text: `${date.toLocaleDateString("zh-CN")} 到期`,
    urgent: days <= 7,
  };
}

/**
 * formatUserInfo 生成订阅用量展示模型。
 *
 * 参数说明：
 * - info: object | null，overview.subscriptions[].userinfo。
 *
 * 返回值说明：
 * 返回 `{ usage, expire, urgent }`；没有数据时返回 null。
 *
 * 可能的异常/错误情况：
 * 缺字段或字段非法时用保守文本兜底，不影响节点列表渲染。
 */
function formatUserInfo(info) {
  if (!info) return null;
  const used = Number(info.upload || 0) + Number(info.download || 0);
  const usage = info.total > 0 ? `${formatBytes(used)} / ${formatBytes(info.total)}` : formatBytes(used);
  const expire = formatExpire(info.expire);
  return { usage, expire: expire.text, urgent: expire.urgent };
}

/**
 * delayClass 根据延迟选择展示颜色。
 *
 * 参数说明：
 * - item: {alive?: boolean, delay?: number}，节点或端口记录。
 *
 * 返回值说明：
 * 返回用于 CSS 着色的 class 名称。
 *
 * 可能的异常/错误情况：
 * 无；不可用节点返回 `delay-muted`。
 */
function delayClass(item) {
  if (!item.alive || !item.delay) return "delay-muted";
  if (item.delay < 300) return "delay-fast";
  if (item.delay < 800) return "delay-mid";
  return "delay-slow";
}

/**
 * sortByDelay 按延迟排序节点或端口列表。
 *
 * 参数说明：
 * - list: Array<object>，含 alive/delay/port 字段的列表。
 * - mode: string，`asc`、`desc` 或其他值；其他值保持原顺序。
 *
 * 返回值说明：
 * 返回新的排序数组；不会修改原数组。
 *
 * 可能的异常/错误情况：
 * 无；失效或未测速项排在最后，同延迟按端口排序。
 */
function sortByDelay(list, mode) {
  if (mode !== "asc" && mode !== "desc" && mode !== "delay") return list;
  const direction = mode === "desc" ? -1 : 1;
  return [...list].sort((a, b) => {
    const da = a.alive && a.delay > 0 ? a.delay : Number.POSITIVE_INFINITY;
    const db = b.alive && b.delay > 0 ? b.delay : Number.POSITIVE_INFINITY;
    return (da - db) * direction || (a.port || 999999) - (b.port || 999999);
  });
}

/**
 * tableViewportHeight 计算 Radix ScrollArea 数据表格的可视高度。
 *
 * 功能说明：
 * 官方 Table 需要明确 viewport 高度才能虚拟滚动。当前控制台多数列表较短，
 * 因此高度应随行数收缩，避免出现大块空白；数据较多时再封顶并启用滚动。
 *
 * 参数说明：
 * - rowCount: number，当前数据行数。
 * - maximum: number，可视区域最大高度，单位像素。
 *
 * 返回值说明：
 * 返回包含表头在内的 viewport 像素高度。
 *
 * 可能的异常/错误情况：
 * 非法或负行数按 0 处理；maximum 非法时使用 480px。
 */
function tableViewportHeight(rowCount, maximum = 480) {
  const safeCount = Number.isFinite(rowCount) && rowCount > 0 ? Math.floor(rowCount) : 0;
  const safeMaximum = Number.isFinite(maximum) && maximum >= 144 ? maximum : 480;
  return Math.min(safeMaximum, Math.max(144, (safeCount + 1) * 48));
}

/**
 * proxyURL 生成本机代理地址文本。
 *
 * 参数说明：
 * - listen: string，proxyd 监听 host。
 * - port: number，代理端口。
 *
 * 返回值说明：
 * 返回 `http://host:port`。
 *
 * 可能的异常/错误情况：
 * 无；listen 为空时使用 127.0.0.1。
 */
function proxyURL(listen, port) {
  return `http://${listen || "127.0.0.1"}:${port}`;
}

/**
 * pickFiniteNumber 从候选值中挑出第一个可用数字。
 *
 * 功能说明：
 * `/connections` 的响应字段在不同 mihomo 版本里可能出现命名差异，
 * 前端不能把单一字段名当成硬契约，因此这里集中做兼容性归一化。
 *
 * 参数说明：
 * - values: Array<unknown>，按优先级排列的候选值。
 *
 * 返回值说明：
 * 返回第一个有限数字；如果没有合法数字则返回 `null`。
 *
 * 可能的异常/错误情况：
 * 无；空字符串、`null`、`undefined` 和非法数字都会被跳过。
 */
function pickFiniteNumber(...values) {
  for (const value of values) {
    if (value === null || value === undefined) continue;
    if (typeof value === "string" && !value.trim()) continue;
    const numericValue = Number(value);
    if (Number.isFinite(numericValue)) {
      return numericValue;
    }
  }
  return null;
}

/**
 * normalizeText 把任意值收敛成可安全展示和搜索的字符串。
 *
 * 功能说明：
 * 连接记录里常见的 `host`、`process`、`chain` 等字段都可能缺失、为空或类型不一，
 * 这里统一转成字符串，避免后续拼接和搜索出现 `undefined` 或对象串化噪音。
 *
 * 参数说明：
 * - value: unknown，待转换值。
 *
 * 返回值说明：
 * 返回去除首尾空白后的字符串；无法转换时返回空字符串。
 *
 * 可能的异常/错误情况：
 * 无。
 */
function normalizeText(value) {
  if (value === null || value === undefined) return "";
  return String(value).trim();
}

/**
 * extractConnectionList 从 `/api/connections` 响应里提取连接数组。
 *
 * 功能说明：
 * 不同实现可能直接返回数组，也可能返回 `{ connections: [...] }`、
 * `{ data: { connections: [...] } }` 之类的包裹结构。这里优先递归查找，
 * 这样前端就不会因为响应包一层而整页失效。
 *
 * 参数说明：
 * - payload: unknown，`/api/connections` 的原始 JSON。
 *
 * 返回值说明：
 * 返回连接数组；找不到时返回空数组。
 *
 * 可能的异常/错误情况：
 * 无；任何非对象或非法结构都会降级为空数组。
 */
function extractConnectionList(payload) {
  if (Array.isArray(payload)) {
    return payload;
  }
  if (!payload || typeof payload !== "object") {
    return [];
  }

  const candidates = [
    payload.connections,
    payload.items,
    payload.entries,
    payload.list,
    payload.records,
    payload.data,
  ];

  for (const candidate of candidates) {
    if (Array.isArray(candidate)) {
      return candidate;
    }
    if (candidate && typeof candidate === "object") {
      const nested = extractConnectionList(candidate);
      if (nested.length > 0) {
        return nested;
      }
    }
  }

  return [];
}

/**
 * normalizeConnectionChains 把链路字段规整成字符串数组。
 *
 * 功能说明：
 * 连接链可能来自字符串数组，也可能是对象数组。为了让搜索和展示都稳定，
 * 这里统一提取节点名或代理名，并过滤掉空值。
 *
 * 参数说明：
 * - value: unknown，原始链路字段。
 *
 * 返回值说明：
 * 返回可展示的链路节点名数组。
 *
 * 可能的异常/错误情况：
 * 无；非法结构会被忽略。
 */
function normalizeConnectionChains(value) {
  if (typeof value === "string") {
    const text = normalizeText(value);
    return text ? [text] : [];
  }
  if (!Array.isArray(value)) {
    return [];
  }
  return value
    .map((item) => {
      if (typeof item === "string") return normalizeText(item);
      if (item && typeof item === "object") {
        return normalizeText(
          item.name || item.node || item.proxy || item.value || item.title || item.id || item.address,
        );
      }
      return "";
    })
    .filter(Boolean);
}

/**
 * normalizeConnectionDate 把连接时间规整成 Date 对象。
 *
 * 功能说明：
 * `/connections` 里的时间字段可能是秒级时间戳、毫秒级时间戳或 ISO 字符串。
 * 前端需要一个统一入口把它变成 `Date`，这样桌面表格和移动卡片都能用同一套渲染逻辑。
 *
 * 参数说明：
 * - value: unknown，原始开始时间。
 *
 * 返回值说明：
 * 返回 `Date`；无法解析时返回 `null`。
 *
 * 可能的异常/错误情况：
 * 无；非法时间值不会抛错。
 */
function normalizeConnectionDate(value) {
  if (value === null || value === undefined || value === "") {
    return null;
  }

  if (value instanceof Date) {
    return Number.isNaN(value.getTime()) ? null : value;
  }

  const numericValue = Number(value);
  if (Number.isFinite(numericValue)) {
    const timestamp = numericValue < 1e12 ? numericValue * 1000 : numericValue;
    const date = new Date(timestamp);
    return Number.isNaN(date.getTime()) ? null : date;
  }

  const date = new Date(String(value));
  return Number.isNaN(date.getTime()) ? null : date;
}

/**
 * normalizeMemoryMetric 把内存字段规整成可展示的数值或文本。
 *
 * 功能说明：
 * 连接面板需要展示摘要内存，但后端字段可能是数字、字符串或对象。
 * 这里优先取字节数，拿不到时保留原始字符串，避免前端硬编码某个版本的字段名。
 *
 * 参数说明：
 * - value: unknown，原始内存字段。
 *
 * 返回值说明：
 * 返回数字、字符串或 `null`。
 *
 * 可能的异常/错误情况：
 * 无。
 */
function normalizeMemoryMetric(value) {
  if (value === null || value === undefined || value === "") {
    return null;
  }
  if (typeof value === "number" && Number.isFinite(value)) {
    return value;
  }
  if (typeof value === "object") {
    const numericValue = pickFiniteNumber(
      value.used,
      value.rss,
      value.resident,
      value.bytes,
      value.value,
      value.size,
    );
    if (numericValue !== null) {
      return numericValue;
    }
    const text = normalizeText(value.text || value.label || value.display || value.name);
    return text || null;
  }
  const numericValue = Number(value);
  if (Number.isFinite(numericValue)) {
    return numericValue;
  }
  const text = normalizeText(value);
  return text || null;
}

/**
 * buildConnectionViewModel 把原始连接记录转换成页面可渲染模型。
 *
 * 功能说明：
 * 这个页面的核心风险不是渲染，而是字段不稳定。这里把可能分散在 metadata、
 * 顶层字段和不同命名风格里的信息统一收口，后续过滤、排序、摘要和卡片展示都只
 * 处理标准化后的模型，避免 UI 里散落一堆防御性判断。
 *
 * 参数说明：
 * - raw: unknown，单条连接记录。
 * - index: number，当前记录位置，用于缺失 id 时生成稳定兜底键。
 *
 * 返回值说明：
 * 返回标准化后的连接模型。
 *
 * 可能的异常/错误情况：
 * 无；无法识别的字段会降级为 `—` 或空字符串。
 */
function buildConnectionViewModel(raw, index) {
  const connection = raw && typeof raw === "object" ? raw : { id: raw };
  const metadata = connection.metadata && typeof connection.metadata === "object" ? connection.metadata : {};
  const protocolKey = normalizeText(connection.network || connection.type || connection.protocol || metadata.network || metadata.type).toLowerCase();
  const protocolLabel = protocolKey ? protocolKey.toUpperCase() : "未知";
  const host = normalizeText(
    metadata.host ||
      connection.host ||
      metadata.domain ||
      connection.domain ||
      metadata.hostname ||
      connection.hostname ||
      connection.target ||
      connection.destination ||
      connection.remoteHost,
  );
  const address = normalizeText(
    metadata.destinationIP ||
      metadata.dstIP ||
      connection.destinationIP ||
      connection.dstIP ||
      metadata.address ||
      connection.address ||
      metadata.ip ||
      connection.ip ||
      metadata.remoteIP ||
      connection.remoteIP ||
      connection.sourceIP,
  );
  const port = pickFiniteNumber(
    connection.port,
    metadata.port,
    connection.dstPort,
    metadata.dstPort,
    connection.remotePort,
    metadata.remotePort,
  );
  const targetLabel = host || address || normalizeText(connection.target) || "—";
  const targetDetail = [host && address && host !== address ? address : "", Number.isFinite(port) ? `端口 ${port}` : ""]
    .filter(Boolean)
    .join(" · ");
  const entryPort = pickFiniteNumber(
    connection.inPort,
    connection.inboundPort,
    connection.listenPort,
    connection.localPort,
    metadata.inPort,
    metadata.inboundPort,
    metadata.listenPort,
    metadata.localPort,
  );
  const entryName = normalizeText(
    connection.inbound ||
      connection.inboundName ||
      connection.listener ||
      connection.listen ||
      metadata.inbound ||
      metadata.inboundName ||
      metadata.listener ||
      metadata.listen,
  );
  // 普通用户排查连接时，入口名称和实际端口缺一不可。只显示名称会让同类型
  // listener 难以区分；只显示端口又会丢掉 TUN/mixed 等入口语义。
  const entryLabel = [entryName, Number.isFinite(entryPort) ? `端口 ${entryPort}` : ""]
    .filter(Boolean)
    .join(" · ") || "—";
  const processName = normalizeText(
    connection.process ||
      connection.processName ||
      connection.processPath ||
      metadata.process ||
      metadata.processName ||
      metadata.processPath,
  );
  const sourceAddress = normalizeText(
    connection.sourceIP ||
      metadata.sourceIP ||
      connection.clientIP ||
      metadata.clientIP ||
      connection.remoteIP ||
      metadata.remoteIP,
  );
  const sourceLabel = [processName, sourceAddress].filter(Boolean).join(" · ") || processName || sourceAddress || "—";
  const chains = normalizeConnectionChains(connection.chains || metadata.chains || connection.chain || metadata.chain || connection.route || metadata.route);
  const exitLabel = chains.length > 0
    ? chains.join(" → ")
    : normalizeText(connection.proxy || connection.rule || connection.rulePayload || connection.name || metadata.proxy || metadata.rule || metadata.name) || "—";
  const uploadBytes = pickFiniteNumber(
    connection.upload,
    connection.uploadBytes,
    connection.up,
    metadata.upload,
    metadata.uploadBytes,
    metadata.up,
  ) ?? 0;
  const downloadBytes = pickFiniteNumber(
    connection.download,
    connection.downloadBytes,
    connection.down,
    metadata.download,
    metadata.downloadBytes,
    metadata.down,
  ) ?? 0;
  const totalBytes = uploadBytes + downloadBytes;
  const startedAt = normalizeConnectionDate(
    connection.start ||
      connection.startedAt ||
      connection.startTime ||
      connection.creation ||
      connection.createdAt ||
      metadata.start ||
      metadata.startedAt ||
      metadata.startTime,
  );
  const startedDateTime = startedAt ? startedAt.toISOString() : "";
  const startedLabel = startedAt ? startedAt.toLocaleString("zh-CN", { hour12: false }) : "—";
  const closeId = normalizeText(connection.id || connection.key || connection.uuid || connection.uid);
  const renderKey = closeId || `${targetLabel}-${index}`;
  const searchText = [
    renderKey,
    protocolLabel,
    protocolKey,
    targetLabel,
    targetDetail,
    entryLabel,
    sourceLabel,
    exitLabel,
    host,
    address,
    processName,
    sourceAddress,
    normalizeText(connection.rule),
    normalizeText(connection.rulePayload),
  ]
    .filter(Boolean)
    .join(" ")
    .toLowerCase();

  return {
    id: renderKey,
    closeId,
    protocolKey,
    protocolLabel,
    targetLabel,
    targetDetail,
    entryLabel,
    sourceLabel,
    exitLabel,
    uploadBytes,
    downloadBytes,
    totalBytes,
    startedAt,
    startedDateTime,
    startedLabel,
    searchText,
    closeable: Boolean(closeId),
  };
}

/**
 * normalizeConnectionsResponse 把 `/api/connections` 响应规整成页面模型。
 *
 * 功能说明：
 * 该接口既可能直接返回数组，也可能返回包含 `connections` 的对象，还可能带
 * 内存或流量汇总字段。这里一次性完成连接列表、摘要指标和字段兜底，避免页面层
 * 再写重复的兼容逻辑。
 *
 * 参数说明：
 * - payload: unknown，`/api/connections` 的原始 JSON。
 *
 * 返回值说明：
 * 返回 `{ items, summary }`，其中 `summary` 包含活动数、累计上/下行与内存。
 *
 * 可能的异常/错误情况：
 * 无；所有非法字段都会被忽略。
 */
function normalizeConnectionsResponse(payload) {
  const root = payload && typeof payload === "object" ? payload : {};
  const rawItems = extractConnectionList(root);
  const items = rawItems.map((item, index) => buildConnectionViewModel(item, index));
  const summarySource = root.summary && typeof root.summary === "object" ? root.summary : root;
  const totalUploadBytes = pickFiniteNumber(
    summarySource.upTotal,
    summarySource.totalUp,
    summarySource.uploadTotal,
    summarySource.upload,
    root.upTotal,
    root.totalUp,
    root.uploadTotal,
  );
  const totalDownloadBytes = pickFiniteNumber(
    summarySource.downTotal,
    summarySource.totalDown,
    summarySource.downloadTotal,
    summarySource.download,
    root.downTotal,
    root.totalDown,
    root.downloadTotal,
  );
  const memory = normalizeMemoryMetric(
    summarySource.memory ??
      summarySource.mem ??
      summarySource.memoryUsage ??
      summarySource.rss ??
      root.memory ??
      root.mem ??
      root.memoryUsage ??
      root.rss,
  );
  const summedUploadBytes = items.reduce((total, item) => total + (item.uploadBytes || 0), 0);
  const summedDownloadBytes = items.reduce((total, item) => total + (item.downloadBytes || 0), 0);

  return {
    items,
    summary: {
      activeCount: items.length,
      uploadBytes: totalUploadBytes ?? summedUploadBytes,
      downloadBytes: totalDownloadBytes ?? summedDownloadBytes,
      memory,
    },
  };
}

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
function useTrafficStream(showToast) {
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

/**
 * useConnectionsFeed 管理活动连接页的轮询、筛选和关闭操作。
 *
 * 功能说明：
 * 这个 hook 只在 `activeView === "connections"` 时工作，并且只轮询 `/api/connections`
 * 一个接口。这样可以确保页面切换时及时停表，避免连接页把全局状态拖慢，也避免
 * 连接操作和其他页面轮询互相覆盖。
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
function useConnectionsFeed(activeView, requestConfirmation, showToast) {
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
  const [paused, setPaused] = useState(false);
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

    loadConnections();
    // 暂停只停止自动轮询，不禁用手动刷新和关闭动作，便于用户冻结列表后检查细节。
    const timer = paused ? 0 : window.setInterval(loadConnections, 2000);
    return () => {
      if (timer) window.clearInterval(timer);
      requestControllerRef.current?.abort();
    };
  }, [activeView, loadConnections, paused]);

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
    paused,
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
    setPaused,
    setTransport,
    closeAllConnections,
    closeConnection,
  };
}

/**
 * useToast 管理短暂展示的 toast 消息。
 *
 * 参数说明：
 * 无。
 *
 * 返回值说明：
 * 返回 toast 队列、展示方法和关闭方法，供页面统一挂载通知栈。
 *
 * 可能的异常/错误情况：
 * 无；重复调用会按时间倒序入队，并受队列长度上限约束。
 */
function useToast() {
  const { toasts, showToast: showRadixToast, dismissToast } = useToastQueue({
    defaultDuration: 3600,
    limit: 4,
  });
  const showToast = useCallback((message, type = "ok") => {
    showRadixToast({
      status: type === "err" ? "error" : "success",
      title: type === "err" ? "操作未完成" : "操作完成",
      description: message,
    });
  }, [showRadixToast]);
  return { dismissToast, showToast, toasts };
}

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
   *
   * 返回值说明：
   * 成功返回 true，失败返回 false。
   *
   * 可能的异常/错误情况：
   * 后端校验失败、网络失败或 JSON 解析失败时 toast 错误并返回 false。
   */
  const postJSON = useCallback(
    async (url, body, message, method = "POST") => {
      try {
        await requestJSON(url, { method, body: JSON.stringify(body) });
        if (message) showToast(message);
        await load(true);
        return true;
      } catch (error) {
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
   * useConnectionsFeed 接入活动连接页的轮询与关闭能力。
   *
   * 功能说明：
   * 这个 hook 只在活动连接页可见时工作，避免后台页签继续打 `/api/connections`。
   * 它把页面所需的列表、摘要、筛选、单条关闭和关闭全部操作集中起来，App 层只
   * 需要挂一次数据源即可。
   *
   * 参数说明：
   * - activeView: string，当前激活视图名称，用于控制是否轮询。
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
      <Sidebar activeView={activeView} connected={Boolean(overview)} mobileOpen={mobileOpen} onNavigate={setActiveView} onClose={() => setMobileOpen(false)} onPalette={() => setPaletteOpen(true)} />
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
            {activeView === "settings" && (
              <SettingsPage
                forms={forms}
                overview={overview}
                onForm={updateForm}
                onImportConfig={importConfig}
                onPost={postJSON}
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
 * - onNavigate: Function，切换页面回调。
 * - onClose: Function，关闭移动端抽屉回调。
 * - onPalette: Function，打开全局命令菜单。
 *
 * 返回值说明：
 * 返回导航 React 元素。
 *
 * 可能的异常/错误情况：
 * 无。
 */
function Sidebar({ activeView, connected, mobileOpen, onNavigate, onClose, onPalette }) {
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
        <Button disabled={Boolean(busy)} variant="outline" type="button" onClick={onTest}>
          <Gauge size={16} aria-hidden="true" />
          <span className="desktop-action-label">测试节点</span>
          <span className="mobile-action-label">测速</span>
        </Button>
        <Button disabled={Boolean(busy)} type="button" onClick={onRefresh}>
          <RefreshCw className={classNames((busy || loading) && "animate-spin")} size={16} aria-hidden="true" />
          <span className="desktop-action-label">同步订阅</span>
          <span className="mobile-action-label">同步</span>
        </Button>
      </div>
    </header>
  );
}

/**
 * OverviewPage 渲染概览页。
 *
 * 参数说明：
 * - overview: object，/api/overview 响应。
 * - aliveCount: number，可用节点数量。
 * - busy/loading: string | boolean，全局后台操作与同步状态。
 * - traffic: object，实时速率流状态。
 * - onCopy/onMenu/onMode/onNavigate/onPalette/onPolicy/onPortMapping/onRefresh/onSystemProxy/onTest/onTun: Function，用户操作回调。
 *
 * 返回值说明：
 * 返回概览页 React 元素。
 *
 * 可能的异常/错误情况：
 * 无；操作失败由父组件 toast。
 */
function OverviewPage({
  overview,
  aliveCount,
  busy,
  loading,
  traffic,
  onCopy,
  onMenu,
  onMode,
  onNavigate,
  onPalette,
  onPolicy,
  onPortMapping,
  onRefresh,
  onSystemProxy,
  onTest,
  onTun,
}) {
  const updated = overview.server_time ? new Date(overview.server_time) : new Date();
  const ready = aliveCount > 0 && overview.mixed_port > 0;
  const takenOver = Boolean(overview.system_proxy || overview.tun?.active || overview.tun?.enabled);
  const policy = resolveMainPolicy(overview);
  const activeNode = resolveActiveNode(overview, policy);
  const attentionItems = buildOverviewAttention(overview, traffic, aliveCount);
  /*
   * 这里必须同时区分“配置意图”和“运行时有效策略”：
   * 1. main_auto 优先级最高，它开启时 mode 与 main_node 都不参与主端口选路；
   * 2. main_auto 关闭且 main_node 存在时，用户意图是固定节点；
   * 3. 固定节点暂时失效时，后端为了保持主入口可用会临时恢复顶层 mixed-port，
   *    并继续遵循 mode；它不会删除 main_node，节点恢复后仍可回到原配置；
   * 4. 只有规则策略或上述运行时回退发生时，rule/global/direct 才决定主端口路径。
   *
   * 因此界面保留“固定节点”选中态表达持久化意图，同时在路由和出口处明确标记
   * “已回退”，避免把失效节点误报成真实出口。节点专属端口、自动选优端口与分组
   * 端口拥有独立 listener，不经过此判断，也不会被 mode 切换影响。
   */
  const fallbackToMainMode = policy === "fixed" && !activeNode;
  const usesConfiguredMode = policy === "rule" || fallbackToMainMode;
  const modeHelp = MODE_HELP[overview.mode] || {
    title: "使用后端配置模式",
    detail: `当前后端返回未识别模式“${overview.mode || "未知"}”，界面不会推测其选路行为。`,
  };
  const modeInactive = !usesConfiguredMode;
  const policyLabel = policy === "auto"
    ? "自动最快"
    : policy === "fixed"
      ? fallbackToMainMode ? `固定节点（回退至${MODE_LABELS[overview.mode] || overview.mode}）` : "固定节点"
      : MODE_LABELS[overview.mode] === "规则"
        ? "规则分流"
        : `${MODE_LABELS[overview.mode] || overview.mode}模式`;
  const modeExitLabel = overview.mode === "direct"
    ? "直接连接"
    : overview.mode === "global"
      ? "PROXY 选择组"
      : "按规则动态选择";
  const exitLabel = activeNode?.name || (usesConfiguredMode ? modeExitLabel : "暂无可用节点");

  return (
    <section className="overview-shell">
      <aside className="policy-pane" aria-labelledby="policy-pane-title">
        <header className="policy-pane-header">
          <div><span>主代理入口</span><h1 id="policy-pane-title">主入口策略</h1></div>
          <Button aria-label="打开系统设置" size="icon" variant="outline" type="button" onClick={() => onNavigate("settings")}><Settings size={17} aria-hidden="true" /></Button>
        </header>
        <div className="policy-options" role="list" aria-label="主入口策略选项">
          <PolicyOption active={policy === "rule"} detail={`当前为${MODE_LABELS[overview.mode] || overview.mode}模式`} icon={ListFilter} label="规则分流" tone="blue" onClick={() => onPolicy("rule")} />
          <PolicyOption active={policy === "auto"} detail="自动选择延迟最低节点" icon={Sparkles} label="自动最快" tone="teal" onClick={() => onPolicy("auto")} />
          <PolicyOption active={policy === "fixed"} detail={overview.main_node ? (activeNode?.name || "配置节点当前不可用") : "前往设置选择固定节点"} icon={Target} label="固定节点" tone="indigo" onClick={() => onPolicy("fixed")} />
        </div>
        <section className="policy-mode">
          <PanelTitle title="规则模式" detail="仅在规则分流策略下生效" />
          <SegmentedControl ariaLabel="流量处理模式" className="policy-mode-control" onValueChange={onMode} options={Object.entries(MODE_LABELS).map(([value, label]) => ({ value, label }))} value={overview.mode} />
          <div className={classNames("policy-mode-note", modeInactive && "inactive")}>
            <CircleHelp size={16} aria-hidden="true" />
            <div>
              <strong>{modeHelp.title}</strong>
              <p>{modeHelp.detail}</p>
              <small>
                {modeInactive
                  ? `当前主入口使用“${policy === "auto" ? "自动最快" : "固定节点"}”，该模式已保存但暂不参与主端口选路。`
                  : "仅影响主代理入口；节点端口、自动选优端口和策略分组端口不受影响。"}
              </small>
            </div>
          </div>
        </section>
        <footer className="policy-pane-footer">
          <span>主入口</span>
          <button className="copy-link" type="button" onClick={() => onCopy(overview.mixed_port)}>127.0.0.1:{overview.mixed_port}<Copy size={14} aria-hidden="true" /></button>
          <small>{aliveCount}/{overview.nodes.length} 个节点可用</small>
        </footer>
      </aside>

      <div className="overview-detail">
        <header className="overview-hero">
          <div className={classNames("hero-status-icon", ready ? "ready" : "attention")}>
            {ready ? <ShieldCheck size={34} aria-hidden="true" /> : <CircleAlert size={34} aria-hidden="true" />}
          </div>
          <div className="overview-hero-copy">
            <span className="overview-mobile-heading"><Button className="mobile-only" size="icon" variant="outline" type="button" onClick={onMenu} aria-label="打开导航"><Menu size={18} aria-hidden="true" /></Button>运行概况</span>
            <h2>{ready ? (takenOver ? "代理已接管" : "代理入口已就绪") : "代理服务需要检查"}</h2>
            <p>{ready ? `当前主入口策略：${policyLabel}` : "当前没有健康节点，请同步订阅或检查节点配置"}</p>
            <div className="hero-badges">
              <Badge variant={ready ? "success" : "destructive"}>{ready ? "当前生效" : "需要处理"}</Badge>
              {overview.tun?.enabled && <Badge variant="outline">TUN 已开启</Badge>}
              {overview.system_proxy && <Badge variant="outline">系统代理已开启</Badge>}
            </div>
          </div>
          <div className="overview-hero-actions">
            <Button className="command-button" size="icon" variant="outline" type="button" onClick={onPalette} aria-label="打开命令菜单"><Search size={16} aria-hidden="true" /></Button>
            <Button disabled={Boolean(busy)} variant="outline" type="button" onClick={onTest}><Gauge size={16} aria-hidden="true" />测速</Button>
            <Button disabled={Boolean(busy)} type="button" onClick={onRefresh}><RefreshCw className={classNames((busy || loading) && "animate-spin")} size={16} aria-hidden="true" />同步</Button>
          </div>
        </header>

        <VersionNotice status={overview.version_check} />

        <section className="effective-route" aria-labelledby="effective-route-title">
          <div className="section-heading-row">
            <div><span>当前生效</span><h2 id="effective-route-title">路由概览</h2></div>
            <small>最后更新 {updated.toLocaleTimeString("zh-CN", { hour12: false })}</small>
          </div>
          <div className="route-flow">
            <RouteStep detail={takenOver ? (overview.tun?.enabled ? "TUN 接管" : "系统代理") : "手动配置入口"} icon={Laptop} label="本机应用" tone="blue" />
            <ArrowRight className="route-arrow" size={20} aria-hidden="true" />
            <RouteStep detail={`127.0.0.1:${overview.mixed_port}`} icon={Shield} label="主代理入口" tone="blue" />
            <ArrowRight className="route-arrow" size={20} aria-hidden="true" />
            <RouteStep detail={policyLabel} icon={ListFilter} label="匹配策略" tone="indigo" />
            <ArrowRight className="route-arrow" size={20} aria-hidden="true" />
            <RouteStep detail={exitLabel} icon={Globe2} label="实际出口" tone="teal" />
          </div>
          <p className="route-summary">当前流量经由主入口进入 {policyLabel}；{activeNode ? `可确认的出口为“${activeNode.name}”。` : "具体出口会根据命中规则和目标地址动态变化。"}</p>
        </section>

        <div className="overview-lower-grid">
          <section className="exit-summary" aria-labelledby="exit-summary-title">
            <div className="section-heading-row compact"><div><span>主入口</span><h2 id="exit-summary-title">实际出口</h2></div><StatusBadge ok={Boolean(activeNode || usesConfiguredMode)} text={activeNode ? "可用" : usesConfiguredMode ? "由模式决定" : "不可用"} /></div>
            <div className="exit-node">
              <div className="exit-node-icon"><Globe2 size={24} aria-hidden="true" /></div>
              <div><strong>{exitLabel}</strong><small>{activeNode?.subscription === "manual" ? "手动节点" : activeNode?.subscription || "由访问规则决定"}</small></div>
              {activeNode && <span className={delayClass(activeNode)}>{formatDelay(activeNode)}</span>}
            </div>
            <dl className="exit-facts">
              <div><dt>策略</dt><dd>{policyLabel}</dd></div>
              <div><dt>主端口</dt><dd>{overview.mixed_port}</dd></div>
              <div><dt>候选节点</dt><dd>{aliveCount} 个可用</dd></div>
            </dl>
          </section>
          <TrafficPanel traffic={traffic} />
        </div>

        <OverviewAttention items={attentionItems} onNavigate={onNavigate} />

        <section className="overview-quick-settings" aria-label="快速接管设置">
          <UISwitch checked={overview.system_proxy} label="接管系统代理" onCheckedChange={onSystemProxy} />
          <UISwitch checked={Boolean(overview.tun?.enabled)} label="启用 TUN" onCheckedChange={onTun} />
          <UISwitch checked={Boolean(overview.port_mapping_enabled)} label="节点端口映射" onCheckedChange={onPortMapping} />
          <button type="button" onClick={() => onNavigate("ports")}>查看全部代理入口 <ArrowRight size={15} aria-hidden="true" /></button>
        </section>
      </div>
    </section>
  );
}

/**
 * resolveMainPolicy 计算当前真正生效的主入口策略。
 *
 * 参数说明：
 * - overview: object，包含 main_auto 与 main_node 的概览响应。
 *
 * 返回值说明：
 * 返回 "auto"、"fixed" 或 "rule"，顺序与后端优先级保持一致。
 *
 * 可能的异常/错误情况：
 * overview 字段缺失时安全回退为规则策略，不抛出异常。
 */
function resolveMainPolicy(overview) {
  /*
   * 必须先判断 main_auto。后端将其定义为最高优先级，即使配置文件中还残留
   * main_node，实际主入口也会使用自动测速结果。把 main_node 放在前面会让界面
   * 显示固定节点，却与运行时生成的 mihomo 配置不一致。
   */
  if (overview?.main_auto) return "auto";
  /*
   * main_auto 关闭后，非空 main_node 表示持久化的固定节点意图。此处不检查节点
   * 是否健康，因为健康状态只决定是否运行时回退，不应悄悄改写用户选择。
   */
  if (overview?.main_node) return "fixed";
  /*
   * 两个覆盖字段都未启用时才进入规则策略；此时 rule/global/direct 决定主端口
   * 流量是按规则匹配、统一代理还是全部直连。
   */
  return "rule";
}

/**
 * resolveActiveNode 为可确定出口的策略找到节点模型。
 *
 * 参数说明：
 * - overview: object，包含 nodes、main_node 与 main_node_up 的概览响应。
 * - policy: "rule" | "auto" | "fixed"，已解析的主入口策略。
 *
 * 返回值说明：
 * 固定节点可用时返回对应节点；自动最快返回延迟最低的健康节点；规则策略返回 null。
 *
 * 可能的异常/错误情况：
 * 节点列表为空、固定节点失效或延迟字段非法时返回 null，避免界面虚构实际出口。
 */
function resolveActiveNode(overview, policy) {
  const nodes = (overview?.nodes || []).filter((node) => node.alive);
  if (policy === "fixed") {
    if (!overview?.main_node_up) return null;
    return nodes.find((node) => node.key === overview.main_node) || null;
  }
  if (policy === "auto") {
    return [...nodes].sort((left, right) => (left.delay || Number.POSITIVE_INFINITY) - (right.delay || Number.POSITIVE_INFINITY))[0] || null;
  }
  return null;
}

/**
 * buildOverviewAttention 汇总需要用户关注但不一定阻断代理的状态。
 *
 * 参数说明：
 * - overview: object，完整概览响应。
 * - traffic: object，实时流量连接状态。
 * - aliveCount: number，健康节点数量。
 *
 * 返回值说明：
 * 返回 Array<{text: string, view: string}>；数组为空表示没有需要主动提醒的状态。
 *
 * 可能的异常/错误情况：
 * 缺失的可选字段会被忽略；函数只生成展示模型，不修改任何配置。
 */
function buildOverviewAttention(overview, traffic, aliveCount) {
  const items = [];
  if (aliveCount === 0) items.push({ text: "当前没有健康节点，请检查订阅或手动节点。", view: "nodes" });
  if (overview.main_node && !overview.main_node_up && !overview.main_auto) items.push({ text: `固定节点当前不可用，主入口已经临时回退到${MODE_LABELS[overview.mode] || overview.mode}模式。`, view: "nodes" });
  if (overview.tun?.enabled && !overview.tun?.active) items.push({ text: "TUN 已配置但没有实际生效，请检查权限与运行日志。", view: "logs" });
  if (!overview.dns_custom && overview.tun?.enabled && overview.dns_preset === "off") items.push({ text: "TUN 已开启但 DNS 预设关闭，建议评估 Fake IP。", view: "settings" });
  if (!overview.port_mapping_enabled) items.push({ text: "节点一对一端口当前未监听，稳定分配仍然保留。", view: "ports" });
  if (traffic.error) items.push({ text: "实时流量暂不可用，控制台正在自动重连。", view: "logs" });
  return items;
}

/**
 * PolicyOption 渲染一个可切换的主入口策略。
 *
 * 参数说明：
 * - active: boolean，是否为当前策略。
 * - detail/label: string，策略说明与名称。
 * - icon: React.ComponentType，来自现有图标库的线性图标组件。
 * - tone: string，蓝色、青色或靛蓝语义色。
 * - onClick: Function，切换策略的回调。
 *
 * 返回值说明：
 * 返回具有按下状态语义的按钮元素。
 *
 * 可能的异常/错误情况：
 * 回调错误由上层统一处理；组件本身不抛出异常。
 */
function PolicyOption({ active, detail, icon: Icon, label, tone, onClick }) {
  return (
    <button aria-pressed={active} className={classNames("policy-option", active && "active", tone)} role="listitem" type="button" onClick={onClick}>
      <span className="policy-option-icon"><Icon size={20} aria-hidden="true" /></span>
      <span><b>{label}</b><small>{detail}</small></span>
      <span className="policy-option-check" aria-hidden="true">{active ? <CheckCircle2 size={18} /> : <i />}</span>
    </button>
  );
}

/**
 * RouteStep 渲染有效流量路径中的单个步骤。
 *
 * 参数说明：
 * - detail/label: string，步骤当前值与名称。
 * - icon: React.ComponentType，步骤图标。
 * - tone: string，步骤语义色。
 *
 * 返回值说明：
 * 返回只读路径节点。
 *
 * 可能的异常/错误情况：
 * 无；长文本会由 CSS 自动换行或省略。
 */
function RouteStep({ detail, icon: Icon, label, tone }) {
  return <div className={classNames("route-step", tone)}><Icon size={22} aria-hidden="true" /><span>{label}<small>{detail}</small></span></div>;
}

/**
 * OverviewAttention 渲染概览页的可操作提醒区。
 *
 * 参数说明：
 * - items: Array<{text: string, view: string}>，提醒与目标页面。
 * - onNavigate: Function，跳转到处理页面的回调。
 *
 * 返回值说明：
 * 有提醒时返回警告区；没有提醒时返回简洁的正常状态条。
 *
 * 可能的异常/错误情况：
 * 无；未知 view 仍交由上层导航处理。
 */
function OverviewAttention({ items, onNavigate }) {
  if (!items.length) {
    return <section className="overview-attention clear"><CheckCircle2 size={19} aria-hidden="true" /><div><h2>当前无异常</h2><p>主入口、节点健康与实时状态均未发现需要处理的问题。</p></div></section>;
  }
  return (
    <section className="overview-attention">
      <CircleAlert size={20} aria-hidden="true" />
      <div><h2>需要注意</h2><ul>{items.slice(0, 3).map((item) => <li key={item.text}><span>{item.text}</span><button type="button" onClick={() => onNavigate(item.view)}>去处理 <ArrowRight size={14} aria-hidden="true" /></button></li>)}</ul></div>
    </section>
  );
}

/**
 * VersionNotice 在发现新稳定版本时显示轻量下载提示。
 *
 * 参数说明：
 * - status: object，overview.version_check 缓存状态。
 *
 * 返回值说明：
 * 有更新时返回全宽链接提示；其余状态返回 null，避免失败状态干扰代理日常操作。
 *
 * 可能的异常/错误情况：
 * 缺少 URL 或 latest 时不渲染链接；版本检查失败信息仍可在设置页查看。
 */
function VersionNotice({ status }) {
  if (status?.state !== "available" || !status.url || !status.latest) return null;
  return (
    <a className="update-notice" href={status.url} rel="noreferrer" target="_blank">
      <span>发现新版本 <b>{status.latest}</b></span>
      <span>查看 Release <ExternalLink size={15} /></span>
    </a>
  );
}

/**
 * versionCheckMessage 把版本检查状态转换为设置页的紧凑说明。
 *
 * 参数说明：
 * - status: object，版本检查开关和最近一次结果。
 *
 * 返回值说明：
 * 返回适合设置面板展示的中文状态；无状态时返回空字符串。
 *
 * 可能的异常/错误情况：
 * 未知状态使用后端 message 或空字符串，不在前端猜测网络错误。
 */
function versionCheckMessage(status) {
  if (!status) return "";
  if (!status.enabled) return "已关闭";
  switch (status.state) {
    case "pending": return "等待启动检查";
    case "checking": return "正在检查";
    case "current": return status.latest ? `当前已是最新版本 ${status.latest}` : "当前已是最新版本";
    case "available": return `发现新版本 ${status.latest}`;
    case "unsupported": return "当前构建版本不可比较";
    case "failed": return "本次检查失败，不影响代理功能";
    default: return status.message || "";
  }
}

/**
 * TrafficPanel 渲染实时上下行速率条。
 *
 * 参数说明：
 * - traffic: object，包含 up/down 当前速率与 upTotal/downTotal 累计值。
 *
 * 返回值说明：
 * 返回概览页顶部的实时速率 React 元素。
 *
 * 可能的异常/错误情况：
 * 流量流不可用时展示离线状态；组件不主动发起请求。
 */
function TrafficPanel({ traffic }) {
  const peak = Math.max(traffic.up || 0, traffic.down || 0, 1);
  const upWidth = `${Math.max(4, Math.round(((traffic.up || 0) / peak) * 100))}%`;
  const downWidth = `${Math.max(4, Math.round(((traffic.down || 0) / peak) * 100))}%`;
  const chartPeak = Math.max(
    1,
    ...(traffic.history || []).flatMap((sample) => [sample.up || 0, sample.down || 0]),
  );
  const uploadPath = buildTrafficPath(traffic.history || [], "up", chartPeak);
  const downloadPath = buildTrafficPath(traffic.history || [], "down", chartPeak);
  return (
    <section className="panel traffic-panel full">
      <div>
        <PanelTitle title="实时速率" />
        <StatusBadge ok={traffic.connected} text={traffic.connected ? "已连接" : "离线"} />
      </div>
      <div className="traffic-grid">
        <div className="traffic-row">
          <span>下载</span>
          <b>{formatBytes(traffic.down)}/s</b>
          <i><em style={{ width: downWidth }} /></i>
          <small>累计 {formatBytes(traffic.downTotal)}</small>
        </div>
        <div className="traffic-row">
          <span>上传</span>
          <b>{formatBytes(traffic.up)}/s</b>
          <i><em style={{ width: upWidth }} /></i>
          <small>累计 {formatBytes(traffic.upTotal)}</small>
        </div>
      </div>
      <div className="traffic-chart" aria-label="最近 60 个采样点的上下行速率趋势">
        <svg role="img" viewBox="0 0 600 92" preserveAspectRatio="none">
          <path className="traffic-line download" d={downloadPath} fill="none" pathLength="1" />
          <path className="traffic-line upload" d={uploadPath} fill="none" pathLength="1" />
        </svg>
        <span><i className="download" />下载</span><span><i className="upload" />上传</span>
      </div>
      {traffic.error && <p className="traffic-message"><CircleAlert size={14} aria-hidden="true" />{traffic.error}</p>}
    </section>
  );
}

/**
 * buildTrafficPath 把定长流量采样转换成 SVG 折线路径。
 *
 * 功能说明：
 * 趋势图只用于表达最近一分钟的相对变化，因此以当前窗口峰值归一化，避免 Mbps
 * 与 B/s 跨量级时曲线贴底。单个采样点会复制为水平短线，保证刚打开页面也可见。
 *
 * 参数说明：
 * - samples: Array<object>，包含 up/down 的采样数组。
 * - key: string，要绘制的数值字段，取 `up` 或 `down`。
 * - peak: number，当前窗口归一化峰值，必须大于 0。
 *
 * 返回值说明：
 * 返回合法的 SVG path `d` 字符串。
 *
 * 可能的异常/错误情况：
 * 空数组返回贴近底部的水平线；非法值按 0 处理，不向渲染层抛错。
 */
function buildTrafficPath(samples, key, peak) {
  const points = samples.length ? samples : [{ [key]: 0 }, { [key]: 0 }];
  const divisor = Math.max(points.length - 1, 1);
  return points.map((sample, index) => {
    const x = (index / divisor) * 600;
    const value = Math.max(0, Number(sample[key]) || 0);
    const y = 86 - Math.min(1, value / Math.max(peak, 1)) * 76;
    return `${index === 0 ? "M" : "L"}${x.toFixed(2)} ${y.toFixed(2)}`;
  }).join(" ");
}

/**
 * NodesPage 渲染跨来源聚合的节点工作台。
 *
 * 功能说明：
 * 节点与订阅拆分后，本页只承担节点搜索、来源/协议/状态筛选、测速和主节点选择。
 * 添加手动节点使用 Radix Dialog，避免常驻大表单挤压列表首屏。
 *
 * 参数说明：
 * - forms: object，全局受控表单状态。
 * - initialSource: string，从订阅页跳转时指定的初始来源。
 * - overview: object，概览、节点和稳定端口分配数据。
 * - onDelete/onForm/onMainNode/onPost/onSourceChange/onTest: Function，页面动作回调。
 *
 * 返回值说明：
 * 返回节点工作台 React 元素。
 *
 * 可能的异常/错误情况：
 * 空地址会在前端拦截；后端解析、热更新或持久化失败由 onPost 统一 toast。
 */
function NodesPage({ forms, initialSource, overview, onDelete, onForm, onMainNode, onPost, onSourceChange, onTest }) {
  const [addOpen, setAddOpen] = useState(false);
  const [adding, setAdding] = useState(false);
  const [query, setQuery] = useState("");
  const [source, setSource] = useState(initialSource || "all");
  const [protocol, setProtocol] = useState("all");
  const [status, setStatus] = useState("all");
  const [sort, setSort] = useState("default");

  useEffect(() => {
    setSource(initialSource || "all");
  }, [initialSource]);

  const sourceOptions = useMemo(
    () => ["manual", ...(overview.subscriptions || []).map((subscription) => subscription.name)],
    [overview.subscriptions],
  );
  const protocolOptions = useMemo(
    () => [...new Set((overview.nodes || []).map((node) => node.type).filter(Boolean))].sort(),
    [overview.nodes],
  );
  const assignmentMap = useMemo(
    () => new Map((overview.port_assignments || []).map((entry) => [`${entry.subscription}:${entry.node}`, entry.port])),
    [overview.port_assignments],
  );
  const visibleNodes = useMemo(() => {
    const normalizedQuery = query.trim().toLowerCase();
    const filtered = (overview.nodes || []).filter((node) => {
      if (source !== "all" && node.subscription !== source) return false;
      if (protocol !== "all" && node.type !== protocol) return false;
      if (status === "alive" && !node.alive) return false;
      if (status === "failed" && node.alive) return false;
      if (normalizedQuery && !`${node.name} ${node.subscription} ${node.type}`.toLowerCase().includes(normalizedQuery)) return false;
      return true;
    });
    return sortByDelay(filtered, sort);
  }, [overview.nodes, protocol, query, sort, source, status]);

  /**
   * submitManualNode 新增一个手动节点并在成功后复位对话框。
   *
   * 参数说明：
   * - event: React.FormEvent<HTMLFormElement>，表单提交事件。
   *
   * 返回值说明：
   * 返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 地址为空或重复提交时直接返回；后端错误由 onPost 展示且对话框保持打开。
   */
  async function submitManualNode(event) {
    event.preventDefault();
    const url = forms.manualURL.trim();
    if (!url || adding) return;
    setAdding(true);
    try {
      const name = forms.manualName.trim();
      const saved = await onPost("/api/manual-nodes", name ? { url, name } : { url }, "节点已添加，后台刷新中");
      if (saved) {
        onForm("manualURL", "");
        onForm("manualName", "");
        setAddOpen(false);
      }
    } finally {
      setAdding(false);
    }
  }

  /**
   * changeSource 同步本页与 App 的来源筛选。
   *
   * 参数说明：
   * - nextSource: string，目标来源名或 `all`。
   *
   * 返回值说明：无。
   *
   * 可能的异常/错误情况：无；不存在的来源只会得到空列表。
   */
  function changeSource(nextSource) {
    setSource(nextSource);
    onSourceChange(nextSource);
  }

  return (
    <div className="stack">
      <PageHeader eyebrow="代理资源" title="代理节点" detail="跨订阅查看健康状态、延迟与稳定端口分配。">
        <div className="page-actions">
          <Button variant="outline" type="button" onClick={onTest}><Gauge size={16} aria-hidden="true" />测试全部</Button>
          <Button type="button" onClick={() => setAddOpen(true)}><Plus size={16} aria-hidden="true" />添加节点</Button>
        </div>
      </PageHeader>

      <section className="panel compact-panel">
        <div className="filter-grid node-filters">
          <Field compact label="搜索节点">
            <div className="input-with-icon"><Search size={15} aria-hidden="true" /><Input value={query} placeholder="名称 / 来源 / 协议" onChange={(event) => setQuery(event.target.value)} /></div>
          </Field>
          <Field compact label="来源">
            <Select
              ariaLabel="筛选节点来源"
              value={source}
              onValueChange={changeSource}
              options={[
                { value: "all", label: "全部来源" },
                ...sourceOptions.map((name) => ({ value: name, label: name === "manual" ? "手动节点" : name })),
              ]}
            />
          </Field>
          <Field compact label="协议">
            <Select
              ariaLabel="筛选节点协议"
              value={protocol}
              onValueChange={setProtocol}
              options={[
                { value: "all", label: "全部协议" },
                ...protocolOptions.map((name) => ({ value: name, label: name.toUpperCase() })),
              ]}
            />
          </Field>
          <Field compact label="状态">
            <Select
              ariaLabel="筛选节点状态"
              value={status}
              onValueChange={setStatus}
              options={[{ value: "all", label: "全部状态" }, { value: "alive", label: "可用" }, { value: "failed", label: "异常" }]}
            />
          </Field>
          <Field compact label="排序">
            <Select
              ariaLabel="节点排序方式"
              value={sort}
              onValueChange={setSort}
              options={[{ value: "default", label: "默认顺序" }, { value: "asc", label: "延迟升序" }, { value: "desc", label: "延迟降序" }]}
            />
          </Field>
        </div>
      </section>

      {!overview.port_mapping_enabled && (
        <div className="notice-row"><CircleAlert size={16} aria-hidden="true" /><span>节点端口映射已关闭。稳定分配仍保留，但这些端口当前不监听。</span></div>
      )}
      <NodeTable
        assignmentMap={assignmentMap}
        mainNode={overview.main_node}
        mappingEnabled={overview.port_mapping_enabled}
        nodes={visibleNodes}
        onMainNode={onMainNode}
      />

      {(overview.manual_nodes || []).length > 0 && (
        <section className="panel compact-panel">
          <PanelTitle title="手动节点源" detail="这里管理原始链接；健康状态统一在上方节点表查看。" />
          <ul className="item-list compact-list">
            {overview.manual_nodes.map((node) => (
              <li key={node.index}>
                <b>{node.name || "未命名节点"}</b><code>{node.url}</code>
                <Button aria-label={`删除手动节点 ${node.name || node.index}`} size="icon" variant="destructive-ghost" type="button" onClick={() => onDelete(`/api/manual-nodes/${node.index}`, "节点已删除，刷新中", `手动节点 ${node.name || "未命名节点"}`)}><Trash2 size={16} aria-hidden="true" /></Button>
              </li>
            ))}
          </ul>
        </section>
      )}

      <Dialog open={addOpen} onOpenChange={setAddOpen}>
        <DialogContent>
          <DialogClose className="dialog-close" aria-label="关闭添加节点对话框"><X size={16} aria-hidden="true" /></DialogClose>
          <form onSubmit={submitManualNode}>
            <DialogHeader>
              <DialogTitle>添加手动节点</DialogTitle>
              <DialogDescription>粘贴完整节点链接；名称留空时使用节点自带名称。</DialogDescription>
            </DialogHeader>
            <div className="dialog-form">
              <Field label="节点地址" hint="支持 socks5://、ss:// 等完整链接"><Input autoFocus value={forms.manualURL} placeholder="socks5://user:pass@host:1080" onChange={(event) => onForm("manualURL", event.target.value)} /></Field>
              <Field label="显示名称" hint="可选"><Input value={forms.manualName} placeholder="例如：东京备用" onChange={(event) => onForm("manualName", event.target.value)} /></Field>
            </div>
            <DialogFooter>
              <Button variant="outline" type="button" onClick={() => setAddOpen(false)}>取消</Button>
              <Button disabled={!forms.manualURL.trim()} loading={adding} type="submit">添加节点</Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </div>
  );
}

/**
 * SubscriptionsPage 渲染订阅资源管理页。
 *
 * 功能说明：
 * 以紧凑卡片展示订阅状态、用量与节点健康度；新增和编辑统一使用 Radix Dialog。
 * 停用操作会提交完整值对象，避免布尔开关覆盖名称、URL 或解析类型。
 *
 * 参数说明：
 * - overview: object，订阅和节点聚合状态。
 * - onDelete/onNavigateNodes/onSubAction/onWrite: Function，删除、跳转、刷新测速与写入回调。
 *
 * 返回值说明：
 * 返回订阅管理页 React 元素。
 *
 * 可能的异常/错误情况：
 * 启用无可用缓存的订阅可能同步失败；失败时弹窗保留或开关回退为服务端状态。
 */
function SubscriptionsPage({ overview, onDelete, onNavigateNodes, onSubAction, onWrite }) {
  const emptyDraft = { name: "", url: "", type: "auto", enabled: true };
  const [editor, setEditor] = useState(null);
  const [draft, setDraft] = useState(emptyDraft);
  const [saving, setSaving] = useState(false);

  /**
   * openEditor 打开新增或编辑对话框。
   *
   * 参数说明：
   * - subscription: object | null，现有订阅；null 表示新增。
   *
   * 返回值说明：无。
   *
   * 可能的异常/错误情况：无；缺失字段使用安全默认值。
   */
  function openEditor(subscription = null) {
    setEditor(subscription ? { mode: "edit", originalName: subscription.name } : { mode: "add", originalName: "" });
    setDraft(subscription ? {
      name: subscription.name || "",
      url: subscription.url || "",
      type: subscription.type || "auto",
      enabled: subscription.enabled !== false,
    } : emptyDraft);
  }

  /**
   * updateDraft 更新订阅对话框的单个字段。
   *
   * 参数说明：
   * - key: string，字段名。
   * - value: string | boolean，字段值。
   *
   * 返回值说明：无。
   *
   * 可能的异常/错误情况：无；最终合法性由提交时后端校验。
   */
  function updateDraft(key, value) {
    setDraft((current) => ({ ...current, [key]: value }));
  }

  /**
   * submitSubscription 提交新增或编辑事务。
   *
   * 参数说明：
   * - event: React.FormEvent<HTMLFormElement>，表单提交事件。
   *
   * 返回值说明：返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 必填项为空时不提交；启用时的下载、校验、热更新或持久化失败由 onWrite toast。
   */
  async function submitSubscription(event) {
    event.preventDefault();
    if (!draft.name.trim() || !draft.url.trim() || saving || !editor) return;
    setSaving(true);
    try {
      const payload = { ...draft, name: draft.name.trim(), url: draft.url.trim() };
      const editing = editor.mode === "edit";
      const url = editing ? `/api/subscriptions/${encodeURIComponent(editor.originalName)}` : "/api/subscriptions";
      const saved = await onWrite(url, payload, editing ? "订阅已更新" : "订阅已添加", editing ? "PUT" : "POST");
      if (saved) setEditor(null);
    } finally {
      setSaving(false);
    }
  }

  /**
   * toggleSubscription 切换现有订阅启用状态。
   *
   * 参数说明：
   * - subscription: object，当前订阅完整快照。
   * - enabled: boolean，目标启用状态。
   *
   * 返回值说明：返回 Promise<boolean>，由通用写入器返回提交结果。
   *
   * 可能的异常/错误情况：
   * 启用且拉取失败、又没有可用缓存时后端拒绝，轮询后开关保持原状态。
   */
  function toggleSubscription(subscription, enabled) {
    return onWrite(
      `/api/subscriptions/${encodeURIComponent(subscription.name)}`,
      { name: subscription.name, url: subscription.url, type: subscription.type || "auto", enabled },
      enabled ? "订阅已启用" : "订阅已停用",
      "PUT",
    );
  }

  return (
    <div className="stack">
      <PageHeader eyebrow="代理资源" title="订阅管理" detail="管理来源、启用状态、同步与缓存健康度。">
        <Button type="button" onClick={() => openEditor()}><Plus size={16} aria-hidden="true" />添加订阅</Button>
      </PageHeader>
      <div className="subscription-cards">
        {(overview.subscriptions || []).map((subscription) => {
          const info = formatUserInfo(subscription.userinfo);
          const stateLabel = {
            disabled: "已停用", empty: "无节点", error: "异常", degraded: "使用缓存", healthy: "正常",
          }[subscription.state] || "未知";
          return (
            <article className="subscription-card" key={subscription.name}>
              <div className="subscription-card-head">
                <div className="subscription-icon"><Rss size={18} aria-hidden="true" /></div>
                <div><h3>{subscription.name}</h3><p title={subscription.url}>{subscription.url}</p></div>
                <UISwitch ariaLabel={`启用 ${subscription.name}`} checked={subscription.enabled !== false} onCheckedChange={(enabled) => toggleSubscription(subscription, enabled)} />
              </div>
              <div className="subscription-stats">
                <span><b>{subscription.alive}</b>/{subscription.total} 可用</span>
                <StatusBadge ok={subscription.state === "healthy"} text={stateLabel} />
                <Badge variant="outline">{(subscription.type || "auto").toUpperCase()}</Badge>
              </div>
              {info && <div className="subscription-usage"><span>{info.usage}</span><span className={classNames(info.urgent && "urgent")}>{info.expire}</span></div>}
              <div className="subscription-card-actions">
                <Button size="sm" variant="ghost" type="button" onClick={() => onNavigateNodes(subscription.name)}>查看节点</Button>
                <Button disabled={!subscription.enabled} size="sm" variant="outline" type="button" onClick={() => onSubAction(subscription.name, "test")}>测速</Button>
                <Button disabled={!subscription.enabled} size="sm" variant="outline" type="button" onClick={() => onSubAction(subscription.name, "refresh")}><RefreshCw size={14} aria-hidden="true" />同步</Button>
                <Button aria-label={`编辑订阅 ${subscription.name}`} size="icon" variant="ghost" type="button" onClick={() => openEditor(subscription)}><Pencil size={15} aria-hidden="true" /></Button>
                <Button aria-label={`删除订阅 ${subscription.name}`} size="icon" variant="destructive-ghost" type="button" onClick={() => onDelete(`/api/subscriptions/${encodeURIComponent(subscription.name)}`, "订阅已删除", `订阅 ${subscription.name}`)}><Trash2 size={15} aria-hidden="true" /></Button>
              </div>
            </article>
          );
        })}
      </div>
      {(overview.subscriptions || []).length === 0 && <EmptyState title="还没有订阅" detail="添加订阅后，节点会自动出现在代理节点页面。" />}

      <Dialog open={Boolean(editor)} onOpenChange={(open) => { if (!open && !saving) setEditor(null); }}>
        <DialogContent>
          <DialogClose className="dialog-close" aria-label="关闭订阅对话框"><X size={16} aria-hidden="true" /></DialogClose>
          <form onSubmit={submitSubscription}>
            <DialogHeader>
              <DialogTitle>{editor?.mode === "edit" ? "编辑订阅" : "添加订阅"}</DialogTitle>
              <DialogDescription>启用订阅时会立即拉取并校验；失败且无缓存时不会提交。</DialogDescription>
            </DialogHeader>
            <div className="dialog-form">
              <Field label="订阅名称"><Input autoFocus value={draft.name} placeholder="例如：主力机场" onChange={(event) => updateDraft("name", event.target.value)} /></Field>
              <Field label="订阅地址"><Input value={draft.url} placeholder="https://example.com/subscription" onChange={(event) => updateDraft("url", event.target.value)} /></Field>
              <Field label="解析类型">
                <Select
                  ariaLabel="订阅解析类型"
                  value={draft.type}
                  onValueChange={(value) => updateDraft("type", value)}
                  options={[{ value: "auto", label: "自动识别" }, { value: "clash", label: "Clash YAML" }, { value: "share", label: "分享链接" }]}
                />
              </Field>
              <UISwitch checked={draft.enabled} label="保存后启用此订阅" onCheckedChange={(enabled) => updateDraft("enabled", enabled)} />
            </div>
            <DialogFooter>
              <Button disabled={saving} variant="outline" type="button" onClick={() => setEditor(null)}>取消</Button>
              <Button disabled={!draft.name.trim() || !draft.url.trim()} loading={saving} type="submit">{editor?.mode === "edit" ? "保存修改" : "添加订阅"}</Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </div>
  );
}

/**
 * NodeTable 渲染节点列表表格。
 *
 * 参数说明：
 * - nodes: Array<object>，节点数据。
 * - assignmentMap: Map<string, number>，按来源与节点名索引的稳定端口分配。
 * - mainNode: string，当前主端口固定节点 key。
 * - mappingEnabled: boolean，节点一对一 listener 是否启用。
 * - onMainNode: Function，设置主端口节点回调。
 *
 * 返回值说明：
 * 返回表格 React 元素。
 *
 * 可能的异常/错误情况：
 * 无；节点不可用时按钮禁用。
 */
function NodeTable({ nodes, assignmentMap = new Map(), mainNode, mappingEnabled = true, onMainNode }) {
  const columns = useMemo(
    () => [
      {
        key: "name",
        header: "节点",
        sortable: true,
        width: "34%",
        cell: (node) => <span className={classNames("node-name", !node.alive && "text-muted-foreground")} title={node.name}>{node.name}</span>,
      },
      { key: "type", header: "类型", sortable: true, width: "100px", cell: (node) => node.type || "-" },
      { key: "subscription", header: "来源", sortable: true, width: "18%", cell: (node) => node.subscription === "manual" ? "手动节点" : node.subscription },
      {
        key: "port",
        header: "稳定端口",
        sortable: true,
        width: "118px",
        cell: (node) => {
          const assignedPort = node.port || assignmentMap.get(`${node.subscription}:${node.name}`);
          return assignedPort ? <span className={classNames(!mappingEnabled && "text-muted-foreground")}>{assignedPort}{!mappingEnabled && <small className="block">未监听</small>}</span> : "-";
        },
        sortValue: (node) => node.port || assignmentMap.get(`${node.subscription}:${node.name}`) || Number.POSITIVE_INFINITY,
      },
      {
        key: "delay",
        header: "延迟",
        sortable: true,
        width: "105px",
        cell: (node) => <span className={delayClass(node)}>{formatDelay(node)}</span>,
        sortValue: (node) => node.alive && node.delay > 0 ? node.delay : Number.POSITIVE_INFINITY,
      },
      {
        key: "alive",
        header: "状态",
        sortable: true,
        width: "110px",
        cell: (node) => <StatusBadge ok={node.alive} text={node.alive ? "可用" : "失效"} />,
        sortValue: (node) => node.alive ? 1 : 0,
      },
      {
        key: "action",
        header: "操作",
        width: "156px",
        cell: (node) => (
          <Button size="sm" variant="outline" disabled={!node.alive || mainNode === node.key} type="button" onClick={() => onMainNode(node.key)}>
            {mainNode === node.key ? "当前主端口" : "设为主端口"}
          </Button>
        ),
      },
    ],
    [assignmentMap, mainNode, mappingEnabled, onMainNode],
  );

  return (
    <Table
      className="data-table"
      columns={columns}
      data={nodes}
      emptyState="暂无节点"
      getRowId={(node, index) => node.key || `${node.subscription}:${node.name}:${index}`}
      height={tableViewportHeight(nodes.length, 528)}
      minColumnWidth={84}
      resizable
    />
  );
}

/**
 * PortsPage 渲染端口映射表。
 *
 * 参数说明：
 * - overview: object，概览数据。
 * - portSort: string，当前排序模式。
 * - onCopy/onSort/onToggle: Function，复制、排序与映射开关回调。
 *
 * 返回值说明：
 * 返回端口页 React 元素。
 *
 * 可能的异常/错误情况：
 * 剪贴板错误由父组件处理。
 */
function PortsPage({ overview, portSort, onCopy, onSort, onToggle }) {
  const sourcePorts = overview.port_mapping_enabled ? overview.ports : (overview.port_assignments || []);
  const ports = portSort === "delay" ? sortByDelay(sourcePorts, "delay") : sourcePorts;
  const columns = useMemo(
    () => [
      {
        key: "port",
        header: "端口",
        sortable: true,
        width: "130px",
        cell: (port) => (
          <button className="copy-link" disabled={!overview.port_mapping_enabled} type="button" onClick={() => onCopy(port.port)}>
            {port.port}<Copy size={14} aria-hidden="true" />
          </button>
        ),
      },
      { key: "node", header: "节点", sortable: true, width: "36%", cell: (port) => <span className="node-name" title={port.node}>{port.node}</span> },
      { key: "subscription", header: "订阅", sortable: true, width: "24%" },
      {
        key: "delay",
        header: "延迟",
        sortable: true,
        width: "110px",
        cell: (port) => <span className={delayClass(port)}>{formatDelay(port)}</span>,
        sortValue: (port) => port.alive && port.delay > 0 ? port.delay : Number.POSITIVE_INFINITY,
      },
      {
        key: "alive",
        header: "状态",
        sortable: true,
        width: "110px",
        cell: (port) => <StatusBadge ok={overview.port_mapping_enabled && port.alive} text={!overview.port_mapping_enabled ? "未监听" : port.alive ? "可用" : "失效"} />,
        sortValue: (port) => port.alive ? 1 : 0,
      },
    ],
    [onCopy, overview.port_mapping_enabled],
  );
  return (
    <div className="stack">
      <PageHeader eyebrow="网络入口" title="代理入口" detail="管理健康节点的一对一监听，关闭后仍保留稳定端口分配。">
        <UISwitch checked={Boolean(overview.port_mapping_enabled)} label="启用节点端口映射" onCheckedChange={onToggle} />
      </PageHeader>
      <section className="panel compact-panel">
      <div className="toolbar ports-toolbar">
        <div className={classNames("port-mapping-state", overview.port_mapping_enabled ? "enabled" : "disabled")}>
          <span>{overview.port_mapping_enabled ? "正在监听" : "已停止监听"}</span>
          <b>{ports.length} 个稳定分配</b>
        </div>
        <Field compact label="排列方式">
          <Select
            ariaLabel="代理入口排列方式"
            value={portSort}
            onValueChange={onSort}
            options={[{ value: "default", label: "按端口" }, { value: "delay", label: "按延迟" }]}
          />
        </Field>
      </div>
      <Table
        className="data-table mt-3"
        columns={columns}
        data={ports}
        emptyState={overview.port_mapping_enabled ? "暂无健康节点映射" : "暂无可保留的稳定分配"}
        getRowId={(port, index) => String(port.port || index)}
        height={tableViewportHeight(ports.length, 576)}
        minColumnWidth={88}
        resizable
      />
      {!overview.port_mapping_enabled && <p className="panel-footnote">主代理端口、自动选优端口与策略分组端口不受此开关影响。</p>}
      </section>
    </div>
  );
}

/**
 * GroupsPage 渲染节点分组页。
 *
 * 参数说明：
 * - forms: object，表单状态。
 * - groupSort: string，待选节点排序。
 * - overview: object，概览数据。
 * - selectedNodes: Set<string>，当前勾选节点。
 * - onCopy/onDelete/onForm/onSort/onSubmit/onToggleNode: Function，操作回调。
 *
 * 返回值说明：
 * 返回分组页 React 元素。
 *
 * 可能的异常/错误情况：
 * 表单缺失本地拦截，后端冲突由父组件展示。
 */
function GroupsPage({ forms, groupSort, overview, selectedNodes, onCopy, onDelete, onForm, onSort, onSubmit, onToggleNode }) {
  const [createOpen, setCreateOpen] = useState(false);
  const [editingName, setEditingName] = useState("");
  const nodes = groupSort === "delay" ? sortByDelay(overview.nodes, "delay") : overview.nodes;
  const sourceOptions = useMemo(() => {
    const names = (overview.subscriptions || []).filter((subscription) => subscription.enabled).map((subscription) => subscription.name);
    if ((overview.manual_nodes || []).length > 0 || overview.nodes.some((node) => node.subscription === "manual")) {
      names.unshift("manual");
    }
    return names;
  }, [overview]);
  const usedBy = useMemo(() => {
    const result = {};
    (overview.groups || []).forEach((group) => {
      (group.nodes || []).forEach((node) => {
        result[node] = [...(result[node] || []), group.name];
      });
    });
    return result;
  }, [overview.groups]);

  /**
   * syncSelectedNodes 把全局节点选择集合调整为目标集合。
   *
   * 参数说明：
   * - targetNames: string[]，编辑分组时需要选中的节点名。
   *
   * 返回值说明：无。
   *
   * 可能的异常/错误情况：
   * 已从当前节点目录消失的旧成员不会出现在选择器中，但后端保存完整目标前仍会校验。
   */
  function syncSelectedNodes(targetNames) {
    const target = new Set(targetNames || []);
    overview.nodes.forEach((node) => {
      if (selectedNodes.has(node.name) !== target.has(node.name)) onToggleNode(node.name);
    });
  }

  /**
   * openCreateDialog 复位分组草稿并打开新建对话框。
   *
   * 参数说明：无。
   * 返回值说明：无。
   * 可能的异常/错误情况：无；草稿清理只影响尚未提交的表单状态。
   */
  function openCreateDialog() {
    setEditingName("");
    onForm("groupName", "");
    onForm("groupPort", "");
    onForm("groupType", "fallback");
    onForm("groupSubscription", "");
    syncSelectedNodes([]);
    setCreateOpen(true);
  }

  /**
   * openEditDialog 用现有分组值填充编辑对话框。
   *
   * 参数说明：
   * - group: object，overview 中的完整策略分组。
   *
   * 返回值说明：无。
   *
   * 可能的异常/错误情况：无；分组名在编辑期间锁定以保护 dialer-proxy 引用。
   */
  function openEditDialog(group) {
    setEditingName(group.name);
    onForm("groupName", group.name);
    onForm("groupPort", String(group.port));
    onForm("groupType", group.type || "url-test");
    onForm("groupSubscription", group.subscription || "");
    syncSelectedNodes(group.nodes || []);
    setCreateOpen(true);
  }

  /**
   * submitGroupDialog 提交策略分组并在事务成功后关闭对话框。
   *
   * 参数说明：无；字段由父级 forms 与 selectedNodes 提供。
   *
   * 返回值说明：返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 表单不完整、端口冲突或后端持久化失败时保持弹窗打开，方便继续修正。
   */
  async function submitGroupDialog() {
    if (await onSubmit(editingName)) setCreateOpen(false);
  }

  return (
    <div className="stack">
      <PageHeader eyebrow="访问策略" title="策略分组" detail="为指定场景提供独立入口，并查看候选节点健康度。">
        <Button type="button" onClick={openCreateDialog}><Plus size={16} aria-hidden="true" />新建分组</Button>
      </PageHeader>
      <section className="panel">
        <PanelTitle title="已有策略分组" detail="分组提供独立代理入口，并按选定策略选择节点" />
        <ul className="item-list">
          {(overview.groups || []).map((group) => (
            <li key={group.name}>
              <b>{group.name}</b>
              <button className="copy-link" type="button" onClick={() => onCopy(group.port)}>:{group.port}<Copy size={14} /></button>
              <span>{GROUP_TYPE_LABELS[group.type] || group.type || "自动测速"}</span>
              <span>{group.subscription ? `来源：${group.subscription}` : `${(group.nodes || []).length} 个固定节点`}</span>
              <StatusBadge
                ok={group.subscription
                  ? overview.subscriptions.some((subscription) => subscription.name === group.subscription && subscription.enabled && subscription.alive > 0)
                  : (group.nodes || []).some((name) => overview.nodes.some((node) => node.name === name && node.alive))}
                text={group.subscription
                  ? `${overview.subscriptions.find((subscription) => subscription.name === group.subscription)?.alive || 0} 个可用`
                  : `${(group.nodes || []).filter((name) => overview.nodes.some((node) => node.name === name && node.alive)).length}/${(group.nodes || []).length} 可用`}
              />
              <Button aria-label={`编辑策略分组 ${group.name}`} size="icon" variant="ghost" type="button" onClick={() => openEditDialog(group)}><Pencil size={15} aria-hidden="true" /></Button>
              <Button aria-label={`删除策略分组 ${group.name}`} size="icon" variant="destructive-ghost" type="button" onClick={() => onDelete(`/api/groups/${encodeURIComponent(group.name)}`, "分组已删除", `策略分组 ${group.name}`)}>
                <Trash2 size={16} aria-hidden="true" />
              </Button>
            </li>
          ))}
        </ul>
      </section>
      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent className="max-w-2xl">
        <DialogClose className="dialog-close" aria-label="关闭新建分组对话框"><X size={16} aria-hidden="true" /></DialogClose>
        <DialogHeader><DialogTitle>{editingName ? "编辑策略分组" : "新建策略分组"}</DialogTitle><DialogDescription>可从一个订阅自动取节点，也可以手动勾选节点。编辑时分组名保持不变。</DialogDescription></DialogHeader>
        <div className="form-grid group-form">
          <Field label="分组名称"><input disabled={Boolean(editingName)} value={forms.groupName} onChange={(event) => onForm("groupName", event.target.value)} placeholder="例如：视频线路" /></Field>
          <Field label="本机端口"><input type="number" min="1" max="65535" value={forms.groupPort} onChange={(event) => onForm("groupPort", event.target.value)} placeholder="例如：42020" /></Field>
          <Field label="选择策略">
            <Select
              ariaLabel="策略分组选择策略"
              value={forms.groupType}
              onValueChange={(value) => onForm("groupType", value)}
              options={[
                { value: "fallback", label: "故障转移（按顺序切换）" },
                { value: "url-test", label: "自动测速（选择最快）" },
                { value: "load-balance", label: "负载均衡（分散连接）" },
              ]}
            />
          </Field>
          <Field label="节点来源">
            <Select
              ariaLabel="策略分组节点来源"
              value={forms.groupSubscription}
              onValueChange={(value) => onForm("groupSubscription", value)}
              options={[
                { value: "", label: "手动选择节点" },
                ...sourceOptions.map((name) => ({ value: name, label: `使用订阅：${name}` })),
              ]}
            />
          </Field>
        </div>
        <div className="toolbar">
          <Field compact label="候选节点排序">
            <Select
              ariaLabel="候选节点排序"
              value={groupSort}
              onValueChange={onSort}
              options={[{ value: "default", label: "默认顺序" }, { value: "delay", label: "延迟从低到高" }]}
            />
          </Field>
        </div>
        <div className="node-picker">
          {nodes.map((node) => (
            <label className={classNames("node-option", (!node.alive || forms.groupSubscription) && "disabled")} key={`${node.subscription}:${node.name}`}>
              <input checked={selectedNodes.has(node.name)} disabled={Boolean(forms.groupSubscription)} type="checkbox" onChange={() => onToggleNode(node.name)} />
              <span className="node-name">{node.name}</span>
              <span className={delayClass(node)}>{formatDelay(node)}</span>
              {(usedBy[node.name] || []).map((group) => <small key={group}>{group}</small>)}
            </label>
          ))}
        </div>
        <DialogFooter><Button variant="outline" type="button" onClick={() => setCreateOpen(false)}>取消</Button><Button type="button" onClick={submitGroupDialog}>{editingName ? <Pencil size={16} aria-hidden="true" /> : <Plus size={16} aria-hidden="true" />}{editingName ? "保存修改" : "创建分组"}</Button></DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

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
function LogsPage() {
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

/**
 * RulesPage 渲染规则与规则源页。
 *
 * 参数说明：
 * - forms/ruleContent/ruleUrls/overview: object，页面数据。
 * - onDelete/onForm/onPost/onViewContent: Function，操作回调。
 *
 * 返回值说明：
 * 返回规则页 React 元素。
 *
 * 可能的异常/错误情况：
 * 表单缺失本地拦截；API 失败由父组件展示。
 */
function RulesPage({ forms, ruleContent, ruleUrls, overview, onDelete, onForm, onPost, onViewContent }) {
  const [query, setQuery] = useState("");
  const [editing, setEditing] = useState(null);
  const visibleRules = useMemo(() => {
    const normalized = query.trim().toLowerCase();
    return (overview.custom_rules || [])
      .map((rule, index) => ({ rule, index }))
      .filter((item) => !normalized || item.rule.toLowerCase().includes(normalized));
  }, [overview.custom_rules, query]);

  /**
   * saveEditedRule 原位保存自定义规则。
   *
   * 参数说明：
   * - event: React.FormEvent<HTMLFormElement>，编辑弹窗提交事件。
   *
   * 返回值说明：返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 空规则直接拦截；后端校验、mihomo 重载或持久化失败由 onPost 统一展示。
   */
  async function saveEditedRule(event) {
    event.preventDefault();
    if (!editing?.rule.trim()) return;
    const saved = await onPost(`/api/rules/${editing.index}`, { rule: editing.rule.trim() }, "规则已更新", "PUT");
    if (saved) setEditing(null);
  }

  /**
   * moveRule 调整规则在完整 custom-rules 列表中的优先级。
   *
   * 参数说明：
   * - from: number，当前零基下标。
   * - to: number，目标零基下标。
   *
   * 返回值说明：返回 Promise<boolean>，由写入器给出结果。
   *
   * 可能的异常/错误情况：
   * 越界按钮在 UI 已禁用；并发配置变化仍由后端事务校验并回滚。
   */
  function moveRule(from, to) {
    return onPost("/api/rules/reorder", { from, to }, "规则优先级已更新");
  }

  return (
    <div className="stack">
      <PageHeader eyebrow="访问策略" title="规则管理" detail="维护自定义访问规则与远程规则源，列表顺序即匹配优先级。" />
      <section className="panel">
        <PanelTitle title="自定义访问规则" detail="规则按从上到下的顺序匹配，新增规则会写入当前配置" />
        <form
          className="form-grid rule-form"
          onSubmit={async (event) => {
            event.preventDefault();
            if (!forms.rule.trim()) return;
            if (await onPost("/api/rules", { rule: forms.rule.trim() }, "规则已添加")) onForm("rule", "");
          }}
        >
          <Field label="规则内容" hint="格式由规则类型、匹配值和目标策略组成">
            <input aria-label="自定义规则内容" className="mono-input" value={forms.rule} onChange={(event) => onForm("rule", event.target.value)} placeholder="DOMAIN-SUFFIX,example.com,DIRECT" />
          </Field>
          <Button className="form-submit" type="submit"><Plus size={16} aria-hidden="true" /><span>添加规则</span></Button>
        </form>
        <div className="toolbar rule-toolbar">
          <Field compact label="搜索自定义规则"><div className="input-with-icon"><Search size={15} aria-hidden="true" /><Input value={query} placeholder="类型、域名或策略" onChange={(event) => setQuery(event.target.value)} /></div></Field>
          <Badge variant="outline">{visibleRules.length}/{(overview.custom_rules || []).length} 条</Badge>
        </div>
        <ul className="item-list rule-list">
          {visibleRules.map(({ rule, index }) => (
            <li key={`${rule}:${index}`}>
              <span className="rule-index">{index + 1}</span><code>{rule}</code>
              <div className="row-actions">
                <Button aria-label={`上移规则 ${rule}`} disabled={index === 0} size="icon" variant="ghost" type="button" onClick={() => moveRule(index, index - 1)}><ArrowUp size={15} aria-hidden="true" /></Button>
                <Button aria-label={`下移规则 ${rule}`} disabled={index === overview.custom_rules.length - 1} size="icon" variant="ghost" type="button" onClick={() => moveRule(index, index + 1)}><ArrowDown size={15} aria-hidden="true" /></Button>
                <Button aria-label={`编辑规则 ${rule}`} size="icon" variant="ghost" type="button" onClick={() => setEditing({ index, rule })}><Pencil size={15} aria-hidden="true" /></Button>
                <Button aria-label={`删除规则 ${rule}`} size="icon" variant="destructive-ghost" type="button" onClick={() => onDelete(`/api/rules/${index}`, "规则已删除", `规则 ${rule}`)}>
                <Trash2 size={16} aria-hidden="true" />
                </Button>
              </div>
            </li>
          ))}
        </ul>
      </section>
      <section className="panel">
        <PanelTitle title="远程规则源" detail="远程内容只读；需要改动时请更新源文件或重新配置 URL" />
        <form
          className="form-grid rule-source-form"
          onSubmit={async (event) => {
            event.preventDefault();
            if (!forms.ruleURLName.trim() || !forms.ruleURL.trim()) return;
            if (await onPost("/api/rule-urls", { name: forms.ruleURLName.trim(), url: forms.ruleURL.trim() }, "规则 URL 已添加")) {
              onForm("ruleURLName", "");
              onForm("ruleURL", "");
            }
          }}
        >
          <Field label="规则源名称"><input value={forms.ruleURLName} onChange={(event) => onForm("ruleURLName", event.target.value)} placeholder="例如：局域网直连" /></Field>
          <Field label="规则源地址"><input value={forms.ruleURL} onChange={(event) => onForm("ruleURL", event.target.value)} placeholder="https://example.com/rules.txt" /></Field>
          <Button className="form-submit" type="submit"><Plus size={16} aria-hidden="true" /><span>添加规则源</span></Button>
        </form>
        <div className="rule-url-list">
          {ruleUrls.map((ruleURL) => (
            <article className="rule-url" key={ruleURL.name}>
              <div>
                <b>{ruleURL.name}</b>
                <code>{ruleURL.url}</code>
              </div>
              <span>{ruleURL.error ? "拉取失败" : `${ruleURL.count} 条${ruleURL.warn ? "（缓存）" : ""}`}</span>
              <Button size="sm" variant="outline" type="button" onClick={() => onViewContent(ruleURL.name)}>{ruleContent[ruleURL.name]?.open ? "收起内容" : "查看内容"}</Button>
              <Button aria-label={`删除规则源 ${ruleURL.name}`} size="icon" variant="destructive-ghost" type="button" onClick={() => onDelete(`/api/rule-urls/${encodeURIComponent(ruleURL.name)}`, "规则 URL 已删除", `规则源 ${ruleURL.name}`)}>
                <Trash2 size={16} aria-hidden="true" />
              </Button>
              {ruleContent[ruleURL.name]?.open && <pre>{ruleContent[ruleURL.name].text}</pre>}
            </article>
          ))}
        </div>
      </section>
      <Dialog open={Boolean(editing)} onOpenChange={(open) => { if (!open) setEditing(null); }}>
        <DialogContent>
          <DialogClose className="dialog-close" aria-label="关闭规则编辑对话框"><X size={16} aria-hidden="true" /></DialogClose>
          <form onSubmit={saveEditedRule}>
            <DialogHeader><DialogTitle>编辑自定义规则</DialogTitle><DialogDescription>规则会保留当前优先级位置，并在保存前完成配置校验。</DialogDescription></DialogHeader>
            <div className="dialog-form"><Field label="规则内容"><Input className="mono-input" value={editing?.rule || ""} onChange={(event) => setEditing((current) => ({ ...current, rule: event.target.value }))} /></Field></div>
            <DialogFooter><Button variant="outline" type="button" onClick={() => setEditing(null)}>取消</Button><Button disabled={!editing?.rule.trim()} type="submit">保存规则</Button></DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </div>
  );
}

// SETTINGS_HELP 定义系统设置卡片的详细用途、优先级和风险边界。
// 内容集中维护可以保证悬浮说明与真实后端行为同步，避免各处零散文案产生矛盾。
const SETTINGS_HELP = {
  mainEntry: {
    heading: "主代理入口如何工作",
    paragraphs: [
      "主端口是应用最常使用的 HTTP + SOCKS5 混合代理入口。默认情况下，它按照当前的规则、全局或直连模式决定出口。",
      "开启“始终使用当前延迟最低的节点”后，主端口会绕过访问规则并交给 AUTO 测速组；固定节点同样会绕过规则。两者同时配置时，自动选优优先于固定节点。",
    ],
    note: "修改端口会热更新代理核心；端口不能与 API、节点映射、自动选优或策略分组端口冲突。",
  },
  nodePorts: {
    heading: "节点端口范围有什么作用",
    paragraphs: [
      "启用后，每个健康节点都会获得一个独立的本地 HTTP + SOCKS5 混合端口。连接某个端口即可固定从对应节点出站，适合多账号、爬虫或需要明确出口的任务。",
      "端口分配会持久化，同一节点在刷新和重启后会尽量沿用原端口。关闭开关只停止这些 listener，不会删除稳定分配，也不影响主端口、自动选优入口或策略分组。",
    ],
    note: "范围容量不足时只会为部分健康节点提供监听；起止端口还必须避开所有其他本机入口。",
  },
  autoPort: {
    heading: "自动选优入口有什么作用",
    paragraphs: [
      "这是一个独立于主端口的快捷入口，始终由 URL-Test 组选择当前延迟最低的健康节点，并绕过访问规则。",
      "它适合希望自动选择低延迟出口、但又不想改变主端口规则行为的应用。填写 0 会关闭此入口，主端口和节点映射不受影响。",
    ],
    note: "没有健康节点时不会启动该 listener；节点恢复后会在后续刷新中自动恢复。",
  },
  dns: {
    heading: "DNS 模式选择指南",
    paragraphs: [
      "DNS 决定域名如何解析，也影响 TUN 流量能否在解析阶段准确命中域名规则。切换预设会热更新 mihomo，不需要重启 proxyd。",
    ],
    items: [
      "不启用预设：沿用系统或配置文件的解析行为。未配置自定义 DNS 时，TUN 的 DNS 劫持和域名规则可能不完整。",
      "Fake IP：先返回保留网段中的虚拟地址，再由 mihomo 还原域名并选择规则。规则识别最稳定，通常是 TUN 的推荐模式；极少数依赖真实 IP、局域网发现或特殊校验的应用可能需要额外排除。",
      "Redir Host：向应用返回真实解析结果，兼容性更高，但域名还原和规则命中的稳定性通常弱于 Fake IP，也更依赖上游 DNS 质量。",
      "自定义 DNS：只要 YAML 中存在 dns 段，就拥有最高优先级，界面预设会被锁定。需要配置 nameserver、fallback、fake-ip-filter 等高级项时应使用这种方式。",
    ],
    note: "一般建议：仅使用系统代理可先保持关闭；开启 TUN 时优先选择 Fake IP，遇到特定应用兼容问题再改用 Redir Host 或自定义 DNS。",
  },
  takeover: {
    heading: "本机接管方式的区别",
    paragraphs: [
      "系统代理只修改操作系统的 HTTP、HTTPS 和 SOCKS 代理设置，适合浏览器及遵循系统代理的应用；进程退出时 proxyd 会尝试恢复原状态。",
      "TUN 在网络层接管 TCP/UDP 流量，可覆盖不读取系统代理的程序，但需要管理员或网络管理权限。通常选择系统代理或 TUN 其中一种即可，同时开启不会改变规则优先级。",
      "登录后自动启动只负责在用户登录时启动 proxyd，不会自动提升 TUN 权限；启用 TUN 时仍需按当前平台完成授权。",
    ],
    note: "修改接管方式可能短暂影响现有连接；操作前请确认主端口和规则配置可用。",
  },
  updates: {
    heading: "版本检查会做什么",
    paragraphs: [
      "启用后，proxyd 只在启动阶段异步查询官方 GitHub Releases 的最新稳定版本，并把结果缓存在内存中。Web 轮询不会反复访问 GitHub。",
      "检查失败、网络超时或限流不会影响代理核心、订阅刷新和 API；开发版或无法比较的版本号也不会产生升级误报。",
    ],
    note: "此开关只提供更新提示，不会自动下载、替换或重启当前程序。",
  },
  backup: {
    heading: "备份与导入的安全边界",
    paragraphs: [
      "打码配置会隐藏 secret、订阅凭据和敏感查询参数，适合排障分享；完整备份包含真实订阅和代理凭据，只应保存在可信位置。",
      "导入会先完成格式校验并展示变更摘要，确认时还会校验文件摘要，避免预览后内容被替换。写盘采用临时文件和原子替换，失败不会覆盖现有配置。",
    ],
    note: "导入成功后必须重启 proxyd 才会整体生效，因为监听地址、状态目录和权限要求可能同时变化。",
  },
};

/**
 * SettingsPage 渲染设置页。
 *
 * 参数说明：
 * - forms: object，表单状态。
 * - overview: object，概览数据。
 * - onForm/onPost: Function，表单与提交回调。
 * - onImportConfig: Function，上传配置文件的回调。
 *
 * 返回值说明：
 * 返回设置页 React 元素。
 *
 * 可能的异常/错误情况：
 * 端口格式错误本地拦截；后端校验错误由父组件展示。
 */
function SettingsPage({ forms, overview, onForm, onImportConfig, onPost }) {
  const availableNodes = overview.nodes.filter((node) => node.alive).sort((a, b) => a.delay - b.delay || a.name.localeCompare(b.name));
  return (
    <div className="settings-layout">
      <PageHeader eyebrow="系统配置" title="系统设置" detail="集中管理代理入口、本机网络接管以及配置维护。" />
      <nav className="settings-jump" aria-label="设置分组快速跳转">
        <span><SlidersHorizontal size={16} aria-hidden="true" />快速定位</span>
        <a href="#settings-ports">代理入口</a>
        <a href="#settings-network">本机网络</a>
        <a href="#settings-maintenance">维护与备份</a>
      </nav>

      <section className="settings-section" id="settings-ports" aria-labelledby="settings-ports-title">
        <div className="settings-section-heading">
          <span>01</span>
          <div>
            <h2 id="settings-ports-title">代理入口</h2>
            <p>管理主端口、节点端口范围和自动选优入口。</p>
          </div>
        </div>
        <div className="settings-grid">
          <section className="setting-row">
            <SettingTitle title="主代理入口" detail="应用通常只需要配置这个端口" help={SETTINGS_HELP.mainEntry} />
            <div className="setting-control">
              <div className="form-grid settings-form">
                <Field label="主端口"><input type="number" min="1" max="65535" value={forms.mainPort} onChange={(event) => onForm("mainPort", event.target.value)} /></Field>
                <Button className="form-submit" type="button" onClick={() => onPost("/api/main-port", { port: Number.parseInt(forms.mainPort, 10) }, `主端口已更新为 ${forms.mainPort}`)}>保存端口</Button>
              </div>
              <UISwitch checked={overview.main_auto} label="始终使用当前延迟最低的节点" onCheckedChange={(enabled) => onPost("/api/main-auto", { enabled }, enabled ? "主端口已切换为最优节点" : "主端口已恢复规则模式")} />
              <Field label="固定节点" hint={overview.main_auto ? "关闭自动选择后可固定节点" : "留空时跟随当前规则和模式"}>
                <Select
                  ariaLabel="主端口固定节点"
                  disabled={overview.main_auto}
                  value={overview.main_node || ""}
                  onValueChange={(node) => onPost("/api/main-node", { node }, node ? "主端口已固定到所选节点" : "主端口已恢复规则模式")}
                  options={[
                    { value: "", label: "跟随规则与模式" },
                    ...availableNodes.map((node) => ({ value: node.key, label: `${node.name} · ${formatDelay(node)}` })),
                  ]}
                />
              </Field>
            </div>
          </section>
          <section className="setting-row">
            <SettingTitle title="节点端口范围" detail="健康节点会依次分配到这个范围内" help={SETTINGS_HELP.nodePorts} />
            <div className="setting-control">
              <UISwitch checked={Boolean(overview.port_mapping_enabled)} label="启用节点一对一端口映射" onCheckedChange={(enabled) => onPost("/api/port-mapping", { enabled }, enabled ? "节点端口映射已开启" : "节点端口映射已关闭")} />
              <div className="form-grid settings-form range-form">
                <Field label="起始端口"><input type="number" min="1" max="65535" value={forms.rangeLo} onChange={(event) => onForm("rangeLo", event.target.value)} /></Field>
                <Field label="结束端口"><input type="number" min="1" max="65535" value={forms.rangeHi} onChange={(event) => onForm("rangeHi", event.target.value)} /></Field>
                <Button className="form-submit" type="button" onClick={() => onPost("/api/port-range", { range: `${forms.rangeLo}-${forms.rangeHi}` }, "端口区间已更新")}>保存范围</Button>
              </div>
              <p className="permission-note ok">关闭后保留稳定分配；主端口、自动选优与分组端口继续工作。</p>
            </div>
          </section>
          <section className="setting-row">
            <SettingTitle title="自动选优入口" detail="提供一个始终指向低延迟节点的独立端口" help={SETTINGS_HELP.autoPort} />
            <div className="form-grid settings-form">
              <Field label="端口（0 表示关闭）"><input type="number" min="0" max="65535" value={forms.autoPort} onChange={(event) => onForm("autoPort", event.target.value)} /></Field>
              <Button className="form-submit" type="button" onClick={() => onPost("/api/auto-port", { port: Number.parseInt(forms.autoPort, 10) || 0 }, "自动选优端口已更新")}>保存端口</Button>
            </div>
          </section>
        </div>
      </section>

      <section className="settings-section" id="settings-network" aria-labelledby="settings-network-title">
        <div className="settings-section-heading">
          <span>02</span>
          <div>
            <h2 id="settings-network-title">本机网络</h2>
            <p>配置 DNS、系统代理、TUN 与开机启动。</p>
          </div>
        </div>
        <div className="settings-grid">
          <section className="setting-row">
            <SettingTitle title="DNS 处理" detail="TUN 模式通常与 Fake IP 配合使用" help={SETTINGS_HELP.dns} />
            <div className="setting-control">
              <Field label="DNS 预设">
                <Select
                  ariaLabel="DNS 预设"
                  disabled={overview.dns_custom}
                  value={overview.dns_custom ? "custom" : (overview.dns_preset || "off")}
                  onValueChange={(preset) => onPost("/api/dns-preset", { preset }, `DNS 预设已切换为 ${preset}`)}
                  options={[
                    ...(overview.dns_custom ? [{ value: "custom", label: "使用配置文件中的自定义 DNS" }] : []),
                    { value: "off", label: "不启用 DNS 预设" },
                    { value: "fake-ip", label: "Fake IP" },
                    { value: "redir-host", label: "Redir Host" },
                  ]}
                />
              </Field>
              {overview.dns_custom && <p className="permission-note ok">配置文件中的 DNS 段优先生效</p>}
              {!overview.dns_custom && overview.tun?.enabled && overview.dns_preset === "off" && <p className="permission-note warn">TUN 已开启，建议选择 Fake IP</p>}
            </div>
          </section>
          <section className="setting-row">
            <SettingTitle title="本机接管" detail="这些开关会修改当前设备的网络集成状态" help={SETTINGS_HELP.takeover} />
            <div className="setting-control switch-stack">
              <UISwitch checked={overview.system_proxy} label="接管系统代理" onCheckedChange={(enabled) => onPost("/api/system-proxy", { enabled }, enabled ? "系统代理已开启" : "系统代理已关闭")} />
              <UISwitch checked={Boolean(overview.tun?.enabled)} label="启用 TUN 模式" onCheckedChange={(enabled) => onPost("/api/tun", { enabled }, enabled ? "TUN 已开启" : "TUN 已关闭")} />
              {overview.tun && <p className={classNames("permission-note", overview.tun.allowed && (!overview.tun.enabled || overview.tun.active) ? "ok" : "warn")}>{overview.tun.enabled && !overview.tun.active ? "TUN 配置已开启但实际未生效，请检查日志" : overview.tun.allowed ? `${overview.tun.platform} 权限可用` : overview.tun.permission}</p>}
              <UISwitch checked={overview.autostart} label="登录后自动启动 proxyd" onCheckedChange={(enabled) => onPost("/api/autostart", { enabled }, enabled ? "开机自启已开启" : "开机自启已关闭")} />
            </div>
          </section>
        </div>
      </section>

      <section className="settings-section" id="settings-maintenance" aria-labelledby="settings-maintenance-title">
        <div className="settings-section-heading">
          <span>03</span>
          <div>
            <h2 id="settings-maintenance-title">维护与备份</h2>
            <p>控制版本检查，并安全导入或导出配置。</p>
          </div>
        </div>
        <div className="settings-grid">
          <section className="setting-row">
            <SettingTitle title="版本检查" detail="仅在启动时检查稳定版本，不影响代理运行" help={SETTINGS_HELP.updates} />
            <div className="setting-control">
              <UISwitch checked={Boolean(overview.version_check?.enabled)} label="启动时检查新版本" onCheckedChange={(enabled) => onPost("/api/update-check", { enabled }, enabled ? "版本检查已开启" : "版本检查已关闭")} />
              {overview.version_check && <p className={classNames("permission-note", overview.version_check.state === "failed" ? "warn" : "ok")}>{versionCheckMessage(overview.version_check)}</p>}
            </div>
          </section>
          <section className="setting-row">
            <SettingTitle title="配置备份" detail="分享配置时使用打码导出；完整备份包含敏感信息" help={SETTINGS_HELP.backup} />
            <div className="config-actions">
              <ButtonLink className="beui-link-button" href="/api/config/export" variant="outline" size="md" download><Download size={16} aria-hidden="true" />导出打码配置</ButtonLink>
              <ButtonLink className="beui-link-button" href="/api/config/export?mask_tokens=false" variant="outline" size="md" download><Download size={16} aria-hidden="true" />下载完整备份</ButtonLink>
              <label className="beui-link-button config-upload"><Upload size={16} aria-hidden="true" />导入配置<input accept=".yaml,.yml,application/yaml,text/yaml" type="file" onChange={(event) => { onImportConfig(event.target.files?.[0]); event.target.value = ""; }} /></label>
            </div>
          </section>
        </div>
      </section>
    </div>
  );
}

/**
 * ConnectionsPage 渲染活动连接页。
 *
 * 功能说明：
 * 该页面把“摘要、筛选、桌面表格、移动卡片、关闭操作”放在一屏里，目标是让普通用户
 * 先看懂哪里在连、连到了哪、为什么还在跑，再决定是否关闭单条或关闭全部。
 *
 * 参数说明：
 * - activeCount: number，活动连接总数。
 * - closingAll: boolean，是否正在执行关闭全部。
 * - error: string，最近一次加载错误文本。
 * - hasLoaded: boolean，是否至少完成过一次加载尝试。
 * - loading: boolean，是否处于首次加载。
 * - paused: boolean，是否暂停自动轮询。
 * - pendingIds: Set<string>，正在关闭中的连接 id 集合。
 * - query: string，搜索词。
 * - refreshing: boolean，是否正在刷新列表。
 * - retryConnections: Function，重新加载列表。
 * - rows: Array<object>，全部连接模型。
 * - summary: object，活动数、累计流量和内存摘要。
 * - transport: string，当前协议筛选值。
 * - updatedAt: Date | null，最近一次刷新时间。
 * - visibleRows: Array<object>，当前筛选后可见的连接。
 * - setPaused/setQuery/setTransport: Function，暂停、搜索与协议筛选 setter。
 * - closeAllConnections/closeConnection: Function，关闭全部与关闭单条动作。
 *
 * 返回值说明：
 * 返回活动连接页 React 元素。
 *
 * 可能的异常/错误情况：
 * - 列表为空或筛选无结果时渲染空状态。
 * - 加载失败时保留旧数据并显示错误条带，用户可点击重试。
 */
function ConnectionsPage({
  activeCount,
  closingAll,
  error,
  hasLoaded,
  loading,
  paused,
  pendingIds,
  query,
  refreshing,
  retryConnections,
  rows,
  summary,
  transport,
  updatedAt,
  visibleRows,
  setPaused,
  setQuery,
  setTransport,
  closeAllConnections,
  closeConnection,
}) {
  const initialLoading = loading && !hasLoaded && rows.length === 0;
  const updatedText = updatedAt ? updatedAt.toLocaleTimeString("zh-CN", { hour12: false }) : "—";
  const visibleCount = visibleRows.length;
  const hasRows = rows.length > 0;
  const hasVisibleRows = visibleRows.length > 0;
  const memoryValue = summary.memory ?? "—";
  const memoryFormatter = typeof memoryValue === "number" ? formatBytes : undefined;

  const emptyTitle = error && !hasRows
    ? "连接列表加载失败"
    : hasRows && !hasVisibleRows
      ? "没有匹配的连接"
      : "当前没有活动连接";
  const emptyDetail = error && !hasRows
    ? `${error}，点击重试重新加载。`
    : hasRows && !hasVisibleRows
      ? "当前筛选条件下没有匹配的连接。"
      : "当代理开始接入流量后，这里会显示域名、入口、出口链和开始时间。";

  const connectionColumns = useMemo(
    () => [
      {
        key: "targetLabel",
        header: "目标域名/地址",
        sortable: true,
        width: "28%",
        cell: (row) => (
          <div className="connections-cell-stack">
            <div className="flex min-w-0 flex-wrap items-center gap-2">
              <span className="min-w-0 break-words font-medium text-foreground">{row.targetLabel}</span>
              <Badge variant="outline">{row.protocolLabel}</Badge>
            </div>
            {row.targetDetail && <small>{row.targetDetail}</small>}
          </div>
        ),
      },
      {
        key: "entryLabel",
        header: "入口与来源",
        sortable: true,
        width: "19%",
        cell: (row) => (
          <div className="connections-cell-stack">
            <span><b>入口：</b>{row.entryLabel}</span>
            <small><b>来源：</b>{row.sourceLabel}</small>
          </div>
        ),
        sortValue: (row) => `${row.entryLabel} ${row.sourceLabel}`,
      },
      {
        key: "exitLabel",
        header: "出口链",
        sortable: true,
        width: "20%",
        cell: (row) => <span className="connections-chain-text">{row.exitLabel}</span>,
      },
      {
        key: "totalBytes",
        header: "累计流量",
        sortable: true,
        width: "150px",
        cell: (row) => (
          <div className="connections-cell-stack">
            <span>{formatBytes(row.totalBytes)}</span>
            <small>↑ {formatBytes(row.uploadBytes)} / ↓ {formatBytes(row.downloadBytes)}</small>
          </div>
        ),
      },
      {
        key: "startedLabel",
        header: "开始时间",
        sortable: true,
        width: "124px",
        cell: (row) => <time className="connections-time-cell" dateTime={row.startedDateTime || undefined}>{row.startedLabel}</time>,
        sortValue: (row) => row.startedDateTime || row.startedLabel,
      },
      {
        key: "close",
        header: "关闭",
        align: "center",
        width: "84px",
        cell: (row) => {
          const isPending = pendingIds.has(row.closeId);
          return (
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  aria-label={`关闭连接 ${row.targetLabel}`}
                  disabled={!row.closeable || isPending}
                  loading={isPending}
                  size="icon"
                  type="button"
                  variant="destructive-ghost"
                  onClick={() => closeConnection(row)}
                >
                  {!isPending && <X size={16} aria-hidden="true" />}
                </Button>
              </TooltipTrigger>
              <TooltipContent>关闭连接</TooltipContent>
            </Tooltip>
          );
        },
      },
    ],
    [closeConnection, pendingIds],
  );

  return (
    <section
      aria-busy={loading || refreshing}
      aria-label="活动连接"
      className="connections-page grid gap-4"
    >
      {(loading || refreshing) && <span className="sr-only" role="status">正在更新活动连接</span>}
      <PageHeader eyebrow="运行监测" title="活动连接" detail="查看实时流量、出口链与连接来源，并可按协议快速筛选。">
        <div className="page-actions">
          <div className="connection-refresh-state">
            <div>
              {refreshing && <Badge variant="secondary">更新中</Badge>}
              {paused && <Badge variant="outline">已暂停</Badge>}
              <Badge variant="outline">{visibleCount}/{activeCount}</Badge>
            </div>
            <p>
            <Clock3 size={14} aria-hidden="true" />
            <span>刷新于</span>
            <time dateTime={updatedAt?.toISOString() || undefined}>{updatedText}</time>
            </p>
          </div>
          <Button
            type="button"
            variant="outline"
            onClick={() => setPaused(!paused)}
            aria-label={paused ? "继续自动刷新活动连接" : "暂停自动刷新活动连接"}
          >
            {paused ? <Play size={16} aria-hidden="true" /> : <Pause size={16} aria-hidden="true" />}
            <span>{paused ? "继续" : "暂停"}</span>
          </Button>
          <Button
            disabled={loading}
            loading={refreshing}
            type="button"
            variant="outline"
            onClick={retryConnections}
            aria-label="刷新活动连接"
          >
            <RefreshCw className={classNames(refreshing && "animate-spin")} size={16} aria-hidden="true" />
            <span>刷新</span>
          </Button>
          <Button
            className="border-red-200 text-red-700 hover:bg-red-50 hover:text-red-800"
            disabled={!hasRows || closingAll}
            loading={closingAll}
            type="button"
            variant="outline"
            onClick={closeAllConnections}
            aria-label="关闭全部活动连接"
          >
            <X size={16} aria-hidden="true" />
            <span>关闭全部</span>
          </Button>
        </div>
      </PageHeader>

      {error && (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-md border border-warning/30 bg-warning-soft px-4 py-3 text-sm text-warning">
          <div className="flex min-w-0 items-start gap-2">
            <CircleAlert size={16} aria-hidden="true" />
            <span className="min-w-0 break-words">{error}</span>
          </div>
          <Button className="h-11" type="button" variant="outline" onClick={retryConnections} aria-label="重试加载活动连接">
            <RefreshCw size={16} aria-hidden="true" />
            <span>重试</span>
          </Button>
        </div>
      )}

      {initialLoading ? (
        <ConnectionsSkeleton />
      ) : (
        <>
          <div className="grid grid-cols-2 gap-3 xl:grid-cols-4">
            <Metric
              detail={visibleCount === activeCount ? "当前筛选显示全部连接" : `当前筛选显示 ${visibleCount} 条`}
              label="活动连接"
              value={summary.activeCount}
            />
            <Metric
              detail="当前连接快照"
              format={formatBytes}
              label="累计上行"
              value={summary.uploadBytes}
            />
            <Metric
              detail="当前连接快照"
              format={formatBytes}
              label="累计下行"
              value={summary.downloadBytes}
            />
            <Metric
              detail="mihomo 运行占用"
              format={memoryFormatter}
              label="内存"
              value={memoryValue}
            />
          </div>

          <section className="panel">
            <PanelTitle title="筛选与搜索" />
            <div className="grid gap-3 xl:grid-cols-[minmax(0,1fr)_auto] xl:items-end">
              <Field label="搜索">
                <Input
                  aria-label="搜索活动连接"
                  className="h-11"
                  placeholder="搜索域名 / IP / 进程 / 节点链"
                  value={query}
                  onChange={(event) => setQuery(event.target.value)}
                />
              </Field>
              <Field compact label="协议筛选">
                <SegmentedControl
                  ariaLabel="连接协议筛选"
                  onValueChange={setTransport}
                  options={[
                    { value: "all", label: "全部" },
                    { value: "tcp", label: "TCP" },
                    { value: "udp", label: "UDP" },
                  ]}
                  value={transport}
                />
              </Field>
            </div>
          </section>

          {!hasVisibleRows ? (
            <ConnectionsEmptyState detail={emptyDetail} onRetry={error ? retryConnections : null} title={emptyTitle} />
          ) : (
            <>
              <Table
                className="data-table hidden rounded-xl shadow-sm md:block"
                columns={connectionColumns}
                data={visibleRows}
                emptyState="暂无活动连接"
                getRowId={(row) => row.id}
                height={tableViewportHeight(visibleRows.length, 624)}
                minColumnWidth={88}
                resizable
              />

              <div className="grid gap-3 md:hidden">
                {visibleRows.map((row) => {
                  const isPending = pendingIds.has(row.closeId);
                  return (
                    <article className="grid gap-4 rounded-md border bg-card p-4 shadow-sm" key={row.id}>
                      <div className="flex items-start justify-between gap-3">
                        <div className="min-w-0">
                          <div className="flex flex-wrap items-center gap-2">
                            <h3 className="min-w-0 break-words text-sm font-semibold text-foreground">{row.targetLabel}</h3>
                            <Badge variant="outline">{row.protocolLabel}</Badge>
                          </div>
                          {row.targetDetail && <p className="mt-1 break-words text-xs leading-5 text-muted-foreground">{row.targetDetail}</p>}
                        </div>
                        <Button
                          aria-label={`关闭连接 ${row.targetLabel}`}
                          className="h-11 w-11"
                          disabled={!row.closeable || isPending}
                          loading={isPending}
                          size="icon"
                          type="button"
                          variant="destructive-ghost"
                          onClick={() => closeConnection(row)}
                        >
                          <X size={16} aria-hidden="true" />
                        </Button>
                      </div>

                      <dl className="grid gap-3 text-sm">
                        <div className="grid gap-1">
                          <dt className="text-xs font-medium text-muted-foreground">入口与来源</dt>
                          <dd className="break-words text-foreground">
                            <span className="font-medium">入口：</span>{row.entryLabel}
                            <br />
                            <span className="font-medium">来源：</span>{row.sourceLabel}
                          </dd>
                        </div>
                        <div className="grid gap-1">
                          <dt className="text-xs font-medium text-muted-foreground">出口链</dt>
                          <dd className="break-words text-foreground">{row.exitLabel}</dd>
                        </div>
                        <div className="grid gap-1">
                          <dt className="text-xs font-medium text-muted-foreground">累计流量</dt>
                          <dd className="text-foreground">
                            {formatBytes(row.totalBytes)}
                            <span className="mt-1 block text-xs text-muted-foreground">↑ {formatBytes(row.uploadBytes)} / ↓ {formatBytes(row.downloadBytes)}</span>
                          </dd>
                        </div>
                        <div className="grid gap-1">
                          <dt className="text-xs font-medium text-muted-foreground">开始时间</dt>
                          <dd><time dateTime={row.startedDateTime || undefined}>{row.startedLabel}</time></dd>
                        </div>
                      </dl>
                    </article>
                  );
                })}
              </div>
            </>
          )}
        </>
      )}
    </section>
  );
}

/**
 * ConnectionsSkeleton 渲染活动连接页的首次加载骨架。
 *
 * 功能说明：
 * 首次进入页面时，连接数据通常还没回来。这里不用空白页面，而是给用户一个和
 * 实际布局相近的骨架，让他们知道页面在加载什么内容，同时避免等待时发生布局跳变。
 *
 * 参数说明：
 * 无。
 *
 * 返回值说明：
 * 返回骨架屏 React 元素。
 *
 * 可能的异常/错误情况：
 * 无；仅用于表现层。
 */
function ConnectionsSkeleton() {
  return (
    <div className="grid gap-4" aria-hidden="true">
      <div className="grid grid-cols-2 gap-3 xl:grid-cols-4">
        {Array.from({ length: 4 }).map((_, index) => (
          <div className="grid min-h-[112px] gap-3 rounded-md border bg-card p-4" key={index}>
            <div className="h-3 w-20 animate-pulse rounded-full bg-muted" />
            <div className="h-9 w-28 animate-pulse rounded-md bg-muted" />
            <div className="h-3 w-32 animate-pulse rounded-full bg-muted" />
          </div>
        ))}
      </div>
      <section className="panel">
        <div className="grid gap-3 xl:grid-cols-[minmax(0,1fr)_auto] xl:items-end">
          <div className="grid gap-2">
            <div className="h-3 w-24 animate-pulse rounded-full bg-muted" />
            <div className="h-11 w-full animate-pulse rounded-md bg-muted" />
          </div>
          <div className="grid gap-2">
            <div className="h-3 w-20 animate-pulse rounded-full bg-muted" />
            <div className="h-11 w-[300px] max-w-full animate-pulse rounded-md bg-muted" />
          </div>
        </div>
      </section>
      <div className="hidden gap-0 overflow-hidden rounded-md border bg-card md:block">
        <div className="grid gap-0">
          {Array.from({ length: 5 }).map((_, index) => (
            <div className="grid grid-cols-6 gap-4 border-b px-4 py-4 last:border-b-0" key={index}>
              {Array.from({ length: 6 }).map((__, columnIndex) => (
                <div className="h-4 w-full animate-pulse rounded-full bg-muted" key={columnIndex} />
              ))}
            </div>
          ))}
        </div>
      </div>
      <div className="grid gap-3 md:hidden">
        {Array.from({ length: 3 }).map((_, index) => (
          <article className="grid gap-3 rounded-md border bg-card p-4" key={index}>
            <div className="flex items-start justify-between gap-3">
              <div className="grid gap-2">
                <div className="h-4 w-40 animate-pulse rounded-full bg-muted" />
                <div className="h-3 w-24 animate-pulse rounded-full bg-muted" />
              </div>
              <div className="h-11 w-11 animate-pulse rounded-md bg-muted" />
            </div>
            <div className="grid gap-2">
              <div className="h-3 w-28 animate-pulse rounded-full bg-muted" />
              <div className="h-3 w-48 animate-pulse rounded-full bg-muted" />
              <div className="h-3 w-36 animate-pulse rounded-full bg-muted" />
            </div>
          </article>
        ))}
      </div>
    </div>
  );
}

/**
 * ConnectionsEmptyState 渲染活动连接页的空状态。
 *
 * 功能说明：
 * 空状态需要区分两种情况：完全没有活动连接，和筛选条件把结果过滤空了。
 * 如果是加载失败，这里还能提供一个明确的重试入口，减少用户在错误条带和页面
 * 中间来回寻找操作按钮的负担。
 *
 * 参数说明：
 * - title: string，空状态标题。
 * - detail: string，空状态说明。
 * - onRetry: Function | null，是否提供重试按钮。
 *
 * 返回值说明：
 * 返回空状态 React 元素。
 *
 * 可能的异常/错误情况：
 * 无；按钮缺省时只展示说明文本。
 */
function ConnectionsEmptyState({ title, detail, onRetry }) {
  return (
    <section className="empty-state">
      <Link2 size={28} />
      <h2>{title}</h2>
      <p>{detail}</p>
      {onRetry && (
        <Button className="mt-4 h-11" type="button" variant="outline" onClick={onRetry} aria-label="重试加载活动连接">
          <RefreshCw size={16} aria-hidden="true" />
          <span>重试</span>
        </Button>
      )}
    </section>
  );
}

/**
 * Metric 渲染指标卡。
 *
 * 功能说明：
 * 这个组件已经在概览页里承担了“快速扫一眼数字”的职责，因此连接页继续复用它，
 * 只在字节类指标上通过可选 `format` 控制展示单位，避免重复造另一套统计卡。
 *
 * 参数说明：
 * - label: string，指标名。
 * - value: string | number，指标值。
 * - detail: string，补充信息。
 * - format: Function | undefined，数字格式化函数；仅在 `value` 是数字时生效。
 *
 * 返回值说明：
 * 返回指标卡 React 元素。
 *
 * 可能的异常/错误情况：
 * - `format` 抛错会继续向外传播，方便快速定位格式化逻辑问题。
 */
function Metric({ label, value, detail, format }) {
  return (
    <section className="metric">
      <span>{label}</span>
      <strong className="tabular-nums">{typeof value === "number" ? (format ? format(value) : Math.round(value).toString()) : value}</strong>
      <small>{detail}</small>
    </section>
  );
}

/**
 * SettingTitle 渲染带详细帮助入口的系统设置标题。
 *
 * 功能说明：
 * 保留设置卡片原有的标题与摘要层级，并使用 Radix Tooltip 提供更完整的用途、
 * 优先级和风险边界说明。触发按钮同时支持 hover、键盘聚焦和触屏点击。
 *
 * 参数说明：
 * - title: string，设置卡片标题。
 * - detail: string，卡片内常驻显示的简短摘要。
 * - help: object，详细帮助模型，包含 heading、paragraphs、items 和 note。
 *
 * 返回值说明：
 * 返回包含标题、帮助触发按钮、摘要和 TooltipContent 的 React 元素。
 *
 * 可能的异常/错误情况：
 * help 缺失时仅渲染普通标题与摘要；帮助段落或列表为空时自动省略对应区块，
 * 不影响设置控件的提交和状态更新。
 */
function SettingTitle({ title, detail, help }) {
  return (
    <div className="panel-heading setting-heading">
      <div className="setting-title-line">
        <h2 className="panel-title">{title}</h2>
        {help && (
          <Tooltip delayDuration={120}>
            <TooltipTrigger asChild>
              <button className="setting-help-trigger" type="button" aria-label={`查看“${title}”详细说明`}>
                <CircleHelp size={16} aria-hidden="true" />
              </button>
            </TooltipTrigger>
            <TooltipContent className="setting-help-content" side="right" sideOffset={8}>
              <div className="setting-help-body">
                <strong>{help.heading}</strong>
                {(help.paragraphs || []).map((paragraph) => <p key={paragraph}>{paragraph}</p>)}
                {(help.items || []).length > 0 && (
                  <ul>
                    {help.items.map((item) => <li key={item}>{item}</li>)}
                  </ul>
                )}
                {help.note && <p className="setting-help-note">{help.note}</p>}
              </div>
            </TooltipContent>
          </Tooltip>
        )}
      </div>
      {detail && <p>{detail}</p>}
    </div>
  );
}

/**
 * PageHeader 为所有桌面业务页面提供统一的标题、说明和操作区。
 *
 * 功能说明：
 * 使用固定的信息层级替代各页面自行拼装的标题栏，避免标题字号、上下间距和
 * 操作按钮位置在页面切换时跳动。eyebrow 负责建立模块语境，children 只承载
 * 当前页面最重要的操作，次要操作继续留在对应内容面板内。
 *
 * 参数说明：
 * - eyebrow: string，页面所属功能分组的短标签。
 * - title: string，页面唯一的一级标题。
 * - detail: string，说明页面职责和数据边界的简短文本。
 * - children: React.ReactNode，可选的页面级操作控件。
 *
 * 返回值说明：
 * 返回语义化 header React 元素。
 *
 * 可能的异常/错误情况：
 * 无；缺失 eyebrow、detail 或 children 时自动省略对应区域，不影响页面主体。
 */
function PageHeader({ eyebrow, title, detail, children }) {
  return (
    <header className="page-header">
      <div className="page-header-copy">
        {eyebrow && <span className="page-kicker">{eyebrow}</span>}
        <h1>{title}</h1>
        {detail && <p>{detail}</p>}
      </div>
      {children && <div className="page-header-actions">{children}</div>}
    </header>
  );
}

/**
 * PanelTitle 渲染面板标题。
 *
 * 参数说明：
 * - title: string，标题文本。
 * - detail: string，可选的当前区块说明，用于补充状态或业务边界。
 *
 * 返回值说明：
 * 返回标题 React 元素。
 *
 * 可能的异常/错误情况：
 * 无。
 */
function PanelTitle({ title, detail }) {
  return (
    <div className="panel-heading">
      <h2 className="panel-title">{title}</h2>
      {detail && <p>{detail}</p>}
    </div>
  );
}

/**
 * Field 为表单控件提供不会随输入内容消失的标签和辅助状态。
 *
 * 参数说明：
 * - label: string，字段的固定中文名称。
 * - hint: string，可选的格式、边界或当前状态说明。
 * - compact: boolean，是否使用工具栏中的紧凑排版。
 * - children: React.ReactNode，实际输入、选择器或其他表单控件。
 *
 * 返回值说明：
 * 返回包含可点击标签、控件和辅助文本的 React 元素。
 *
 * 可能的异常/错误情况：
 * 无；输入校验和提交错误仍由对应业务表单及 API 调用处理。
 */
function Field({ label, hint, compact = false, children }) {
  return (
    <label className={classNames("field", compact && "compact")}>
      <span>{label}</span>
      {children}
      {hint && <small>{hint}</small>}
    </label>
  );
}

/**
 * PortChip 渲染可复制端口 chip。
 *
 * 参数说明：
 * - label: string，端口说明。
 * - port: number，端口号。
 * - tone: string，视觉类型。
 * - onCopy: Function，复制回调。
 *
 * 返回值说明：
 * 返回端口 chip React 元素。
 *
 * 可能的异常/错误情况：
 * 复制失败由父组件处理。
 */
function PortChip({ label, port, tone, onCopy }) {
  return (
    <button className={classNames("port-chip", tone)} type="button" onClick={() => onCopy(port)}>
      <span>{port}</span>
      <small>{label}</small>
      <Copy size={14} />
    </button>
  );
}

/**
 * StatusBadge 渲染状态徽标。
 *
 * 参数说明：
 * - ok: boolean，是否为正常状态。
 * - text: string，展示文本。
 *
 * 返回值说明：
 * 返回状态徽标 React 元素。
 *
 * 可能的异常/错误情况：
 * 无。
 */
function StatusBadge({ ok, text }) {
  return <Badge variant={ok ? "success" : "destructive"}>{text}</Badge>;
}

/**
 * EmptyState 渲染空状态。
 *
 * 参数说明：
 * - title: string，主文案。
 * - detail: string，补充文案。
 *
 * 返回值说明：
 * 返回空状态 React 元素。
 *
 * 可能的异常/错误情况：
 * 无。
 */
function EmptyState({ title, detail }) {
  return (
    <section className="empty-state">
      <Zap size={28} />
      <h2>{title}</h2>
      <p>{detail}</p>
    </section>
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
