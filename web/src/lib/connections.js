import { normalizeText } from "@/lib/format";

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
export function normalizeConnectionsResponse(payload) {
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
