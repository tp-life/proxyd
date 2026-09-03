import { useCallback, useEffect, useRef, useState } from "react";
import { requestJSON } from "@/lib/api";

/**
 * useRemoteFeed 接入远程连接页的加载与管理能力。
 *
 * 功能说明：
 * 这个 hook 只在 `activeView === "remote"` 时工作，统一维护 `/api/remote`（服务状态、
 * 暴露端口与本地转发）和 `/api/remote/remotes`（远程设备列表）两份数据。加载策略与
 * 活动连接页一致：进入页面加载一次，之后由刷新按钮或写操作触发重新拉取，不做后台
 * 轮询，避免隐藏页签持续请求。
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
 * 接口失败时保留上一份数据并把错误文本交给页面错误条带；写操作失败时 toast 后端
 * 返回的纯文本错误。
 */
export function useRemoteFeed(activeView, requestConfirmation, showToast) {
  const [status, setStatus] = useState(null);
  const [remotes, setRemotes] = useState([]);
  const [loading, setLoading] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState("");
  const [hasLoaded, setHasLoaded] = useState(false);
  const requestControllerRef = useRef(null);
  const requestTokenRef = useRef(0);
  const hasLoadedRef = useRef(false);

  /**
   * loadRemote 拉取远程连接服务状态与远程设备列表。
   *
   * 功能说明：
   * 两个接口互为补充：`/api/remote` 描述本机服务与转发，`/api/remote/remotes` 描述
   * 已保存的对端。每次拉取前取消上一轮请求，保证手动刷新与写操作后的重拉不会互相
   * 覆盖。
   *
   * 参数说明：
   * 无。
   *
   * 返回值说明：
   * 返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * - 请求被新的加载或页面切换中断时，使用 AbortError 静默退出。
   * - 上游 4xx/5xx 或网络失败时保留旧数据，并把错误文本展示到错误条带。
   */
  const loadRemote = useCallback(async () => {
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
      const [nextStatus, nextRemotes] = await Promise.all([
        requestJSON("/api/remote", { signal: controller.signal }),
        requestJSON("/api/remote/remotes", { signal: controller.signal }),
      ]);
      if (requestTokenRef.current !== requestToken) {
        return;
      }
      setStatus(nextStatus);
      setRemotes(nextRemotes?.remotes || []);
      setError("");
      hasLoadedRef.current = true;
      setHasLoaded(true);
    } catch (loadError) {
      if (loadError?.name === "AbortError") {
        return;
      }
      if (requestTokenRef.current !== requestToken) {
        return;
      }
      hasLoadedRef.current = true;
      setHasLoaded(true);
      setError(loadError.message || "远程连接状态加载失败");
    } finally {
      if (requestTokenRef.current === requestToken) {
        setLoading(false);
        setRefreshing(false);
      }
    }
  }, []);

  useEffect(() => {
    if (activeView !== "remote") {
      requestControllerRef.current?.abort();
      return undefined;
    }

    // 与活动连接页一致：进入页面时加载一次，之后由刷新按钮或写操作触发。
    loadRemote();
    return () => {
      requestControllerRef.current?.abort();
    };
  }, [activeView, loadRemote]);

  /**
   * toggleEnabled 开关远程连接服务。
   *
   * 参数说明：
   * - enabled: boolean，目标开关状态。
   *
   * 返回值说明：
   * 返回 Promise<void>；成功后直接用响应体（与 GET 同构）更新状态。
   *
   * 可能的异常/错误情况：
   * 后端拒绝或网络失败时 toast 错误，开关保持原状态。
   */
  const toggleEnabled = useCallback(
    async (enabled) => {
      try {
        const payload = await requestJSON("/api/remote", {
          method: "POST",
          body: JSON.stringify({ enabled }),
        });
        if (payload) setStatus(payload);
        showToast(enabled ? "远程连接服务已开启" : "远程连接服务已关闭");
      } catch (toggleError) {
        showToast(`操作失败：${toggleError.message}`, "err");
      }
    },
    [showToast],
  );

  /**
   * copyToken 获取完整本机 token 并写入剪贴板。
   *
   * 功能说明：
   * 列表接口只返回打码摘要，复制前必须显式请求 `/api/remote/token` 取完整值，
   * 保证页面上永远不出现完整 token 的常驻渲染。
   *
   * 参数说明：
   * 无。
   *
   * 返回值说明：
   * 返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 接口失败或剪贴板权限被拒绝时 toast 错误。
   */
  const copyToken = useCallback(async () => {
    try {
      const payload = await requestJSON("/api/remote/token");
      await navigator.clipboard.writeText(payload?.token || "");
      showToast("token 已复制");
    } catch (copyError) {
      showToast(`复制失败：${copyError.message}`, "err");
    }
  }, [showToast]);

  /**
   * saveServe 整体替换暴露端口列表。
   *
   * 参数说明：
   * - ports: number[]，目标端口列表；添加与删除都先由页面算出新列表再整体提交。
   *
   * 返回值说明：
   * 返回 Promise<boolean>；成功为 `true`。
   *
   * 可能的异常/错误情况：
   * 后端校验失败或网络失败时 toast 错误并返回 false。
   */
  const saveServe = useCallback(
    async (ports) => {
      try {
        const payload = await requestJSON("/api/remote/serve", {
          method: "POST",
          body: JSON.stringify({ ports }),
        });
        if (payload) setStatus(payload);
        showToast("暴露端口已更新");
        return true;
      } catch (saveError) {
        showToast(`操作失败：${saveError.message}`, "err");
        return false;
      }
    },
    [showToast],
  );

  /**
   * addRemote 新增一个远程设备。
   *
   * 参数说明：
   * - name: string，设备名称。
   * - token: string，对端完整 token。
   *
   * 返回值说明：
   * 返回 Promise<boolean>；成功后重新拉取列表。
   *
   * 可能的异常/错误情况：
   * 后端返回 400（如 token 格式非法或名称重复）时 toast 纯文本错误并返回 false。
   */
  const addRemote = useCallback(
    async (name, token) => {
      try {
        await requestJSON("/api/remote/remotes", {
          method: "POST",
          body: JSON.stringify({ name, token }),
        });
        showToast(`远程设备 ${name} 已添加`);
        await loadRemote();
        return true;
      } catch (addError) {
        showToast(`添加失败：${addError.message}`, "err");
        return false;
      }
    },
    [loadRemote, showToast],
  );

  /**
   * removeRemote 经确认后删除远程设备。
   *
   * 参数说明：
   * - name: string，设备名称。
   *
   * 返回值说明：
   * 返回 Promise<boolean>；用户取消时返回 false。
   *
   * 可能的异常/错误情况：
   * 删除失败时 toast 错误并保留现有列表。
   */
  const removeRemote = useCallback(
    async (name) => {
      const accepted = await requestConfirmation({
        title: `删除远程设备 ${name}？`,
        description: "删除后指向该设备的转发将失效，已保存的 token 无法从控制台恢复。",
        confirmLabel: "确认删除",
        destructive: true,
      });
      if (!accepted) return false;
      try {
        await requestJSON(`/api/remote/remotes/${encodeURIComponent(name)}`, { method: "DELETE" });
        showToast(`远程设备 ${name} 已删除`);
        await loadRemote();
        return true;
      } catch (removeError) {
        showToast(`删除失败：${removeError.message}`, "err");
        return false;
      }
    },
    [loadRemote, requestConfirmation, showToast],
  );

  /**
   * copyText 把任意文本写入剪贴板并 toast 结果。
   *
   * 参数说明：
   * - text: string，待复制文本。
   * - message: string，成功时的 toast 文案。
   *
   * 返回值说明：
   * 返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 剪贴板权限被拒绝时 toast 错误。
   */
  const copyText = useCallback(
    async (text, message = "已复制") => {
      try {
        await navigator.clipboard.writeText(text);
        showToast(message);
      } catch (copyError) {
        showToast(`复制失败：${copyError.message}`, "err");
      }
    },
    [showToast],
  );

  /**
   * copySSHCommand 复制到指定远程设备的 SSH 命令。
   *
   * 参数说明：
   * - name: string，远程设备名称。
   *
   * 返回值说明：
   * 返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 剪贴板权限被拒绝时 toast 错误。
   */
  const copySSHCommand = useCallback(
    async (name) => {
      try {
        await navigator.clipboard.writeText(`proxyd ssh ${name}`);
        showToast(`已复制 SSH 命令：proxyd ssh ${name}`);
      } catch (copyError) {
        showToast(`复制失败：${copyError.message}`, "err");
      }
    },
    [showToast],
  );

  /**
   * addForward 新增一条本地转发。
   *
   * 参数说明：
   * - form: {name: string, listen: string, remote: string, remotePort: string}，页面表单原始值；
   *   remote 可以是已保存设备名称或完整 token。
   *
   * 返回值说明：
   * 返回 Promise<boolean>；成功后重新拉取状态。
   *
   * 可能的异常/错误情况：
   * 表单不完整时本地拦截并 toast；后端校验失败时 toast 纯文本错误。
   */
  const addForward = useCallback(
    async (form) => {
      const name = form.name.trim();
      const listen = form.listen.trim();
      const remote = form.remote.trim();
      const remotePort = Number.parseInt(form.remotePort, 10);
      if (!name || !listen || !remote || !Number.isInteger(remotePort) || remotePort < 1 || remotePort > 65535) {
        showToast("请填写名称、监听地址、远端与 1-65535 的远端端口", "err");
        return false;
      }
      try {
        await requestJSON("/api/remote/forwards", {
          method: "POST",
          body: JSON.stringify({ name, listen, remote, remote_port: remotePort }),
        });
        showToast(`本地转发 ${name} 已添加`);
        await loadRemote();
        return true;
      } catch (addError) {
        showToast(`添加失败：${addError.message}`, "err");
        return false;
      }
    },
    [loadRemote, showToast],
  );

  /**
   * fetchPeerToken 获取指定远程设备的完整 token。
   *
   * 功能说明：
   * 列表接口只返回打码摘要，「连接」对话框需要完整 token 来生成 proxyd CLI 与
   * ssh config 命令，因此在打开对话框时按需显式请求。与 copyToken 同理，完整值
   * 只常驻于对话框生命周期内。
   *
   * 参数说明：
   * - name: string，远程设备名称。
   *
   * 返回值说明：
   * 返回 Promise<string>，成功时为完整 token。
   *
   * 可能的异常/错误情况：
   * 设备不存在（404）或网络失败时抛出 Error，由调用方在对话框内展示并支持重试。
   */
  const fetchPeerToken = useCallback(async (name) => {
    const payload = await requestJSON(`/api/remote/remotes/${encodeURIComponent(name)}/token`);
    return payload?.token || "";
  }, []);

  /**
   * createSSHForward 为指定对端创建一条自动分配端口的 SSH 本地转发。
   *
   * 功能说明：
   * listen 传空串，由后端自动分配 127.0.0.1 上的空闲端口；响应体即创建后的转发
   * 对象，其 listen 字段是实际监听的 `127.0.0.1:<port>`。
   *
   * 参数说明：
   * - peerName: string，远程设备名称（与转发 remote 字段的命名约定一致）。
   *
   * 返回值说明：
   * 返回 Promise<object | null>；成功时为创建后的转发对象。
   *
   * 可能的异常/错误情况：
   * 名称冲突或后端校验失败时 toast 纯文本错误并返回 null。
   */
  const createSSHForward = useCallback(
    async (peerName) => {
      const name = `ssh-${peerName}`;
      try {
        const payload = await requestJSON("/api/remote/forwards", {
          method: "POST",
          body: JSON.stringify({ name, listen: "", remote: peerName, remote_port: 22 }),
        });
        showToast(`SSH 转发 ${name} 已创建`);
        await loadRemote();
        return payload || null;
      } catch (createError) {
        showToast(`创建失败：${createError.message}`, "err");
        return null;
      }
    },
    [loadRemote, showToast],
  );

  /**
   * toggleForward 启用或停用一条本地转发。
   *
   * 参数说明：
   * - name: string，转发名称。
   * - enabled: boolean，目标开关状态。
   *
   * 返回值说明：
   * 返回 Promise<void>；成功后用响应体（与 GET 同构）更新状态。
   *
   * 可能的异常/错误情况：
   * 后端拒绝或网络失败时 toast 错误，开关保持原状态。
   */
  const toggleForward = useCallback(
    async (name, enabled) => {
      try {
        const payload = await requestJSON(`/api/remote/forwards/${encodeURIComponent(name)}`, {
          method: "PUT",
          body: JSON.stringify({ enabled }),
        });
        if (payload) setStatus(payload);
        showToast(enabled ? `转发 ${name} 已启用` : `转发 ${name} 已停用`);
      } catch (toggleError) {
        showToast(`操作失败：${toggleError.message}`, "err");
      }
    },
    [showToast],
  );

  /**
   * removeForward 经确认后删除一条本地转发。
   *
   * 参数说明：
   * - name: string，转发名称。
   *
   * 返回值说明：
   * 返回 Promise<boolean>；用户取消时返回 false。
   *
   * 可能的异常/错误情况：
   * 删除失败时 toast 错误并保留现有转发。
   */
  const removeForward = useCallback(
    async (name) => {
      const accepted = await requestConfirmation({
        title: `删除本地转发 ${name}？`,
        description: "删除后本地监听端口会立即释放，进行中的连接会被断开。",
        confirmLabel: "确认删除",
        destructive: true,
      });
      if (!accepted) return false;
      try {
        await requestJSON(`/api/remote/forwards/${encodeURIComponent(name)}`, { method: "DELETE" });
        showToast(`本地转发 ${name} 已删除`);
        await loadRemote();
        return true;
      } catch (removeError) {
        showToast(`删除失败：${removeError.message}`, "err");
        return false;
      }
    },
    [loadRemote, requestConfirmation, showToast],
  );

  return {
    error,
    hasLoaded,
    loading,
    refreshing,
    remotes,
    status,
    addForward,
    addRemote,
    copySSHCommand,
    copyText,
    copyToken,
    createSSHForward,
    fetchPeerToken,
    reload: loadRemote,
    removeForward,
    removeRemote,
    saveServe,
    toggleEnabled,
    toggleForward,
  };
}
