import { useEffect, useMemo, useState } from "react";
import { CircleAlert, Copy, Gauge, Plus, Search, Trash2, X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Table } from "@/components/ui/data-table";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { SegmentedControl } from "@/components/ui/segmented-control";
import { Switch as UISwitch } from "@/components/ui/switch";
import { Field } from "@/components/Field";
import { PageHeader } from "@/components/PageHeader";
import { PanelTitle } from "@/components/PanelTitle";
import { StatusBadge } from "@/components/StatusBadge";
import { requestJSON } from "@/lib/api";
import { classNames, delayClass, formatDelay, sortByDelay, tableViewportHeight } from "@/lib/format";

/** VPN_TYPE_OPTIONS 只保留尚未拥有独立管理页的高级隧道出站类型。 */
const VPN_TYPE_OPTIONS = [
  { value: "zerotier", label: "ZeroTier" },
  { value: "wireguard", label: "WireGuard" },
  { value: "ssh", label: "SSH" },
];

/** ADVANCED_VPN_TYPES 走通用 JSON 透传的隧道类型（参数较多，面向高级用户）。 */
const ADVANCED_VPN_TYPES = ["zerotier", "wireguard", "ssh"];

/** TUNNEL_GROUP_TYPE_LABELS 把 mihomo 策略类型转换为隧道页可读文案。 */
const TUNNEL_GROUP_TYPE_LABELS = {
  fallback: "故障转移",
  "url-test": "自动测速",
  "load-balance": "负载均衡",
  select: "手动选择",
};

/** EMPTY_VPN_FORM 是 VPN 节点表单的初始草稿。 */
const EMPTY_VPN_FORM = {
  type: "tailscale",
  name: "",
  authKey: "",
  authMode: "approval",
  controlURL: "",
  hostname: "",
  groupName: "",
  groupPort: "",
  accessMode: "proxy",
  target: "",
  exitNode: "",
  dialerProxy: "",
  ipVersion: "dual",
  acceptRoutes: false,
  exitNodeAllowLANAccess: false,
  udp: false,
  ephemeral: false,
  server: "",
  port: "",
  proto: "udp",
  ca: "",
  cert: "",
  key: "",
  tlsAuth: "",
  username: "",
  password: "",
  profile: "",
  profileName: "",
  privateKeyPassphrase: "",
  openVPNUDP: true,
  json: "",
};

/** VPN_TEXTAREA_CLASS 复用远程模块的等宽多行输入样式，承载证书材料与 JSON。 */
const VPN_TEXTAREA_CLASS = "mono-input min-h-24 w-full rounded-md border bg-background p-2 text-xs";

/**
 * NodesPage 渲染跨来源聚合的节点工作台。
 *
 * 功能说明：
 * 节点与订阅拆分后，本页只承担节点搜索、来源/协议/状态筛选、测速和主节点选择。
 * 添加手动节点使用 Radix Dialog，避免常驻大表单挤压列表首屏。
 *
 * 参数说明：
 * - busy: string，全局后台操作标识；非空时禁用测速按钮，等于“测速”时显示加载态。
 * - forms: object，全局受控表单状态。
 * - initialSource: string，从订阅页跳转时指定的初始来源。
 * - overview: object，概览、节点和稳定端口分配数据。
 * - onDelete/onForm/onMainNode/onPost/onSourceChange/onTest: Function，页面动作回调。
 * - workspace: string，可选的独立隧道工作区；`tailscale`、`openvpn` 或默认节点页。
 *
 * 返回值说明：
 * 返回节点工作台 React 元素。
 *
 * 可能的异常/错误情况：
 * 空地址会在前端拦截；后端解析、热更新或持久化失败由 onPost 统一 toast。
 */
export function NodesPage({ busy, forms = {}, initialSource, overview, onDelete, onForm, onMainNode, onPost, onSourceChange, onTest, workspace = "nodes" }) {
  const dedicatedTunnel = workspace === "tailscale" || workspace === "openvpn";
  const [addOpen, setAddOpen] = useState(false);
  const [adding, setAdding] = useState(false);
  const [terminating, setTerminating] = useState(false);
  const [addMode, setAddMode] = useState(dedicatedTunnel ? "vpn" : "url");
  const [addError, setAddError] = useState("");
  const [vpn, setVpn] = useState(() => ({ ...EMPTY_VPN_FORM, type: dedicatedTunnel ? workspace : VPN_TYPE_OPTIONS[0].value }));
  const [enrollment, setEnrollment] = useState(null);
  const [openVPNPreview, setOpenVPNPreview] = useState(null);
  const [importingOpenVPN, setImportingOpenVPN] = useState(false);
  const [query, setQuery] = useState("");
  const [source, setSource] = useState(initialSource || "all");
  const [protocol, setProtocol] = useState("all");
  const [status, setStatus] = useState("all");
  const [sort, setSort] = useState("default");

  useEffect(() => {
    setSource(initialSource || "all");
  }, [initialSource]);

  useEffect(() => {
    if (!addOpen || !enrollment?.name) return undefined;
    let active = true;

    /**
     * loadEnrollment 拉取 mihomo/tsnet 当前注册状态。
     *
     * 参数说明：无；节点名来自刚成功提交的一体化接入结果。
     *
     * 返回值说明：返回 Promise<void>；成功时更新对话框内的注册链接或连接状态。
     *
     * 可能的异常/错误情况：进程重启或状态尚未建立时保留当前提示并等待下轮轮询，
     * 避免短暂 404 把已经成功落盘的接入配置误报为失败。
     */
    async function loadEnrollment() {
      try {
        const next = await requestJSON(`/api/tailscale/enrollments/${encodeURIComponent(enrollment.name)}`);
        if (active) setEnrollment(next);
      } catch {
        // 注册触发与管理面轮询是异步的；暂时不可读时继续保留初始状态。
      }
    }

    loadEnrollment();
    const timer = window.setInterval(loadEnrollment, 2000);
    return () => {
      active = false;
      window.clearInterval(timer);
    };
  }, [addOpen, enrollment?.name]);

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
  // 手动触发后 busy 立即为「测速」；后台检测期间由 overview.testing 接力，
  // 避免延迟列在新结果出来前继续展示容易被误读为已完成的上轮延迟。
  const testing = busy === "测速" || Boolean(overview.testing);
  const managedManualNodes = useMemo(
    () => (overview.manual_nodes || []).filter((node) => {
      if (dedicatedTunnel) return node.type === workspace;
      return node.type !== "tailscale" && node.type !== "openvpn";
    }),
    [dedicatedTunnel, overview.manual_nodes, workspace],
  );
  const managedTunnelNames = useMemo(
    () => new Set(managedManualNodes.map((node) => node.name).filter(Boolean)),
    [managedManualNodes],
  );
  const managedTunnelRuntimeNodes = useMemo(
    () => (overview.nodes || []).filter((node) => node.type === workspace && managedTunnelNames.has(node.name)),
    [managedTunnelNames, overview.nodes, workspace],
  );
  const managedTunnelGroups = useMemo(
    () => (overview.groups || []).filter((group) => tunnelGroupReferencesNodes(group, managedTunnelNames, managedTunnelRuntimeNodes)),
    [managedTunnelNames, managedTunnelRuntimeNodes, overview.groups],
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
   * updateVPN 更新 VPN 节点表单的单个字段。
   *
   * 参数说明：
   * - key: string，EMPTY_VPN_FORM 中的字段名。
   * - value: string|boolean，文本、选择器或开关的新值。
   *
   * 返回值说明：无。
   *
   * 可能的异常/错误情况：无；字段合法性在提交时统一校验。
   */
  function updateVPN(key, value) {
    setVpn((current) => ({ ...current, [key]: value }));
    setAddError("");
  }

  /**
   * importOpenVPNProfile 读取用户选择的 .ovpn，并调用后端领域解析器做安全预览。
   *
   * 参数说明：event 是原生 file input 的 change 事件，只读取第一个文件。
   *
   * 返回值说明：返回 Promise<void>；成功后原始文本仅保存在当前表单内存，页面只
   * 展示后端返回的不含证书和凭据摘要。
   *
   * 可能的异常/错误情况：文件超过 1 MiB、浏览器读取失败或 profile 使用外部证书
   * 文件/不兼容指令时显示内联错误；旧预览会被清空，避免误用上一次解析结果。
   */
  async function importOpenVPNProfile(event) {
    const file = event.target.files?.[0];
    if (!file || importingOpenVPN) return;
    setOpenVPNPreview(null);
    setAddError("");
    if (file.size > 1024 * 1024) {
      setAddError(".ovpn 文件不能超过 1 MiB");
      event.target.value = "";
      return;
    }
    setImportingOpenVPN(true);
    try {
      const profile = await file.text();
      const preview = await requestJSON("/api/openvpn/import", {
        method: "POST",
        body: JSON.stringify({ profile }),
      });
      setVpn((current) => ({
        ...current,
        profile,
        profileName: file.name,
        server: preview.server || "",
        port: preview.port ? String(preview.port) : "",
        proto: preview.proto || "udp",
      }));
      setOpenVPNPreview(preview);
    } catch (error) {
      setVpn((current) => ({ ...current, profile: "", profileName: "" }));
      setAddError(`导入失败：${error.message}`);
    } finally {
      setImportingOpenVPN(false);
      event.target.value = "";
    }
  }

  /**
   * clearOpenVPNProfile 清除当前对话框内尚未提交的 .ovpn 文本与解析摘要。
   *
   * 参数说明：无。
   *
   * 返回值说明：无；服务器地址、证书等手工字段同时复位，防止导入值与手填值混用。
   *
   * 可能的异常/错误情况：无；该操作只影响浏览器草稿，不会修改已创建接入。
   */
  function clearOpenVPNProfile() {
    setVpn((current) => ({
      ...current,
      profile: "",
      profileName: "",
      server: "",
      port: "",
      proto: "udp",
      ca: "",
      cert: "",
      key: "",
      tlsAuth: "",
      privateKeyPassphrase: "",
    }));
    setOpenVPNPreview(null);
    setAddError("");
  }

  /**
   * buildVPNProxy 把 VPN 表单草稿组装成 mihomo 出站映射。
   *
   * 参数说明：无；读取当前 vpn 状态。
   *
   * 返回值说明：
   * 返回 `{ proxy }`、`{ setup }` 或 `{ error }`；普通高级隧道的 proxy 对应
   * POST /api/manual-nodes，Tailscale/OpenVPN setup 对应各自的一体化接入事务。
   *
   * 可能的异常/错误情况：
   * 必填项缺失或 JSON 解析失败时返回 error 文案，由对话框内联展示。
   */
  function buildVPNProxy() {
    const name = vpn.name.trim();
    if (!name) return { error: "请填写节点名称" };
    if (vpn.type === "tailscale") {
      if (!vpn.controlURL.trim()) return { error: "请填写 Tailscale/Headscale 控制服务地址" };
      if (vpn.authMode === "auth-key" && !vpn.authKey.trim()) return { error: "Auth Key 模式必须填写 auth-key" };
      if (["tun", "both"].includes(vpn.accessMode) && !vpn.target.trim()) return { error: "TUN 访问方式必须填写目标 IP 或 CIDR" };
      const groupPort = vpn.groupPort.trim() ? Number.parseInt(vpn.groupPort, 10) : 0;
      if (vpn.groupPort.trim() && (!groupPort || groupPort < 1 || groupPort > 65535)) return { error: "代理入口端口必须为 1-65535，留空可自动分配" };
      return {
        setup: {
          name,
          control_url: vpn.controlURL.trim(),
          hostname: vpn.hostname.trim(),
          auth_mode: vpn.authMode,
          auth_key: vpn.authMode === "auth-key" ? vpn.authKey.trim() : "",
          group_name: vpn.groupName.trim(),
          port: groupPort,
          access_mode: vpn.accessMode,
          target: vpn.target.trim(),
          accept_routes: vpn.acceptRoutes,
          udp: vpn.udp,
          ephemeral: vpn.ephemeral,
          exit_node: vpn.exitNode.trim(),
          exit_node_allow_lan_access: Boolean(vpn.exitNode.trim() && vpn.exitNodeAllowLANAccess),
          dialer_proxy: vpn.dialerProxy.trim(),
          ip_version: vpn.ipVersion,
        },
      };
    }
    if (vpn.type === "openvpn") {
      const port = Number.parseInt(vpn.port, 10);
      if (!vpn.profile && !vpn.server.trim()) return { error: "请导入 .ovpn，或填写 OpenVPN 服务器地址" };
      if (!vpn.profile && (!port || port <= 0 || port > 65535)) return { error: "OpenVPN 节点必须填写有效端口（1-65535）" };
      if (!vpn.profile && !vpn.ca.trim()) return { error: "OpenVPN 节点必须提供 CA 证书内容" };
      if (openVPNPreview?.requires_user_password && (!vpn.username.trim() || !vpn.password)) return { error: "该配置需要填写 auth-user-pass 用户名和密码" };
      if (openVPNPreview?.requires_private_key_passphrase && !vpn.privateKeyPassphrase) return { error: "该配置的私钥已加密，请填写 askpass 私钥口令" };
      if (["tun", "both"].includes(vpn.accessMode) && !vpn.target.trim()) return { error: "TUN 访问方式必须填写目标 IP 或 CIDR" };
      const groupPort = vpn.groupPort.trim() ? Number.parseInt(vpn.groupPort, 10) : 0;
      if (vpn.groupPort.trim() && (!groupPort || groupPort < 1 || groupPort > 65535)) return { error: "代理入口端口必须为 1-65535，留空可自动分配" };
      return {
        setup: {
          name,
          profile: vpn.profile,
          server: vpn.server.trim(),
          server_port: port || 0,
          proto: vpn.proto,
          ca: vpn.ca,
          cert: vpn.cert,
          key: vpn.key,
          tls_auth: vpn.tlsAuth,
          username: vpn.username.trim(),
          password: vpn.password,
          private_key_passphrase: vpn.privateKeyPassphrase,
          udp: vpn.openVPNUDP,
          dialer_proxy: vpn.dialerProxy.trim(),
          group_name: vpn.groupName.trim(),
          port: groupPort,
          access_mode: vpn.accessMode,
          target: vpn.target.trim(),
        },
      };
    }
    let extra;
    try {
      extra = JSON.parse(vpn.json);
    } catch {
      return { error: "出站 JSON 解析失败，请检查格式" };
    }
    if (!extra || typeof extra !== "object" || Array.isArray(extra)) return { error: "出站 JSON 必须是对象" };
    return { proxy: { ...extra, name, type: vpn.type } };
  }

  /**
   * submitManualNode 新增一个手动节点并在成功后复位对话框。
   *
   * 功能说明：
   * 链接模式提交 `{url, name?}`；VPN 模式提交 `{proxy}`（结构化隧道出站映射），
   * 两者对应后端 POST /api/manual-nodes 的二选一入口。保存后后台只重建现有节点，
   * 不会连带下载任何订阅。
   *
   * 参数说明：
   * - event: React.FormEvent<HTMLFormElement>，表单提交事件。
   *
   * 返回值说明：
   * 返回 Promise<void>。
   *
   * 可能的异常/错误情况：
   * 地址为空或重复提交时直接返回；VPN 表单校验失败在对话框内联提示；
   * 后端错误由 onPost 展示且对话框保持打开。
   */
  async function submitManualNode(event) {
    event.preventDefault();
    if (adding) return;
    let body;
    let endpoint = "/api/manual-nodes";
    if (addMode === "vpn") {
      const { proxy, setup, error } = buildVPNProxy();
      if (error) {
        setAddError(error);
        return;
      }
      if (setup) {
        endpoint = vpn.type === "openvpn" ? "/api/openvpn/setups" : "/api/tailscale/setups";
        body = setup;
      } else {
        body = { proxy };
      }
    } else {
      const url = forms.manualURL.trim();
      if (!url) return;
      const name = forms.manualName.trim();
      body = name ? { url, name } : { url };
    }
    setAdding(true);
    try {
      const tailscaleSetup = endpoint === "/api/tailscale/setups";
      const openVPNSetup = endpoint === "/api/openvpn/setups";
      const saved = await onPost(endpoint, body, tailscaleSetup ? "Tailscale 接入配置已提交" : openVPNSetup ? "OpenVPN 接入已创建" : "节点已添加，后台重建现有节点中");
      if (saved) {
        if (!dedicatedTunnel) {
          onForm("manualURL", "");
          onForm("manualName", "");
        }
        setAddError("");
        if (tailscaleSetup) {
          setEnrollment({ name: vpn.name.trim(), state: "starting", message: "正在启动 mihomo/tsnet 注册流程" });
        } else {
          setOpenVPNPreview(null);
          setVpn({ ...EMPTY_VPN_FORM, type: dedicatedTunnel ? workspace : VPN_TYPE_OPTIONS[0].value });
          setAddMode(dedicatedTunnel ? "vpn" : "url");
          setAddOpen(false);
        }
      }
    } finally {
      setAdding(false);
    }
  }

  /**
   * copyRegistrationURL 复制本次 tsnet 生成的一次性注册链接，便于交给 Headscale
   * 管理员批准。
   *
   * 参数说明：无；链接来自后端内存注册状态。
   *
   * 返回值说明：返回 Promise<void>。
   *
   * 可能的异常/错误情况：浏览器拒绝剪贴板权限时在对话框内显示错误，不清除链接。
   */
  async function copyRegistrationURL() {
    if (!enrollment?.registration_url) return;
    try {
      await navigator.clipboard.writeText(enrollment.registration_url);
      setAddError("");
    } catch (error) {
      setAddError(`复制失败：${error.message}`);
    }
  }

  /**
   * copyTailscaleApprovalCommand 复制 Headscale 当前版本推荐的管理员审批命令。
   *
   * 参数说明：无；Auth ID 来自后端对 tsnet 用户注册链接的严格解析。
   *
   * 返回值说明：返回 Promise<void>；成功后审批命令进入系统剪贴板。
   *
   * 可能的异常/错误情况：尚未取得 Auth ID 时直接返回；浏览器拒绝剪贴板权限时
   * 在对话框内显示错误。命令保留 `<USER>` 占位符，因为 proxyd 不持有 Headscale
   * 用户目录或管理凭据，管理员复制后必须替换为实际用户。
   */
  async function copyTailscaleApprovalCommand() {
    if (!enrollment?.auth_id) return;
    const command = `headscale auth register --user <USER> --auth-id ${enrollment.auth_id}`;
    try {
      await navigator.clipboard.writeText(command);
      setAddError("");
    } catch (error) {
      setAddError(`复制失败：${error.message}`);
    }
  }

  /**
   * closeAddDialog 关闭节点对话框并清空一次性创建状态。
   *
   * 参数说明：无。
   *
   * 返回值说明：无；对话框关闭后，下次打开会从空白表单开始。
   *
   * 可能的异常/错误情况：无。这里只结束管理面交互，不删除已经提交的节点；需要
   * 撤销创建时必须使用“终止并删除”，避免把关闭窗口误解为配置回滚。
   */
  function closeAddDialog() {
    setAddOpen(false);
    setAddError("");
    setEnrollment(null);
    setOpenVPNPreview(null);
    setVpn({ ...EMPTY_VPN_FORM, type: dedicatedTunnel ? workspace : VPN_TYPE_OPTIONS[0].value });
    setAddMode(dedicatedTunnel ? "vpn" : "url");
  }

  /**
   * terminateTailscaleSetup 终止当前失败、启动中或等待审批的 Tailscale 接入。
   *
   * 参数说明：无；目标名称来自后端已确认创建成功的 enrollment 状态。
   *
   * 返回值说明：返回 Promise<void>；删除成功后关闭并重置表单，允许立即同名重建。
   *
   * 可能的异常/错误情况：用户取消确认或后端事务失败时保留当前对话框和状态；
   * onDelete 负责展示确认框及错误 toast，重复点击由 terminating 状态阻止。
   */
  async function terminateTailscaleSetup() {
    if (!enrollment?.name || terminating) return;
    setTerminating(true);
    try {
      const removed = await onDelete(
        `/api/tailscale/setups/${encodeURIComponent(enrollment.name)}`,
        "Tailscale 接入已终止并删除",
        `Tailscale 接入 ${enrollment.name}`,
      );
      if (removed) closeAddDialog();
    } finally {
      setTerminating(false);
    }
  }

  /**
   * deleteManualNode 删除手动节点；Tailscale/OpenVPN 一体化节点走专用聚合删除接口。
   *
   * 参数说明：node 是 overview.manual_nodes 中的脱敏手动节点记录。
   *
   * 返回值说明：返回 onDelete 的 Promise<boolean>，调用方无需等待结果。
   *
   * 可能的异常/错误情况：通用节点按下标删除；两类一体化 VPN 使用名称删除节点、
   * 接入组与受管路由。确认取消或后端失败由 onDelete 统一处理并保留现有配置。
   */
  function deleteManualNode(node) {
    const tailscale = node.type === "tailscale" && node.name;
    const openvpn = node.type === "openvpn" && node.name;
    const endpoint = tailscale
      ? `/api/tailscale/setups/${encodeURIComponent(node.name)}`
      : openvpn
        ? `/api/openvpn/setups/${encodeURIComponent(node.name)}`
        : `/api/manual-nodes/${node.index}`;
    const label = tailscale ? `Tailscale 接入 ${node.name}` : openvpn ? `OpenVPN 接入 ${node.name}` : `手动节点 ${node.name || "未命名节点"}`;
    const success = tailscale ? "Tailscale 接入已删除" : openvpn ? "OpenVPN 接入已删除" : "节点已删除，刷新中";
    return onDelete(endpoint, success, label);
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

  const pageMeta = workspace === "tailscale"
    ? {
        eyebrow: "VPN 接入",
        title: "Tailscale",
        detail: "管理由 mihomo 托管的 Tailnet/Headscale 客户端、代理入口与透明访问路由。",
        addLabel: "添加 Tailscale",
        listTitle: "Tailscale 接入",
        listDetail: "每条接入由节点、策略组、代理端口及可选 TUN 规则共同组成，删除时会作为一个整体清理。",
      }
    : workspace === "openvpn"
      ? {
          eyebrow: "VPN 接入",
          title: "OpenVPN",
          detail: "管理 mihomo 进程内运行的 OpenVPN 客户端出站，无需安装本机 OpenVPN 客户端。",
          addLabel: "添加 OpenVPN",
          listTitle: "OpenVPN 出站",
          listDetail: "OpenVPN 作为隧道出口参与策略分组；应用直连私网地址时仍需 TUN 或等价流量入口。",
        }
      : {
          eyebrow: "代理资源",
          title: "代理节点",
          detail: "跨订阅查看健康状态、延迟与稳定端口分配。",
          addLabel: "添加节点",
          listTitle: "手动节点源",
          listDetail: "这里管理原始链接与高级隧道出站；Tailscale 和 OpenVPN 请前往各自的独立菜单。",
        };

  return (
    <div className="stack">
      <PageHeader eyebrow={pageMeta.eyebrow} title={pageMeta.title} detail={pageMeta.detail}>
        <div className="page-actions">
          {!dedicatedTunnel && <Button disabled={Boolean(busy)} loading={busy === "测速"} variant="outline" type="button" onClick={onTest}>{busy !== "测速" && <Gauge size={16} aria-hidden="true" />}{busy === "测速" ? "测速中…" : "测试全部"}</Button>}
          <Button type="button" onClick={() => setAddOpen(true)}><Plus size={16} aria-hidden="true" />{pageMeta.addLabel}</Button>
        </div>
      </PageHeader>

      {!dedicatedTunnel && (
        <>
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
            testing={testing}
            onMainNode={onMainNode}
          />
        </>
      )}

      {dedicatedTunnel ? (
        <TunnelResources
          groups={managedTunnelGroups}
          manualNodes={managedManualNodes}
          overviewNodes={overview.nodes || []}
          pageMeta={pageMeta}
          runtimeNodes={managedTunnelRuntimeNodes}
          onDelete={deleteManualNode}
        />
      ) : managedManualNodes.length > 0 ? (
        <section className="panel compact-panel">
          <PanelTitle title={pageMeta.listTitle} detail={pageMeta.listDetail} />
          <ul className="item-list compact-list">
            {managedManualNodes.map((node) => (
              <li key={node.index}>
                <b>{node.name || "未命名节点"}</b>
                {node.type ? (
                  <>
                    <Badge variant="secondary">隧道</Badge>
                    <code>{manualProxySummary(node)}</code>
                  </>
                ) : (
                  <code>{node.url}</code>
                )}
                <Button aria-label={`删除手动节点 ${node.name || node.index}`} size="icon" variant="destructive-ghost" type="button" onClick={() => deleteManualNode(node)}><Trash2 size={16} aria-hidden="true" /></Button>
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      <Dialog open={addOpen} onOpenChange={(open) => { if (open) setAddOpen(true); else closeAddDialog(); }}>
        <DialogContent className={classNames("node-dialog", dedicatedTunnel && "tunnel-dialog")}>
          <DialogClose className="dialog-close" aria-label="关闭添加节点对话框"><X size={16} aria-hidden="true" /></DialogClose>
          <form onSubmit={submitManualNode}>
            <DialogHeader>
              <DialogTitle>{dedicatedTunnel ? pageMeta.addLabel : "添加手动节点"}</DialogTitle>
              <DialogDescription>
                {workspace === "tailscale"
                  ? "一次完成认证、mihomo 出站、接入策略组、代理入口与可选 TUN 路由配置。"
                  : workspace === "openvpn"
                    ? "导入 .ovpn 或手工填写配置，一次创建 OpenVPN 出站、策略组、代理入口与可选 TUN 路由。"
                    : addMode === "vpn"
                      ? "高级隧道使用 JSON 参数录入；Tailscale 和 OpenVPN 已移至独立管理页。"
                  : "粘贴完整节点链接；名称留空时使用节点自带名称。"}
              </DialogDescription>
            </DialogHeader>
            <div className="dialog-form node-dialog-form">
              {!dedicatedTunnel && <SegmentedControl
                ariaLabel="节点录入方式"
                value={addMode}
                onValueChange={setAddMode}
                options={[{ value: "url", label: "链接" }, { value: "vpn", label: "高级隧道" }]}
              />}
              {addMode === "url" ? (
                <>
                  <Field label="节点地址" hint="支持 socks5://、ss:// 等完整链接"><Input autoFocus value={forms.manualURL} placeholder="socks5://user:pass@host:1080" onChange={(event) => onForm("manualURL", event.target.value)} /></Field>
                  <Field label="显示名称" hint="可选"><Input value={forms.manualName} placeholder="例如：东京备用" onChange={(event) => onForm("manualName", event.target.value)} /></Field>
                </>
              ) : (
                <>
                  {!dedicatedTunnel && <Field label="隧道类型">
                    <Select
                      ariaLabel="VPN 节点隧道类型"
                      value={vpn.type}
                      onValueChange={(value) => updateVPN("type", value)}
                      options={VPN_TYPE_OPTIONS}
                    />
                  </Field>}
                  <Field label="节点名称" hint="必填，作为分组出口选项显示"><Input value={vpn.name} placeholder={workspace === "openvpn" ? "例如：office-vpn" : "例如：ts-exit"} onChange={(event) => updateVPN("name", event.target.value)} /></Field>
                  {vpn.type === "tailscale" && (
                    <>
                      <div className="notice-row">
                        <CircleAlert size={16} aria-hidden="true" />
                        <span>一次保存会原子创建 mihomo/tsnet 出站、策略组、代理入口与可选 TUN 规则，不调用本机 Tailscale SDK 或客户端。</span>
                      </div>
                      <Field label="认证方式">
                        <Select
                          ariaLabel="Tailscale 认证方式"
                          value={vpn.authMode}
                          onValueChange={(value) => updateVPN("authMode", value)}
                          options={[
                            { value: "approval", label: "管理员审批（推荐）" },
                            { value: "auth-key", label: "Auth Key 自动注册" },
                          ]}
                        />
                      </Field>
                      {vpn.authMode === "auth-key" && (
                        <Field label="Auth Key" hint="管理员预先生成的注册密钥"><Input autoComplete="off" type="password" value={vpn.authKey} placeholder="tskey-…" onChange={(event) => updateVPN("authKey", event.target.value)} /></Field>
                      )}
                      <Field label="控制服务地址" hint="Headscale 填完整 URL；官方 Tailscale 可填 controlplane 地址"><Input value={vpn.controlURL} placeholder="https://hs.campone.cc" onChange={(event) => updateVPN("controlURL", event.target.value)} /></Field>
                      <Field label="主机名" hint="可选，在 Tailnet 中显示的名称"><Input value={vpn.hostname} placeholder="proxyd-exit" onChange={(event) => updateVPN("hostname", event.target.value)} /></Field>
                      <div className="vpn-field-row">
                        <Field label="接入策略组" hint="留空自动使用 节点名-access"><Input value={vpn.groupName} placeholder="campone-access" onChange={(event) => updateVPN("groupName", event.target.value)} /></Field>
                        <Field label="代理入口端口" hint="留空从 43000 起自动分配"><Input min="1" max="65535" type="number" value={vpn.groupPort} placeholder="自动分配" onChange={(event) => updateVPN("groupPort", event.target.value)} /></Field>
                      </div>
                      <Field label="访问方式">
                        <Select
                          ariaLabel="Tailnet 访问方式"
                          value={vpn.accessMode}
                          onValueChange={(value) => updateVPN("accessMode", value)}
                          options={[
                            { value: "proxy", label: "本地代理端口" },
                            { value: "tun", label: "TUN 透明访问" },
                            { value: "both", label: "代理端口 + TUN" },
                          ]}
                        />
                      </Field>
                      {["tun", "both"].includes(vpn.accessMode) && (
                        <Field label="TUN 目标" hint="只接管明确的 Tailnet IP/CIDR，避免误路由"><Input value={vpn.target} placeholder="100.64.0.1 或 100.64.0.0/10" onChange={(event) => updateVPN("target", event.target.value)} /></Field>
                      )}
                      <Field label="Exit Node" hint="可选；填写节点 IP/名称或 auto:any。留空时仅访问 Tailnet/子网路由"><Input value={vpn.exitNode} placeholder="100.64.0.1 或 auto:any" onChange={(event) => updateVPN("exitNode", event.target.value)} /></Field>
                      <Field label="底层拨号出口" hint="可选，mihomo 的代理或策略组名称；控制面、DERP 与 STUN 将经该出口"><Input value={vpn.dialerProxy} placeholder="例如：PROXY" onChange={(event) => updateVPN("dialerProxy", event.target.value)} /></Field>
                      <Field label="底层 IP 偏好">
                        <Select
                          ariaLabel="Tailscale 底层 IP 偏好"
                          value={vpn.ipVersion}
                          onValueChange={(value) => updateVPN("ipVersion", value)}
                          options={[
                            { value: "dual", label: "双栈" },
                            { value: "ipv4-prefer", label: "IPv4 优先" },
                            { value: "ipv6-prefer", label: "IPv6 优先" },
                            { value: "ipv4", label: "仅 IPv4" },
                            { value: "ipv6", label: "仅 IPv6" },
                          ]}
                        />
                      </Field>
                      <div className="grid gap-2 rounded-md border bg-muted/30 p-3">
                        <UISwitch checked={vpn.acceptRoutes} label="接受 Tailnet 发布的子网路由" onCheckedChange={(value) => updateVPN("acceptRoutes", value)} />
                        <UISwitch checked={vpn.udp} label="允许业务流量使用 UDP" onCheckedChange={(value) => updateVPN("udp", value)} />
                        <UISwitch checked={vpn.ephemeral} label="使用临时 Tailscale 设备身份" onCheckedChange={(value) => updateVPN("ephemeral", value)} />
                        {vpn.exitNode.trim() && (
                          <UISwitch checked={vpn.exitNodeAllowLANAccess} label="使用 Exit Node 时仍允许访问本地局域网" onCheckedChange={(value) => updateVPN("exitNodeAllowLANAccess", value)} />
                        )}
                      </div>
                      {enrollment && (
                        <div className="notice-row" role="status">
                          <CircleAlert size={16} aria-hidden="true" />
                          <div className="stack gap-2">
                            <strong>{enrollment.state === "connected" ? "已加入 Tailnet" : enrollment.state === "waiting_approval" ? "等待管理员批准" : enrollment.state === "error" ? "注册失败" : "正在发起注册"}</strong>
                            <span>{enrollment.message}</span>
                            {enrollment.auth_id && (
                              <div className="stack gap-2">
                                <div className="page-actions">
                                  <code className="break-all">Auth ID：{enrollment.auth_id}</code>
                                  <Button size="sm" variant="outline" type="button" onClick={copyTailscaleApprovalCommand}><Copy size={14} aria-hidden="true" />复制审批命令</Button>
                                </div>
                                <small>管理员将命令中的 &lt;USER&gt; 替换为实际 Headscale 用户后执行。</small>
                              </div>
                            )}
                            {enrollment.registration_url && (
                              <div className="page-actions">
                                <code className="break-all">{enrollment.registration_url}</code>
                                <Button size="sm" variant="outline" type="button" onClick={copyRegistrationURL}><Copy size={14} aria-hidden="true" />复制注册链接</Button>
                              </div>
                            )}
                          </div>
                        </div>
                      )}
                    </>
                  )}
                  {vpn.type === "openvpn" && (
                    <>
                      <div className="notice-row">
                        <CircleAlert size={16} aria-hidden="true" />
                        <span>一次保存会原子创建 OpenVPN 出站、单成员策略组、固定代理入口与可选 TUN 路由；无需安装本机 OpenVPN 客户端。</span>
                      </div>
                      <Field label="导入 .ovpn" hint="推荐使用包含 <ca>/<cert>/<key> 的内联配置，最大 1 MiB">
                        <input
                          accept=".ovpn,.conf,text/plain"
                          aria-label="导入 OpenVPN 配置文件"
                          className="openvpn-profile-input"
                          disabled={importingOpenVPN}
                          type="file"
                          onChange={importOpenVPNProfile}
                        />
                      </Field>
                      {vpn.profile && openVPNPreview && (
                        <div className="openvpn-import-summary">
                          <div>
                            <strong>{vpn.profileName}</strong>
                            <span>{openVPNPreview.server}:{openVPNPreview.port} · {String(openVPNPreview.proto || "udp").toUpperCase()}</span>
                          </div>
                          <Button size="sm" variant="outline" type="button" onClick={clearOpenVPNProfile}><X size={14} aria-hidden="true" />移除文件</Button>
                          {openVPNPreview.warnings?.length > 0 && (
                            <ul>{openVPNPreview.warnings.map((warning) => <li key={warning}>{warning}</li>)}</ul>
                          )}
                        </div>
                      )}
                      {!vpn.profile && (
                        <>
                          <Field label="服务器地址" hint="未导入 .ovpn 时必填"><Input value={vpn.server} placeholder="vpn.example.com" onChange={(event) => updateVPN("server", event.target.value)} /></Field>
                          <div className="vpn-field-row">
                            <Field label="端口" hint="必填"><Input min="1" max="65535" type="number" value={vpn.port} placeholder="1194" onChange={(event) => updateVPN("port", event.target.value)} /></Field>
                            <Field label="传输协议">
                              <Select
                                ariaLabel="OpenVPN 传输协议"
                                value={vpn.proto}
                                onValueChange={(value) => updateVPN("proto", value)}
                                options={[{ value: "udp", label: "UDP" }, { value: "tcp", label: "TCP" }]}
                              />
                            </Field>
                          </div>
                          <Field label="CA 证书" hint="必填，粘贴 PEM 全文"><textarea aria-label="OpenVPN CA 证书" className={VPN_TEXTAREA_CLASS} value={vpn.ca} placeholder="-----BEGIN CERTIFICATE-----…" onChange={(event) => updateVPN("ca", event.target.value)} /></Field>
                          <Field label="客户端证书" hint="可选；与客户端私钥成对提供"><textarea aria-label="OpenVPN 客户端证书" className={VPN_TEXTAREA_CLASS} value={vpn.cert} onChange={(event) => updateVPN("cert", event.target.value)} /></Field>
                          <Field label="客户端私钥" hint="可选，PEM 全文；提交后仅服务端保存"><textarea aria-label="OpenVPN 客户端私钥" className={VPN_TEXTAREA_CLASS} value={vpn.key} onChange={(event) => updateVPN("key", event.target.value)} /></Field>
                          <Field label="TLS-Auth 密钥" hint="可选"><textarea aria-label="OpenVPN TLS-Auth 密钥" className={VPN_TEXTAREA_CLASS} value={vpn.tlsAuth} onChange={(event) => updateVPN("tlsAuth", event.target.value)} /></Field>
                        </>
                      )}
                      <div className="vpn-field-row">
                        <Field label="用户名" hint="auth-user-pass；配置不需要时可留空"><Input autoComplete="off" value={vpn.username} onChange={(event) => updateVPN("username", event.target.value)} /></Field>
                        <Field label="登录密码" hint="OpenVPN 用户密码（pass）"><Input autoComplete="new-password" type="password" value={vpn.password} onChange={(event) => updateVPN("password", event.target.value)} /></Field>
                      </div>
                      {(openVPNPreview?.requires_private_key_passphrase || (!vpn.profile && vpn.key.includes("ENCRYPTED"))) && (
                        <Field label="私钥口令" hint="askpass；仅用于本次解密，不会保存口令"><Input autoComplete="new-password" type="password" value={vpn.privateKeyPassphrase} onChange={(event) => updateVPN("privateKeyPassphrase", event.target.value)} /></Field>
                      )}
                      <div className="vpn-field-row">
                        <Field label="接入策略组" hint="留空自动使用 节点名-access"><Input value={vpn.groupName} placeholder="office-vpn-access" onChange={(event) => updateVPN("groupName", event.target.value)} /></Field>
                        <Field label="代理入口端口" hint="留空从 43000 起自动分配"><Input min="1" max="65535" type="number" value={vpn.groupPort} placeholder="自动分配" onChange={(event) => updateVPN("groupPort", event.target.value)} /></Field>
                      </div>
                      <Field label="访问方式">
                        <Select
                          ariaLabel="OpenVPN 私网访问方式"
                          value={vpn.accessMode}
                          onValueChange={(value) => updateVPN("accessMode", value)}
                          options={[
                            { value: "proxy", label: "本地代理端口" },
                            { value: "tun", label: "TUN 透明访问" },
                            { value: "both", label: "代理端口 + TUN" },
                          ]}
                        />
                      </Field>
                      {["tun", "both"].includes(vpn.accessMode) && (
                        <Field label="TUN 目标" hint="只接管 VPN 内网 IP/CIDR，避免影响普通网页流量"><Input value={vpn.target} placeholder="10.0.0.0/8 或 192.168.1.20" onChange={(event) => updateVPN("target", event.target.value)} /></Field>
                      )}
                      <Field label="底层拨号出口" hint="可选，填写 mihomo 节点或策略组名称"><Input value={vpn.dialerProxy} placeholder="例如：PROXY" onChange={(event) => updateVPN("dialerProxy", event.target.value)} /></Field>
                      <div className="grid gap-2 rounded-md border bg-muted/30 p-3">
                        <UISwitch checked={vpn.openVPNUDP} label="允许经 OpenVPN 转发 UDP 业务流量" onCheckedChange={(value) => updateVPN("openVPNUDP", value)} />
                      </div>
                    </>
                  )}
                  {ADVANCED_VPN_TYPES.includes(vpn.type) && (
                    <Field label="出站参数" hint="JSON 对象，字段原样透传给 mihomo；name 与 type 由上方表单生成">
                      <textarea
                        aria-label={`${vpn.type} 出站参数 JSON`}
                        className={VPN_TEXTAREA_CLASS}
                        value={vpn.json}
                        placeholder={vpn.type === "wireguard" ? '{"server":"…","port":51820,"private-key":"…","public-key":"…"}' : "{}"}
                        onChange={(event) => updateVPN("json", event.target.value)}
                      />
                    </Field>
                  )}
                  {addError && <p className="dialog-error" role="alert">{addError}</p>}
                </>
              )}
            </div>
            <DialogFooter>
              {enrollment && enrollment.state !== "connected" && (
                <Button loading={terminating} variant="destructive" type="button" onClick={terminateTailscaleSetup}>终止并删除</Button>
              )}
              <Button variant="outline" type="button" onClick={closeAddDialog}>{enrollment ? "关闭" : "取消"}</Button>
              {!enrollment && <Button disabled={addMode === "url" ? !forms.manualURL.trim() : !vpn.name.trim()} loading={adding} type="submit">{vpn.type === "tailscale" ? "创建并发起注册" : dedicatedTunnel ? pageMeta.addLabel : addMode === "vpn" ? "添加隧道" : "添加节点"}</Button>}
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </div>
  );
}

/**
 * TailscalePage 渲染代理域内独立的 Tailscale 接入管理页。
 *
 * 参数说明：
 * - overview: object，包含已脱敏手动节点的代理概览。
 * - onDelete/onPost: Function，执行聚合删除与事务写入的上层回调。
 *
 * 返回值说明：返回限定为 `tailscale` 工作区的 React 页面元素。
 *
 * 可能的异常/错误情况：后端注册、删除或热更新错误由共享页面及上层 toast 呈现。
 */
export function TailscalePage({ overview, onDelete, onPost }) {
  return <NodesPage overview={overview} onDelete={onDelete} onPost={onPost} workspace="tailscale" />;
}

/**
 * OpenVPNPage 渲染代理域内独立的 OpenVPN 出站管理页。
 *
 * 参数说明：
 * - overview: object，包含已脱敏手动节点的代理概览。
 * - onDelete/onPost: Function，执行节点删除与事务写入的上层回调。
 *
 * 返回值说明：返回限定为 `openvpn` 工作区的 React 页面元素。
 *
 * 可能的异常/错误情况：字段校验在共享表单内完成，后端失败由上层 toast 呈现。
 */
export function OpenVPNPage({ overview, onDelete, onPost }) {
  return <NodesPage overview={overview} onDelete={onDelete} onPost={onPost} workspace="openvpn" />;
}

/**
 * TunnelResources 展示某一种隧道在 proxyd 中形成的节点与关联策略组。
 *
 * 功能说明：
 * 独立 VPN 页面不再只展示脱敏后的原始配置，而是把配置节点与 mihomo 运行时节点
 * 对齐，并单独列出引用这些节点的策略组，便于确认入口端口和当前可用成员数量。
 *
 * 参数说明：
 * - groups: Array<object>，已筛选为当前隧道类型的关联策略组。
 * - manualNodes: Array<object>，当前类型的脱敏手动节点配置。
 * - overviewNodes: Array<object>，全部 mihomo 运行时节点，用于计算分组健康度。
 * - pageMeta: object，当前独立页面的标题和说明。
 * - runtimeNodes: Array<object>，与 manualNodes 同名、同类型的运行时节点。
 * - onDelete: Function，删除当前节点或 Tailscale 聚合接入的回调。
 *
 * 返回值说明：返回节点面板和关联策略组面板两个 React 元素。
 *
 * 可能的异常/错误情况：运行时刷新期间可能暂时找不到节点，此时明确显示“未加载”；
 * 分组中的失效成员仍计入总数，避免把陈旧引用误报成健康配置。
 */
function TunnelResources({ groups, manualNodes, overviewNodes, pageMeta, runtimeNodes, onDelete }) {
  const runtimeByName = new Map(runtimeNodes.map((node) => [node.name, node]));
  return (
    <div className="tunnel-resource-stack">
      <section className="panel compact-panel">
        <PanelTitle title={pageMeta.listTitle} detail={pageMeta.listDetail} />
        {manualNodes.length > 0 ? (
          <ul className="item-list compact-list tunnel-resource-list">
            {manualNodes.map((node) => {
              const runtimeNode = runtimeByName.get(node.name);
              const managedTailscale = isMihomoManagedTailscale(runtimeNode);
              return (
                <li key={node.index}>
                  <div className="tunnel-resource-name">
                    <b>{node.name || "未命名节点"}</b>
                    <Badge variant="secondary">隧道节点</Badge>
                  </div>
                  <code>{manualProxySummary(node)}</code>
                  {runtimeNode && !managedTailscale && <span className={delayClass(runtimeNode)}>{formatDelay(runtimeNode)}</span>}
                  <StatusBadge
                    ok={Boolean(runtimeNode?.alive)}
                    text={runtimeNode ? (managedTailscale ? "mihomo 托管" : runtimeNode.alive ? "可用" : "失效") : "未加载"}
                  />
                  <Button aria-label={`删除手动节点 ${node.name || node.index}`} size="icon" variant="destructive-ghost" type="button" onClick={() => onDelete(node)}><Trash2 size={16} aria-hidden="true" /></Button>
                </li>
              );
            })}
          </ul>
        ) : (
          <p className="tunnel-resource-empty">尚未配置 {pageMeta.title}，点击“{pageMeta.addLabel}”创建第一条接入。</p>
        )}
      </section>

      <section className="panel compact-panel">
        <PanelTitle title="关联策略分组" detail="仅展示直接引用当前页面隧道节点，或动态包含这些节点的策略组。" />
        {groups.length > 0 ? (
          <ul className="item-list compact-list tunnel-group-list">
            {groups.map((group) => {
              const memberNames = tunnelGroupMemberNames(group, overviewNodes);
              const aliveCount = memberNames.filter((name) => overviewNodes.some((node) => node.name === name && node.alive)).length;
              return (
                <li key={group.name}>
                  <div className="tunnel-resource-name">
                    <b>{group.name}</b>
                    <Badge variant="outline">{TUNNEL_GROUP_TYPE_LABELS[group.type] || group.type || "自动测速"}</Badge>
                  </div>
                  <code>入口 :{group.port} · 成员 {memberNames.length > 0 ? memberNames.join("、") : "暂无"}</code>
                  {group.type === "select" && group.selected && <span className="tunnel-group-selected">当前：{group.selected}</span>}
                  <StatusBadge ok={aliveCount > 0} text={`${aliveCount}/${memberNames.length} 可用`} />
                </li>
              );
            })}
          </ul>
        ) : (
          <p className="tunnel-resource-empty">暂无引用当前 {pageMeta.title} 节点的策略分组。</p>
        )}
      </section>
    </div>
  );
}

/**
 * tunnelGroupReferencesNodes 判断策略组是否属于当前隧道页面。
 *
 * 参数说明：
 * - group: object，overview.groups 中的策略组。
 * - managedNames: Set<string>，当前页面配置节点的名称集合。
 * - runtimeNodes: Array<object>，当前隧道类型的 mihomo 运行时节点。
 *
 * 返回值说明：固定成员组命中任一节点名、或订阅动态组包含当前运行时节点时返回 true。
 *
 * 可能的异常/错误情况：字段缺失时按空集合处理并返回 false；动态组必须依据运行时
 * 来源判断，因为其成员不会固化在 group.nodes 中。
 */
function tunnelGroupReferencesNodes(group, managedNames, runtimeNodes) {
  if (group?.subscription) {
    return runtimeNodes.some((node) => node.subscription === group.subscription);
  }
  return (group?.nodes || []).some((name) => managedNames.has(name));
}

/**
 * tunnelGroupMemberNames 解析策略组当前应展示的成员名称。
 *
 * 参数说明：
 * - group: object，固定成员或订阅动态成员策略组。
 * - overviewNodes: Array<object>，全部运行时节点。
 *
 * 返回值说明：返回成员名称数组；固定成员保留尚未加载的名称，动态成员按来源展开。
 *
 * 可能的异常/错误情况：字段缺失时返回空数组，不抛出异常。
 */
function tunnelGroupMemberNames(group, overviewNodes) {
  if (group?.subscription) {
    return overviewNodes.filter((node) => node.subscription === group.subscription).map((node) => node.name);
  }
  return group?.nodes || [];
}

/**
 * NodeTable 渲染节点列表表格。
 *
 * 参数说明：
 * - nodes: Array<object>，节点数据。
 * - assignmentMap: Map<string, number>，按来源与节点名索引的稳定端口分配。
 * - mainNode: string，当前主端口固定节点 key。
 * - mappingEnabled: boolean，节点一对一 listener 是否启用。
 * - testing: boolean，节点健康检测进行中；延迟列显示「测速中…」而不是上一轮结果。
 * - onMainNode: Function，设置主端口节点回调。
 *
 * 返回值说明：
 * 返回表格 React 元素。
 *
 * 可能的异常/错误情况：
 * 无；节点不可用时按钮禁用。
 */
function NodeTable({ nodes, assignmentMap = new Map(), mainNode, mappingEnabled = true, testing = false, onMainNode }) {
  const columns = useMemo(
    () => [
      {
        key: "name",
        header: "节点",
        sortable: true,
        width: "34%",
        cell: (node) => (
          <span className={classNames("node-name", !node.alive && "text-muted-foreground")} title={node.name}>
            {node.name}
            {node.tunnel && <Badge variant="secondary">隧道</Badge>}
            {mainNode && node.key === mainNode && <Badge variant="outline">主端口</Badge>}
          </span>
        ),
      },
      { key: "type", header: "类型", sortable: true, width: "100px", cell: (node) => node.type || "-" },
      { key: "subscription", header: "来源", sortable: true, width: "18%", cell: (node) => node.subscription === "manual" ? "手动节点" : node.subscription },
      {
        key: "port",
        header: "稳定端口",
        sortable: true,
        width: "118px",
        cell: (node) => {
          // 隧道节点不参与端口映射（port 恒为 0）是正常状态，展示占位符而非缺失。
          if (node.tunnel) return <span className="text-muted-foreground" title="隧道节点经分组出口使用，不分配独立端口">—</span>;
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
        cell: (node) => testing
          ? <span className="delay-muted">测速中…</span>
          : isMihomoManagedTailscale(node)
            ? <span className="delay-muted" title="未配置 Exit Node；由 mihomo 在首次匹配流量时启动">按需启动</span>
          : <span className={delayClass(node)}>{formatDelay(node)}</span>,
        sortValue: (node) => node.alive && node.delay > 0 ? node.delay : Number.POSITIVE_INFINITY,
      },
      {
        key: "alive",
        header: "状态",
        sortable: true,
        width: "110px",
        cell: (node) => (
          <StatusBadge
            ok={node.alive}
            text={node.alive ? (isMihomoManagedTailscale(node) ? "mihomo 托管" : "可用") : "失效"}
          />
        ),
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
    [assignmentMap, mainNode, mappingEnabled, onMainNode, testing],
  );

  return (
    <Table
      className="data-table"
      columns={columns}
      data={nodes}
      emptyState="暂无节点"
      getRowClassName={(node) => (mainNode && node.key === mainNode ? "main-node-row" : "")}
      getRowId={(node, index) => node.key || `${node.subscription}:${node.name}:${index}`}
      height={tableViewportHeight(nodes.length, 880)}
      minColumnWidth={84}
      resizable
    />
  );
}

/**
 * isMihomoManagedTailscale 判断节点是否处于“配置已加载、连接按需启动”的状态。
 *
 * 参数说明：
 * - node: object，overview 返回的节点记录。
 *
 * 返回值说明：
 * 返回 boolean；Tailscale 节点已进入 mihomo 代理表但没有公网测速延迟时为 true。
 *
 * 可能的异常/错误情况：
 * 无；字段缺失按 false 处理。未配置 Exit Node 的 Tailnet/子网路由不能用公网地址
 * 测速，因此这里明确显示“mihomo 托管”，避免把 delay=0 误解成已验证的公网可用。
 */
function isMihomoManagedTailscale(node) {
  return node?.type === "tailscale" && Boolean(node.alive) && !node.delay;
}

/**
 * manualProxySummary 为结构化（VPN）手动节点生成紧凑摘要。
 *
 * 参数说明：
 * - entry: object，overview.manual_nodes 条目，proxy 字段的凭据已被后端打码。
 *
 * 返回值说明：
 * 返回「类型 · 目标」文本；取不到地址时只显示类型，不泄露凭据字段。
 *
 * 可能的异常/错误情况：
 * 无；缺失字段按空串处理。
 */
function manualProxySummary(entry) {
  const proxy = entry.proxy || {};
  const target = proxy.server
    ? `${proxy.server}${proxy.port ? `:${proxy.port}` : ""}`
    : proxy["control-url"] || "";
  if (entry.type === "tailscale" && proxy["exit-node"]) {
    return `tailscale · Exit ${proxy["exit-node"]}`;
  }
  return target ? `${entry.type} · ${target}` : `${entry.type} 隧道出站`;
}
