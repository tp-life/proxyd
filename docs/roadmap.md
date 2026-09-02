# proxyd 功能规划与实施计划

> 来源：2026-09 功能差距分析 + Web 重构可行性评估。本文档是待实施清单，动工时按批次执行，完成一项勾掉一项。
> 关键决策：**Web 控制台重构（beui + embed）与功能开发合并进行**，所有涉及 UI 的功能直接在新 UI 里落地，避免旧 ui.html 上做完又重写两遍。

## 现状结论

- **功能基础盘已完整**：订阅多源/手动节点、节点-端口映射、快照与缓存兜底、主端口三模式 + main-auto/main-node、auto-port、自定义规则 + 规则 URL、节点分组（url-test）、守护/自启/系统代理、CLI + REST + Web 控制台、external-controller 兼容第三方面板。
- **差距两类**：A. mihomo 尚未暴露的能力；B. 信息可见性与操作成本。
- **前端**：单个 1601 行 vanilla `internal/api/ui.html`（`//go:embed`），前后端仅靠 `/api/*` REST 交互、已完全解耦——重写前端不碰 Go 业务代码。

## 技术路线：beui + embed（已评估可行）

- [beui.dev](https://beui.dev)：React 19 + Tailwind 4 + Motion 的 copy-paste 组件库，shadcn CLI 分发，**源码进仓库、非运行时依赖**；本项目优先通过官方 Registry 集成可用组件，不引入 Radix UI；部分组件 Premium 付费，选型避开。
- 新增 `web/` 前端工程（Vite），构建产物由 Go 嵌入：`//go:embed all:dist`（embed.FS）替换单文件 embed。**单二进制交付不变，无 CDN、无运行时依赖**（不走 CDN 引 React 的免构建路线——代理工具目标环境网络不可靠）。
- 构建策略：**dist 提交进 git**，`make build` 对纯 Go 开发者零 Node 依赖；`make web` 单独构建前端。开发时 Vite dev server proxy → 19091。
- `internal/api/api.go`：静态服务改 embed.FS + SPA fallback（非 /api 路径回 index.html）。旧 ui.html 直接替换不保留。

### 新布局（IA）

```
侧边栏导航（任务分组，窄屏抽屉收起）
├─ 查看：运行概况 / 节点与订阅 / 代理入口
├─ 配置：策略分组 / 访问规则
└─ 诊断与维护：活动连接 / 运行日志 / 系统设置
全局：⌘K Command Palette（切模式、刷新、测速、跳页、搜节点）+ Animated Toast
```

组件映射：beUI Button/Tooltip→命令操作；Switch→布尔开关；Segmented Control→模式切换；Table→节点、端口与连接数据；Animated Number→统计；Animated Toast Stack→统一反馈；Morphing Dialog→危险操作确认。页面切换、列表进入、悬停和状态变化可使用更丰富的动画，同时尊重 `prefers-reduced-motion`。

---

## 第一批：Web 重构骨架 + 现有功能迁移

目标：新前端工程跑通、embed 改造完成、现有五页功能对齐（不加新功能）。

- [x] 搭建 `web/`（Vite + React 19 + Tailwind 4），配置 shadcn registry，并通过 beUI 官方 Registry 源码承载 Button/Switch/Tabs/Table；其余暂未开放的组件按官方交互方向本地实现，不引入 Radix UI
- [x] `internal/api/api.go` embed 改造（embed.FS + SPA fallback）；Makefile 加 `make web`；dist 提交策略落地
- [x] 按新 IA 迁移五页：概览 / 订阅与节点 / 端口映射 / 自定义规则 / 节点分组（功能与现状对齐，含 10 秒轮询、toast、排序等既有交互）
- [x] 全局 Command Palette（切模式/刷新/测速/跳页）与 Animated Toast 统一反馈
- **验证**：已跑 `npm run build`、`go test ./...`、`go vet ./...`、`make build`；已完成 Chrome 桌面端与 390px 移动端走查，并验证移动端菜单、设置页及无横向溢出

## 第二批：新 UI 上落地高感知功能

- [x] **B1 订阅流量/到期信息**：`internal/subscribe/fetch.go` 解析 `subscription-userinfo` 响应头（upload/download/total/expire）并随缓存持久化 → overview API 带 userinfo → 新 UI 节点页订阅标题行显示"已用 87G/500G · 到期日期"（<7 天标黄）；CLI `subs list` 同步显示
- [x] **B2 日志面板**：内存环形缓冲（~1000 条）挂到日志输出（注意 daemon 模式落文件需同时进缓冲）→ `GET /api/logs?tail=N&level=` → 新 UI「日志」页 + CLI `proxyd logs`
- [x] **B3 实时速率条**：概览页顶部，数据源 mihomo `/traffic`（自有 API 以 HTTP 流代理，后端附加 `secret` 鉴权）
- [x] **A2 分组类型 + 按订阅生成组**：`internal/config/config.go` Group 加 `type: url-test|fallback|load-balance` 与 `subscription: <订阅名>`（成员=该订阅当前可用节点，刷新自动跟随）→ `internal/core/gen.go` 生成对应 proxy-groups → 新 UI 分组表单（Multi Select + 类型下拉 + 按订阅选项）+ CLI `groups add` 参数
  - **待决策**：存量配置无 `type` 的默认值——倾向 url-test（行为不变），新 UI 创建时默认推荐 fallback，手册注明
- **验证**：B1 mock 带 userinfo 头响应单测；A2 补 gen_test 用例；B2/B3 API 单测；完整命令见本批完成记录

## 第三批：TUN 模式

- [x] `internal/config/config.go`：`tun:` 段透传 + mihomo `enable` 开关（默认 system/auto-route/auto-detect-interface/dns-hijack；API 请求字段使用 `enabled`）
- [x] `internal/core/gen.go` 输出 tun 段；API + 新 UI 设置页开关 + CLI `proxyd tun on|off|status`（对齐 sysproxy 形态，并二次确认 listener 实际状态）
- [x] **权限处理**（主要工作量）：macOS sudo / Linux setcap / Windows 管理员，权限不足给明确指引
- [x] 手册写清 TUN 与 system-proxy 的关系、与 DNS 预设的联动
- **验证**：开启后 curl 走代理、关闭恢复；UDP 应用验证

## 第四批：打磨项

- [x] **A3 DNS 预设**：`dns-preset: off|fake-ip|redir-host` 三档，手写 `dns:` 优先；TUN 模式默认建议 fake-ip。落点 `internal/core/gen.go`
- [x] **B4 配置导出/导入**：设置页导出 config.yaml（可选 token 打码）+ 导入恢复。落点 `internal/api/api.go`
- [x] **B5 首次运行引导**：无配置无订阅时打印三行引导。落点 `cmd/proxyd/main.go`
- [x] **B6 版本检查**：启动异步查 GitHub Releases，概览页提示，可配置关闭
- [x] **A5 include 过滤**：与 exclude 对称的正则。落点 `internal/subscribe/merge.go`
- [x] **A4 链式代理透传**（可选）：透传订阅节点 `dialer-proxy`，处理跨订阅同名重写、节点身份、proxy-only 依赖与两阶段完整链路测速；支持指向节点/策略组。mihomo 已移除 `relay` 组，按官方迁移方向不再生成该类型；不做 UI
- [x] **B3b 连接面板**（可选）：基于 mihomo `/connections`，独立高频 API 代理（后端附加 secret）+ 活动连接页，查看域名/入口端口/进程/出口链，支持搜索、协议筛选和关闭连接

---

## 明确不做（避免范围漂移）

- 自建面板替代 metacubexd：external-controller 已兼容成熟面板
- 订阅转换/协议互转：subconverter 生态已解决，偏离"端口映射"定位
- Web 远端访问/多用户鉴权：本机工具定位，现有 `secret` + 反代足够
- CDN 引入前端依赖：目标环境网络不可靠，必须自包含

## 每批完成后的固定动作

1. `go test ./...`（含 e2e/）；前端构建产物随源码提交，`make build` 验证自包含
2. 更新 `README.md` 功能列表、`docs/manual.md` 对应章节、`configs/config.example.yaml` 注释（项目惯例：文档与配置示例同步）
3. 本文件中对应项打勾
