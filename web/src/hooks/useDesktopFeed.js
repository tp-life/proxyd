/**
 * 远程桌面页面数据 Hook。
 *
 * 功能说明：
 * 在独立的 desktop bounded context 内管理本机桌面服务、已保存连接档案和临时桌面
 * 会话。远程设备仍来自 remote context，本 Hook 只读取其名称作为连接档案引用，不
 * 接触或复制 token，避免凭据进入浏览器业务状态。
 *
 * 可能的异常/错误情况：
 * API 不可达时保留上一份成功快照并返回错误文本；写操作失败会通过全局 toast 提示，
 * 不会乐观修改页面状态。
 */
import { useCallback, useEffect, useRef, useState } from "react";
import { requestJSON } from "@/lib/api";

// 桌面页可见时的刷新间隔；它用于更新临时会话连接数和本机服务监听状态。
const DESKTOP_REFRESH_INTERVAL_MS = 10_000;

/**
 * launchDesktopClient 把后端生成的安全启动目标交给浏览器处理。
 *
 * 参数说明：session 为 POST /api/desktop/sessions 返回的会话对象。
 * 返回值说明：无；RDP 触发同源配置文件下载，VNC 交给系统 URI handler。
 * 可能的异常/错误情况：浏览器可能拦截自定义协议或机器未安装对应客户端；此时页面
 * 仍保留活动会话和本地地址，用户可以手动连接或重新点击。
 */
function launchDesktopClient(session) {
  if (!session?.launch_url) {
    throw new Error("后端没有返回桌面客户端启动地址");
  }
  if (session.launch_kind === "download") {
    const anchor = document.createElement("a");
    anchor.href = session.launch_url;
    anchor.download = "proxyd-desktop.rdp";
    anchor.rel = "noopener";
    document.body.appendChild(anchor);
    anchor.click();
    anchor.remove();
    return;
  }
  if (session.launch_kind === "uri") {
    window.location.assign(session.launch_url);
    return;
  }
  throw new Error(`不支持的桌面客户端启动类型：${session.launch_kind || "空"}`);
}

/**
 * useDesktopFeed 接入独立远程桌面页面的数据与操作。
 *
 * 参数说明：
 * - activeView: string，只有值为 desktop 时才加载与轮询。
 * - requestConfirmation: Function，删除连接前使用的全局确认对话框。
 * - showToast: Function，显示成功或错误消息。
 *
 * 返回值说明：
 * 返回 DesktopPage 所需的快照、加载状态和服务/档案/会话操作函数。
 *
 * 可能的异常/错误情况：
 * 页面切换会中止在途 GET；写请求不会因轮询而取消，成功响应始终成为最新快照。
 */
export function useDesktopFeed(activeView, requestConfirmation, showToast) {
  const [status, setStatus] = useState(null);
  const [remotes, setRemotes] = useState([]);
  const [loading, setLoading] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const loadedRef = useRef(false);
  const requestControllerRef = useRef(null);
  const requestTokenRef = useRef(0);
  const mutationTokenRef = useRef(0);

  /**
   * loadDesktop 并行读取桌面快照与远程设备名称。
   *
   * 参数说明：silent 为 true 时执行后台轮询，不显示刷新动画。
   * 返回值说明：返回 Promise<boolean>，成功为 true，被取消或失败为 false。
   * 可能的异常/错误情况：新请求会取消旧请求并通过递增 token 防止旧响应覆盖新状态；
   * 后端错误会保留已有数据并写入 error。
   */
  const loadDesktop = useCallback(async (silent = false) => {
    const requestToken = requestTokenRef.current + 1;
    const mutationToken = mutationTokenRef.current;
    requestTokenRef.current = requestToken;
    requestControllerRef.current?.abort();
    const controller = new AbortController();
    requestControllerRef.current = controller;

    if (!loadedRef.current) {
      setLoading(true);
    } else if (!silent) {
      setRefreshing(true);
    }

    try {
      const [nextStatus, nextRemotes] = await Promise.all([
        requestJSON("/api/desktop", { signal: controller.signal }),
        requestJSON("/api/remote/remotes", { signal: controller.signal }),
      ]);
      if (requestTokenRef.current !== requestToken || mutationTokenRef.current !== mutationToken) return false;
      setStatus(nextStatus);
      setRemotes(nextRemotes?.remotes || []);
      setError("");
      loadedRef.current = true;
      return true;
    } catch (loadError) {
      if (loadError?.name === "AbortError" || requestTokenRef.current !== requestToken || mutationTokenRef.current !== mutationToken) return false;
      setError(loadError.message || "远程桌面状态加载失败");
      loadedRef.current = true;
      return false;
    } finally {
      if (requestTokenRef.current === requestToken) {
        setLoading(false);
        setRefreshing(false);
      }
    }
  }, []);

  useEffect(() => {
    if (activeView !== "desktop") {
      requestControllerRef.current?.abort();
      return undefined;
    }
    loadDesktop();
    const timer = window.setInterval(() => loadDesktop(true), DESKTOP_REFRESH_INTERVAL_MS);
    return () => {
      window.clearInterval(timer);
      requestControllerRef.current?.abort();
    };
  }, [activeView, loadDesktop]);

  /**
   * runMutation 串行呈现一个桌面写操作并采用服务端完整快照更新页面。
   *
   * 参数说明：key 是忙碌标识；request 执行实际请求；successMessage 是成功提示。
   * 返回值说明：返回 Promise<object|null>，成功为响应对象，失败为 null。
   * 可能的异常/错误情况：HTTP 或网络错误会 toast；finally 总会清理 busy，避免按钮
   * 永久锁死。后端事务失败时页面不伪装为已更新。
   */
  const runMutation = useCallback(
    async (key, request, successMessage) => {
      // 写操作开始与结束各推进一次代次。这样不仅取消已经在途的 GET，也能阻止写入
      // 过程中由定时器新发起的旧快照在事务完成后覆盖成功响应。
      mutationTokenRef.current += 1;
      requestControllerRef.current?.abort();
      setBusy(key);
      try {
        const payload = await request();
        mutationTokenRef.current += 1;
        if (payload?.services) setStatus(payload);
        setError("");
        if (successMessage) showToast(successMessage);
        return payload;
      } catch (mutationError) {
        mutationTokenRef.current += 1;
        showToast(`操作失败：${mutationError.message}`, "err");
        return null;
      } finally {
        setBusy("");
      }
    },
    [showToast],
  );

  /**
   * saveService 保存一个协议的真实服务端口和隧道暴露开关。
   *
   * 参数说明：protocol 为 rdp/vnc；port 为 1-65535；exposed 表示是否允许对端访问。
   * 返回值说明：返回 Promise<boolean>，事务成功为 true。
   * 可能的异常/错误情况：非法端口由前端先提示，后端还会二次校验并原子回滚。
   */
  const saveService = useCallback(
    async (protocol, port, exposed) => {
      const parsedPort = Number(port);
      if (!Number.isInteger(parsedPort) || parsedPort < 1 || parsedPort > 65535) {
        showToast("服务端口必须是 1-65535 的整数", "err");
        return false;
      }
      const payload = await runMutation(
        `service:${protocol}`,
        () => requestJSON(`/api/desktop/services/${encodeURIComponent(protocol)}`, {
          method: "POST",
          body: JSON.stringify({ port: parsedPort, exposed: Boolean(exposed) }),
        }),
        "桌面服务配置已保存",
      );
      return Boolean(payload);
    },
    [runMutation, showToast],
  );

  /**
   * saveConnection 新增或更新一个不含密码的桌面连接档案。
   *
   * 参数说明：connection 是完整表单值；currentName 非空表示编辑该原名称。
   * 返回值说明：返回 Promise<boolean>，持久化成功为 true。
   * 可能的异常/错误情况：名称冲突、远端/协议/端口非法或落盘失败由后端返回；连接
   * 密码从不接受也不保存。
   */
  const saveConnection = useCallback(
    async (connection, currentName = "") => {
      const editing = Boolean(currentName);
      const url = editing
        ? `/api/desktop/connections/${encodeURIComponent(currentName)}`
        : "/api/desktop/connections";
      const payload = await runMutation(
        `connection:${currentName || connection.name}`,
        () => requestJSON(url, {
          method: editing ? "PUT" : "POST",
          body: JSON.stringify(connection),
        }),
        editing ? "桌面连接已更新" : "桌面连接已保存",
      );
      return Boolean(payload);
    },
    [runMutation],
  );

  /**
   * deleteConnection 经用户确认后删除保存档案。
   *
   * 参数说明：name 为档案名称。
   * 返回值说明：返回 Promise<boolean>；用户取消或请求失败均为 false。
   * 可能的异常/错误情况：删除不强制中断已启动会话，这是后端的显式生命周期规则；
   * 活动会话仍可在页面中手动断开或由空闲回收器清理。
   */
  const deleteConnection = useCallback(
    async (name) => {
      const confirmed = await requestConfirmation({
        title: "删除桌面连接",
        description: `确定删除“${name}”吗？已经打开的临时会话不会被强制中断。`,
        confirmLabel: "删除",
        destructive: true,
      });
      if (!confirmed) return false;
      const payload = await runMutation(
        `delete:${name}`,
        () => requestJSON(`/api/desktop/connections/${encodeURIComponent(name)}`, { method: "DELETE" }),
        "桌面连接已删除",
      );
      return Boolean(payload);
    },
    [requestConfirmation, runMutation],
  );

  /**
   * startSession 创建或复用临时回环转发，并尝试唤起系统桌面客户端。
   *
   * 参数说明：connectionName 为保存档案名称。
   * 返回值说明：返回 Promise<object|null>，成功时为会话快照。
   * 可能的异常/错误情况：远端不可达或转发创建失败会 toast；客户端未安装/浏览器拦截
   * 时也会提示，但已经创建的会话继续保留，便于用户复制本地地址手动连接。
   */
  const startSession = useCallback(
    async (connectionName) => {
      mutationTokenRef.current += 1;
      requestControllerRef.current?.abort();
      setBusy(`start:${connectionName}`);
      try {
        const session = await requestJSON("/api/desktop/sessions", {
          method: "POST",
          body: JSON.stringify({ connection: connectionName }),
        });
        mutationTokenRef.current += 1;
        setStatus((current) => {
          if (!current) return current;
          const sessions = (current.sessions || []).filter((item) => item.id !== session.id);
          return { ...current, sessions: [...sessions, session] };
        });
        try {
          launchDesktopClient(session);
          showToast(session.protocol === "rdp" ? "RDP 配置已下载，请用系统客户端打开" : "已请求打开系统 VNC 客户端");
        } catch (launchError) {
          showToast(`会话已建立，但客户端未能自动打开：${launchError.message}`, "err");
        }
        return session;
      } catch (sessionError) {
        mutationTokenRef.current += 1;
        showToast(`连接失败：${sessionError.message}`, "err");
        return null;
      } finally {
        setBusy("");
      }
    },
    [showToast],
  );

  /**
   * relaunchSession 对已有会话再次触发系统客户端，不新建转发。
   *
   * 参数说明：session 为当前状态中的会话对象。
   * 返回值说明：返回 boolean，浏览器接受启动动作时为 true。
   * 可能的异常/错误情况：启动目标缺失、协议未知或浏览器拒绝时返回 false 并 toast。
   */
  const relaunchSession = useCallback(
    (session) => {
      try {
        launchDesktopClient(session);
        return true;
      } catch (launchError) {
        showToast(`客户端未能打开：${launchError.message}`, "err");
        return false;
      }
    },
    [showToast],
  );

  /**
   * stopSession 显式关闭临时转发并释放本机端口。
   *
   * 参数说明：sessionID 为后端生成的随机会话标识。
   * 返回值说明：返回 Promise<boolean>，关闭成功为 true。
   * 可能的异常/错误情况：会话已被空闲回收时后端可能返回 404；页面会提示并重新加载
   * 当前快照，以消除过期记录。
   */
  const stopSession = useCallback(
    async (sessionID) => {
      mutationTokenRef.current += 1;
      requestControllerRef.current?.abort();
      setBusy(`stop:${sessionID}`);
      try {
        await requestJSON(`/api/desktop/sessions/${encodeURIComponent(sessionID)}`, { method: "DELETE" });
        mutationTokenRef.current += 1;
        setStatus((current) => current ? {
          ...current,
          sessions: (current.sessions || []).filter((item) => item.id !== sessionID),
        } : current);
        showToast("桌面会话已断开");
        return true;
      } catch (stopError) {
        mutationTokenRef.current += 1;
        showToast(`断开失败：${stopError.message}`, "err");
        await loadDesktop(true);
        return false;
      } finally {
        setBusy("");
      }
    },
    [loadDesktop, showToast],
  );

  /**
   * copyText 复制本地地址或 CLI 命令，并统一处理非安全上下文中的剪贴板失败。
   *
   * 参数说明：text 是待复制文本；message 是成功提示。
   * 返回值说明：返回 Promise<boolean>，复制成功为 true。
   * 可能的异常/错误情况：浏览器权限或非 HTTPS 环境可能拒绝 clipboard API，此时 toast。
   */
  const copyText = useCallback(
    async (text, message = "已复制") => {
      try {
        await navigator.clipboard.writeText(text);
        showToast(message);
        return true;
      } catch (copyError) {
        showToast(`复制失败：${copyError.message}`, "err");
        return false;
      }
    },
    [showToast],
  );

  return {
    status,
    remotes,
    loading,
    refreshing,
    busy,
    error,
    reload: loadDesktop,
    saveService,
    saveConnection,
    deleteConnection,
    startSession,
    relaunchSession,
    stopSession,
    copyText,
  };
}
