# 远程连接模块（remote）功能计划

> 来源：2026-09 远程模块功能评估（对照 tailcat 数据面能力）。本文档覆盖评估中「高价值」与「中价值」
> 共 7 项，低价值项（tailcat cp/recv、SOCKS/exit-node、托管 derper）不做，理由见文末。
> 所有功能按 AGENTS.md 垂直模块规则落位在 remote 模块各层，不碰 proxy 模块文件。

## 前置约束与共同决策

- **tailcat 上游不承诺 API/wire format 稳定**：凡依赖其内部状态暴露的能力（连接路径、RTT、按连接归因），
  采集点统一封装在 `internal/remote` 薄层内，升级依赖时只需重验这一层。
- **Web 终端的安全前提**：Web 控制台默认绑 `127.0.0.1` 且无多用户鉴权，而 Web 终端等价于给出本机 shell。
  因此 Web 终端必须：默认关闭、独立开关持久化、开启时若 `api-listen` 非回环地址则显式警告。
- **allow 条目结构一次改到位**：TTL（M3a）与按端口授权（M3b）都扩展白名单条目，统一为
  `{name, key, expires_at?, ports?}`，一个批次内完成，避免结构改两次。
- **新依赖需逐个确认**：`xterm.js` + `@xterm/addon-fit`（前端，懒加载 chunk）、`creack/pty`（PTY）。
  前端组件仍走 beUI registry 惯例；xterm 是功能库而非 UI 组件，允许引入。

## M0 前置验证（spike，不写产品代码）

目标：用半天以内的实验确认 tailcat v0.4.0 能提供什么，给 M1/M3 的落法定案。

- [ ] 确认隧道对端状态可暴露哪些字段：直连/中继路径、RTT、累计流量（查 tailcat `Conn`/magicsock 层 API）
- [ ] 确认 `tailcat ping` 的实现路径可否在进程内复用（对端在线状态探测的数据源）
- [ ] 确认 serve 侧能否拿到「单条入连的客户端公钥 + 目标端口」（现有 `attributeClient` 已能做公钥归因，
      需验证是否能挂到每连接回调上做按端口拦截）
- [ ] 产出：每项一段结论写进本文档对应任务下；某项不可行则该任务降级或改方案（备选见任务备注）

## M1 观测性：对端在线状态 + 连接路径可视化

目标：远程设备列表一眼看出「活没活、走直连还是中继、有多快」。

- [ ] **M1a 对端在线状态**：对已保存远端做隧道内探测（复用 M0 的 ping 路径），
      结果含 在线/离线 + RTT。CLI `proxyd remote remotes list` 加状态列；
      Web 远程设备表加「状态」列（手动触发 + 每 30s 自动刷新已展开行）
  - 落点：`internal/remote/remote.go`（探测）、`internal/api/remote.go`（`POST /api/remote/ping`）、
    `cmd/proxyd/remote.go`、`web/src/pages/RemotePage.jsx` + `useRemoteFeed.js`
- [ ] **M1b 连接路径与质量**：服务端状态卡与每条入连显示 直连/DERP 中继、RTT、收发字节。
      数据源即 M0 验证的字段；只读展示，不做控制
  - 落点：`internal/remote`（状态采集薄层）、`Status` 扩字段、Web 服务状态卡 + 白名单条目行
- **最终目标 / 验收**：dev 实例下 `tailcat ping` 与 CLI/Web 显示的 RTT 同量级；
  强制中继（打洞不可能的环境）时路径显示「中继」；直连环境显示「直连」；tailcat 升级只需改 `internal/remote`

## M2 Web 终端

目标：浏览器内经隧道 SSH 登录本机，补齐「没带电脑」应急链路的最后一环（配合已有临时身份）。

- [ ] 后端：WebSocket 端点 `GET /api/remote/terminal`（`coder/websocket` 已在依赖树），
      服务端作为 SSH 客户端经 loopback 接进程内 builtin-ssh（认证=控制台会话本身，隧道即认证）；
      依赖 builtin-ssh 已开启，未开时返回明确引导。PTY 用 `creack/pty`
  - 落点：`internal/remote/server.go`（会话桥接）、`internal/api/remote.go`（WS handler + 开关校验）
- [ ] 配置与开关：`remote.web-terminal: false`（默认关），CLI `proxyd remote web-terminal on|off`，
      Web 服务状态卡开关；`api-listen` 非回环时开启动作二次确认并打印警告
  - 落点：`internal/config/remote.go`、`internal/app/remote.go`（事务模式）
- [ ] 前端：xterm.js + addon-fit，动态 import 懒加载（不进首屏 chunk）；
      服务状态卡「打开终端」按钮 → 全屏 Dialog；resize 同步 PTY 窗口大小
  - 落点：`web/src/pages/RemotePage.jsx` + 新组件 `web/src/components/TerminalDialog.jsx`
- [ ] 体积预算落地验证：二进制增量 ≤1MB、首屏 JS 不变（xterm chunk 按需加载）
- **最终目标 / 验收**：dev 实例开启后浏览器内可登录本机 shell（含 TERM、颜色、窗口缩放正常）；
  关闭开关后 WS 端点 404；`api-listen: 0.0.0.0` 时开启被警告拦截；`npm run build` 产物中 xterm 为独立 chunk

## M3 安全深化：白名单 TTL + 按端口授权 + 连接审计

目标：从「进得来就全通、事后无记录」升级为「最小授权 + 可追溯」。

- [ ] **M3a 白名单条目过期**：条目加 `expires_at`（可空）；连接时校验 + 每分钟清扫过期条目并落盘。
      CLI `proxyd remote allow add <公钥> [别名] --ttl 1h`；Web 表单加「有效期」下拉（永久/1h/1d/7d），
      列表显示剩余时间，过期条目灰显待清扫
- [ ] **M3b 按端口授权**：条目加 `ports`（可空=serve 全开）；非空时该客户端只可连列出的端口。
      拦截点依赖 M0 第 3 项结论；若 tailcat 层拿不到每连接目标端口，备选方案：
      仅对内嵌 SSH 与转发层生效 + 文档注明限制。Web 表单加「限定端口」可选输入
  - 共同落点：`internal/config/remote.go`（条目结构 + 校验）、`internal/remote/server.go`（强制点）、
    `internal/app/remote.go`、`internal/api/remote.go`、CLI/Web 表单与列表
- [ ] **M3c 连接审计日志**：环形缓冲（~500 条，复用 `logbuf` 思路但独立实例避免被代理日志冲掉），
      记录 时间/客户端公钥/别名/目标端口/动作（建立、拒绝、断开）/时长/字节数；
      `GET /api/remote/audit?tail=N`；Web 服务端区加「连接记录」面板；CLI `proxyd remote audit`
  - 落点：`internal/remote`（事件产生）、`internal/api/remote.go`、Web/CLI 各一处
- **最终目标 / 验收**：过期条目到期后新连接被拒、列表自动消失；限定端口的客户端连未授权端口被拒且
  审计中有拒绝记录；审计面板能完整复盘一次「谁何时连了哪个端口持续多久」；配置重启后 TTL/端口限制不丢

## M4 服务端密钥导出/导入

目标：换机迁移不换身份，token 延续。

- [ ] 导出：`proxyd remote keyfile export <路径>` + Web 状态卡「导出密钥」按钮（下载 *.private.json）；
      导出走专用端点（与 token 同样按需获取，不进列表/状态接口）
- [ ] 导入：`proxyd remote keyfile import <路径>` + Web 文件选择；写入内置托管路径（0600），
      隧道重建、token 变更前端显式提示；与现有自定义 `key-file` 逻辑共存（导入即覆盖内置托管密钥）
  - 落点：`internal/app/remote.go`（事务）、`internal/api/remote.go`、CLI/Web
- **最终目标 / 验收**：导出文件可直接被 `tailcat` CLI 当密钥用（格式兼容）；导入后 token 与源机器一致；
  导入非法文件整体回滚不留中间态

## 明确不做（避免范围漂移）

- tailcat cp/recv 原生文件传输：已有 scp 包装，仅对端不暴露 22 时有意义，场景窄
- SOCKS/exit-node（经对端网络出口）：与 proxy 主功能有化学反应，但会模糊 remote/proxy 模块边界，
  要做应独立提案评估，不在本计划内
- 托管/内置自建 derper：`remote.derpmap-url` 已留出路口，用得上的人自己会搭
- Web 控制台多用户/鉴权体系：本机工具定位，Web 终端用开关 + 回环绑定约束即可

## 批次顺序与依赖

```
M0（spike）→ M1（观测性）   M2（Web 终端，独立可并行）
           → M3（安全深化，M3a/M3b 依赖 M0 结论）
M4 无依赖，可插入任何批次
```

## 每批完成后的固定动作

1. `go test ./...`（含 e2e）；动了 `web/src` 必须重新 `make`，dist 产物随仓库提交
2. 更新 `docs/manual.md` 第十节、`configs/config.example.yaml` remote 段注释、CLI 帮助文本
3. 本文件对应项打勾；M0 结论与降级决策直接补写在对应任务下
