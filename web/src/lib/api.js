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
export async function requestJSON(url, options = {}) {
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
export async function requestText(url, options = {}) {
  const response = await fetch(url, options);
  const text = await response.text();
  if (!response.ok) {
    throw new Error(text.trim() || `HTTP ${response.status}`);
  }
  return text;
}
