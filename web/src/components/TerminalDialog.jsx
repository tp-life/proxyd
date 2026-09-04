/**
 * 浏览器 Web Terminal 全屏对话框模块。
 *
 * 功能说明：
 * 仅在用户主动打开终端时动态加载 xterm.js 与自适应插件，再通过 WebSocket 把键盘输入、
 * 终端输出和窗口尺寸与后端 PTY 会话桥接。组件不持有任何远程模块配置，也不执行开关
 * 变更，关闭弹层即关闭当前会话。可携带一条首命令（如 `proxyd ssh <设备>`），连接
 * 建立后自动发送执行，用于一键进入对端设备。
 *
 * 可能的异常/错误情况：
 * 动态资源加载失败、WebSocket 握手被安全门拒绝、服务运行中断或浏览器网络断开时，
 * 弹层会保留并显示明确错误；用户可关闭后修复配置再重新打开。
 */
import { useEffect, useRef, useState } from "react";
import { CircleAlert, LoaderCircle, TerminalSquare, Wifi, WifiOff, X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogClose, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";

/**
 * buildTerminalWebSocketURL 构造与当前控制台同源的终端 WebSocket 地址。
 *
 * 参数说明：
 * - cols: number，xterm 当前列数。
 * - rows: number，xterm 当前行数。
 *
 * 返回值说明：返回 string，协议按当前页面的 http/https 自动映射为 ws/wss。
 * 可能的异常/错误情况：仅在浏览器环境调用；若页面 URL 本身无效，URL 构造会抛错并由启动流程展示。
 */
function buildTerminalWebSocketURL(cols, rows) {
  const url = new URL("/api/remote/terminal", window.location.href);
  url.protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  url.searchParams.set("cols", String(cols));
  url.searchParams.set("rows", String(rows));
  return url.toString();
}

/**
 * parseTerminalControl 解析服务端文本控制帧。
 *
 * 参数说明：message 为 string，当前协议中仅用于传递 JSON 错误消息。
 * 返回值说明：返回 string；能识别时返回服务端错误，否则返回可读的原始消息。
 * 可能的异常/错误情况：JSON 损坏时不会继续抛错，而是回退显示原文，避免掩盖诊断信息。
 */
function parseTerminalControl(message) {
  try {
    const payload = JSON.parse(message);
    if (payload?.type === "error" && payload?.message) return payload.message;
  } catch {
    // 文本帧不是 JSON 时直接展示原文，便于诊断服务端或代理层返回的异常内容。
  }
  return message || "终端服务返回了未知控制消息";
}

/**
 * TerminalDialog 渲染一条独立的浏览器 shell 会话。
 *
 * 参数说明：
 * - open: boolean，是否打开弹层；从 false 变为 true 时创建全新的终端与 WebSocket。
 * - onOpenChange: (open: boolean) => void，Radix 关闭事件回调。
 * - command: string，可选；连接建立后自动发送到 shell 执行的首条命令（如 proxyd ssh）。
 * - target: string，可选；会话目标说明（如远程设备名称），仅用于标题展示。
 *
 * 返回值说明：返回 React 元素；关闭时仍保留 Dialog 根节点以正确归还焦点。
 * 可能的异常/错误情况：终端依赖、WebSocket 或 PTY 任一环节失败时进入 error/disconnected 状态。
 */
export default function TerminalDialog({ open, onOpenChange, command = "", target = "" }) {
  const containerRef = useRef(null);
  const [phase, setPhase] = useState("loading");
  const [message, setMessage] = useState("正在加载终端组件…");

  useEffect(() => {
    if (!open) return undefined;

    let active = true;
    let terminal;
    let fitAddon;
    let socket;
    let inputSubscription;
    let resizeObserver;
    let resizeFrame = 0;
    let commandTimer = 0;

    /**
     * currentSize 读取并约束当前 xterm 网格尺寸。
     *
     * 参数说明：无。
     * 返回值说明：返回 `{cols:number, rows:number}`，终端尚未完成测量时使用 80x24。
     * 可能的异常/错误情况：无；异常尺寸会在浏览器端先回退，后端仍会再次执行边界约束。
     */
    function currentSize() {
      return {
        cols: Number.isInteger(terminal?.cols) && terminal.cols > 0 ? terminal.cols : 80,
        rows: Number.isInteger(terminal?.rows) && terminal.rows > 0 ? terminal.rows : 24,
      };
    }

    /**
     * fitTerminal 根据弹层可用空间重新计算终端行列数。
     *
     * 参数说明：无。
     * 返回值说明：无；测量成功后会同步 PTY 尺寸。
     * 可能的异常/错误情况：弹层尚不可见时跳过测量；插件测量异常不会中断现有终端会话。
     */
    function fitTerminal() {
      const container = containerRef.current;
      if (!fitAddon || !container || container.clientWidth <= 0 || container.clientHeight <= 0) return;
      try {
        fitAddon.fit();
        sendResize();
      } catch {
        // 布局切换的中间帧可能暂时无法计算字符网格，下一次 ResizeObserver 会自动重试。
      }
    }

    /**
     * sendResize 把当前 xterm 行列数作为文本控制帧同步给后端 PTY。
     *
     * 参数说明：无。
     * 返回值说明：无。
     * 可能的异常/错误情况：WebSocket 未连接或已关闭时静默跳过，避免 resize 竞争导致 DOM 异常。
     */
    function sendResize() {
      if (!socket || socket.readyState !== WebSocket.OPEN) return;
      const size = currentSize();
      socket.send(JSON.stringify({ type: "resize", cols: size.cols, rows: size.rows }));
    }

    /**
     * handleTerminalData 把 xterm 键盘输入编码为二进制帧发给后端标准输入。
     *
     * 参数说明：data 为 string，包含按键、粘贴或终端控制序列。
     * 返回值说明：无。
     * 可能的异常/错误情况：连接未就绪时丢弃输入，避免浏览器缓存敏感命令后延迟发送。
     */
    function handleTerminalData(data) {
      if (!socket || socket.readyState !== WebSocket.OPEN) return;
      socket.send(new TextEncoder().encode(data));
    }

    /**
     * handleSocketOpen 标记会话已连接并让终端获得键盘焦点。
     *
     * 参数说明：无。
     * 返回值说明：无；携带首命令时延迟 300ms 再发送，等待对端 shell 就绪后再注入输入。
     * 可能的异常/错误情况：组件已卸载时不更新状态；关闭竞争由 active 标记隔离，
     * 首命令定时器在清理函数中取消，避免弹层关闭后向旧连接迟发。
     */
    function handleSocketOpen() {
      if (!active) return;
      setPhase("connected");
      setMessage(target ? `已连接，正在进入 ${target}…` : "已连接到本机 shell");
      fitTerminal();
      terminal?.focus();
      if (command) {
        commandTimer = window.setTimeout(() => {
          if (!active || !socket || socket.readyState !== WebSocket.OPEN) return;
          socket.send(new TextEncoder().encode(`${command}\r`));
        }, 300);
      }
    }

    /**
     * handleSocketMessage 把服务端二进制输出写入 xterm，并处理文本错误控制帧。
     *
     * 参数说明：event 为 MessageEvent，data 可能是 ArrayBuffer 或 string。
     * 返回值说明：无。
     * 可能的异常/错误情况：未知二进制类型会转为 Uint8Array；文本控制帧会切换错误状态。
     */
    function handleSocketMessage(event) {
      if (!active) return;
      if (typeof event.data === "string") {
        setPhase("error");
        setMessage(parseTerminalControl(event.data));
        return;
      }
      terminal?.write(new Uint8Array(event.data));
    }

    /**
     * handleSocketClose 展示会话关闭状态。
     *
     * 参数说明：event 为 CloseEvent，包含关闭码与可选原因。
     * 返回值说明：无。
     * 可能的异常/错误情况：服务端未给出原因时显示通用提示；主动关闭弹层时不更新已卸载状态。
     */
    function handleSocketClose(event) {
      if (!active) return;
      setPhase("disconnected");
      setMessage(event.reason || (event.code === 1000 ? "终端会话已结束" : `终端连接已断开（${event.code}）`));
    }

    /**
     * handleSocketError 标记 WebSocket 传输失败。
     *
     * 参数说明：无；浏览器不会向页面暴露底层网络错误细节。
     * 返回值说明：无。
     * 可能的异常/错误情况：后续 close 事件可能覆盖本提示，但仍保留最终关闭码帮助定位。
     */
    function handleSocketError() {
      if (!active) return;
      setPhase("error");
      setMessage("终端连接失败，请确认 Web Terminal 已开启");
    }

    /**
     * handleContainerResize 把连续布局变化合并到下一动画帧，避免频繁重排 xterm。
     *
     * 参数说明：无；ResizeObserver 的条目内容由 fit-addon 自行读取容器尺寸。
     * 返回值说明：无。
     * 可能的异常/错误情况：组件清理时会取消尚未执行的动画帧。
     */
    function handleContainerResize() {
      window.cancelAnimationFrame(resizeFrame);
      resizeFrame = window.requestAnimationFrame(fitTerminal);
    }

    /**
     * startTerminal 懒加载终端依赖、挂载 xterm 并建立 WebSocket。
     *
     * 参数说明：无。
     * 返回值说明：返回 Promise<void>，连接建立本身由 WebSocket 事件异步反馈。
     * 可能的异常/错误情况：chunk 加载、样式加载或 xterm 初始化失败时 Promise 会拒绝。
     */
    async function startTerminal() {
      setPhase("loading");
      setMessage("正在加载终端组件…");

      /*
       * xterm 及其 CSS 只在弹层真正打开时下载。这样主控制台首屏不会包含终端运行时，
       * 同时由 Vite 为 xterm 和 addon-fit 生成独立懒加载 chunk。
       */
      const [{ Terminal }, { FitAddon }] = await Promise.all([
        import("@xterm/xterm"),
        import("@xterm/addon-fit"),
        import("@xterm/xterm/css/xterm.css"),
      ]);
      if (!active || !containerRef.current) return;

      terminal = new Terminal({
        allowProposedApi: false,
        convertEol: false,
        cursorBlink: true,
        cursorStyle: "bar",
        fontFamily: '"SFMono-Regular", "Cascadia Code", "Liberation Mono", Menlo, monospace',
        fontSize: 14,
        lineHeight: 1.25,
        scrollback: 5000,
        theme: {
          background: "#080d14",
          foreground: "#d7e4eb",
          cursor: "#67e8f9",
          cursorAccent: "#080d14",
          selectionBackground: "#164e6388",
          black: "#17202b",
          red: "#fb7185",
          green: "#5eead4",
          yellow: "#facc15",
          blue: "#60a5fa",
          magenta: "#c084fc",
          cyan: "#22d3ee",
          white: "#e2e8f0",
          brightBlack: "#64748b",
          brightRed: "#fda4af",
          brightGreen: "#99f6e4",
          brightYellow: "#fde047",
          brightBlue: "#93c5fd",
          brightMagenta: "#d8b4fe",
          brightCyan: "#67e8f9",
          brightWhite: "#f8fafc",
        },
      });
      fitAddon = new FitAddon();
      terminal.loadAddon(fitAddon);
      terminal.open(containerRef.current);
      fitTerminal();

      const size = currentSize();
      socket = new WebSocket(buildTerminalWebSocketURL(size.cols, size.rows));
      socket.binaryType = "arraybuffer";
      socket.addEventListener("open", handleSocketOpen);
      socket.addEventListener("message", handleSocketMessage);
      socket.addEventListener("close", handleSocketClose);
      socket.addEventListener("error", handleSocketError);
      inputSubscription = terminal.onData(handleTerminalData);

      resizeObserver = new ResizeObserver(handleContainerResize);
      resizeObserver.observe(containerRef.current);
    }

    /**
     * handleStartFailure 把动态加载或初始化失败转换为可见错误状态。
     *
     * 参数说明：error 为 unknown，通常是资源加载或浏览器 API 错误。
     * 返回值说明：无。
     * 可能的异常/错误情况：组件已关闭时忽略迟到的 Promise 拒绝。
     */
    function handleStartFailure(error) {
      if (!active) return;
      setPhase("error");
      setMessage(error?.message || "终端组件加载失败");
    }

    startTerminal().catch(handleStartFailure);

    /**
     * effect 清理顺序先阻止异步回调更新 UI，再断开传输和释放终端 DOM，避免 StrictMode
     * 重挂载时上一条会话的 close 事件污染新会话状态。
     */
    return () => {
      active = false;
      window.cancelAnimationFrame(resizeFrame);
      window.clearTimeout(commandTimer);
      resizeObserver?.disconnect();
      inputSubscription?.dispose();
      if (socket?.readyState === WebSocket.OPEN) socket.close(1000, "用户关闭终端");
      else if (socket?.readyState === WebSocket.CONNECTING) socket.close();
      terminal?.dispose();
    };
  }, [open, command, target]);

  const connected = phase === "connected";

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="terminal-dialog" aria-describedby="web-terminal-description">
        <header className="terminal-dialog-toolbar">
          <DialogHeader className="min-w-0 gap-0 pr-0">
            <DialogTitle className="flex items-center gap-2 text-base">
              <TerminalSquare size={18} aria-hidden="true" />
              <span>Web Terminal{target ? ` · ${target}` : ""}</span>
            </DialogTitle>
            <DialogDescription id="web-terminal-description" className="sr-only">
              {target
                ? `当前 proxyd 进程用户的本机交互式 shell，已自动执行连接 ${target} 的命令，关闭弹层即结束会话。`
                : "当前 proxyd 进程用户的本机交互式 shell，关闭弹层即结束会话。"}
            </DialogDescription>
          </DialogHeader>
          <div className="ml-auto flex items-center gap-2">
            <Badge className="gap-1.5" variant={connected ? "secondary" : phase === "error" ? "destructive" : "outline"}>
              {connected ? <Wifi size={12} aria-hidden="true" /> : phase === "loading" ? <LoaderCircle className="animate-spin" size={12} aria-hidden="true" /> : <WifiOff size={12} aria-hidden="true" />}
              {connected ? "已连接" : phase === "loading" ? "连接中" : "已断开"}
            </Badge>
            <DialogClose asChild>
              <Button aria-label="关闭 Web Terminal" size="icon" type="button" variant="ghost">
                <X size={18} aria-hidden="true" />
              </Button>
            </DialogClose>
          </div>
        </header>

        <div className="terminal-dialog-body">
          <div ref={containerRef} className="terminal-surface" aria-label="本机 shell 终端" />
          {phase !== "connected" && (
            <div className="terminal-dialog-state" role={phase === "error" ? "alert" : "status"}>
              {phase === "loading" ? <LoaderCircle className="animate-spin" size={20} aria-hidden="true" /> : <CircleAlert size={20} aria-hidden="true" />}
              <span>{message}</span>
            </div>
          )}
        </div>

        <footer className="terminal-dialog-footer">
          <span>高权限会话 · 当前进程用户 · 关闭窗口立即断开</span>
          <span className="font-mono">TERM=xterm-256color</span>
        </footer>
      </DialogContent>
    </Dialog>
  );
}
