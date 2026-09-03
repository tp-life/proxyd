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
export function classNames(...items) {
  return items.filter(Boolean).join(" ");
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
export function formatDelay(item) {
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
export function formatBytes(value) {
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
export function formatExpire(expire) {
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
export function formatUserInfo(info) {
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
export function delayClass(item) {
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
export function sortByDelay(list, mode) {
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
 * 官方 Table 需要明确 viewport 高度才能虚拟滚动。高度随行数增长，数据较少时
 * 收缩避免大块空白；数据较多时封顶并启用滚动。封顶取页面给定上限与当前视口
 * 可用高度（视口高减去页头/工具栏约 260px 的留白）的较小值，这样大屏能利用
 * 更多空间，小屏也不会把页面撑出嵌套滚动条。
 *
 * 参数说明：
 * - rowCount: number，当前数据行数。
 * - maximum: number，可视区域最大高度，单位像素。
 *
 * 返回值说明：
 * 返回包含表头在内的 viewport 像素高度。
 *
 * 可能的异常/错误情况：
 * 非法或负行数按 0 处理；maximum 非法时使用 720px；视口高度不可用时退回 maximum。
 */
export function tableViewportHeight(rowCount, maximum = 720) {
  const safeCount = Number.isFinite(rowCount) && rowCount > 0 ? Math.floor(rowCount) : 0;
  const safeMaximum = Number.isFinite(maximum) && maximum >= 288 ? maximum : 720;
  const viewportCap =
    typeof window !== "undefined" && window.innerHeight
      ? Math.max(320, window.innerHeight - 260)
      : safeMaximum;
  const cap = Math.min(safeMaximum, viewportCap);
  return Math.min(cap, Math.max(288, (safeCount + 1) * 48));
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
export function proxyURL(listen, port) {
  return `http://${listen || "127.0.0.1"}:${port}`;
}

/**
 * proxyEnvCommands 生成指向指定端口的 shell 代理环境变量文本。
 *
 * 参数说明：
 * - listen: string，监听地址；缺省按 127.0.0.1。
 * - port: number，目标端口。
 *
 * 返回值说明：
 * 返回单行 export 命令，可直接粘贴到终端一次生效。
 *
 * 可能的异常/错误情况：
 * 无；纯字符串拼接。mixed 端口同时支持 HTTP 与 SOCKS5，http(s)_proxy 用 http scheme、
 * all_proxy 用 socks5 scheme，覆盖常见 CLI 工具的读取习惯。
 */
export function proxyEnvCommands(listen, port) {
  const host = listen || "127.0.0.1";
  return `export https_proxy=http://${host}:${port} http_proxy=http://${host}:${port} all_proxy=socks5://${host}:${port}`;
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
export function normalizeText(value) {
  if (value === null || value === undefined) return "";
  return String(value).trim();
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
export function versionCheckMessage(status) {
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
 * maskRemoteSecret 把 token 类长文本收敛成摘要展示。
 *
 * 功能说明：
 * 远程连接模块里的 token 长达 95+ 字符，整行铺开会破坏表格布局并泄露凭据。
 * 服务端通常已返回打码摘要，但转发配置里的 remote 字段可能仍是完整 token，
 * 这里统一做一次前端兜底截断。
 *
 * 参数说明：
 * - value: unknown，待展示的文本。
 *
 * 返回值说明：
 * 返回可安全展示的摘要字符串；超过 24 字符时保留首尾各 6 字符。
 *
 * 可能的异常/错误情况：
 * 无；空值返回占位短横线。
 */
export function maskRemoteSecret(value) {
  const text = normalizeText(value);
  if (!text) return "-";
  if (text.length > 24) return `${text.slice(0, 6)}…${text.slice(-6)}`;
  return text;
}
