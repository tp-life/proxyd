# HANDOFF — PROXY 组选择取代 main-node / main-auto（中断于实施中途）

> 整理时间：2026-09-19。本文件描述当前工作区的完整交接状态。
> **工作区含有三个功能的未提交改动**：①广告拦截（已完成已验证）②局域网共享（已完成已验证）③本次任务 main-node/main-auto 移除 + PROXY 组选择（**进行中，被用户中断**）。

## 原始任务目标

用户决策（已确认，不要重开）：

1. 把 mihomo 内置 PROXY 组变成界面/API 可选、选择持久化的一等公民（补上规则模式下 TUN / gateway / 系统代理 / 主端口兜底出口不可控的缺口）。
2. **完全删除 main-node / main-auto 功能**，不留迁移兼容（用户明确「不用保留」）。「配合规则走某节点」由 PROXY 组选择表达；「完全跳过规则」由节点映射端口和 `mode: global` 表达。
3. PROXY 组成员 = 全部可用节点（含隧道类，替代 ADR 0002 中 main-node 引用隧道节点的能力）+ `AUTO` + `DIRECT`；顺序节点在前、AUTO/DIRECT 殿后，默认仍落成员首位（不改变无持久化选择时的行为）。
4. 选择持久化复用 groupstate（`state-dir/group-selected.json`）+ gen.go `default-selected` 注入，键为 `"PROXY"`。
5. 旧配置残留的 `main-node`/`main-auto` 键无需迁移：非严格 `yaml.Unmarshal` 静默忽略。

## 已完成的工作（本次任务，由 coder subagent 实施，被中途停止）

主包编译通过（`go build -tags "with_gvisor ts_omit_acme" ./...` 成功，2026-09-19 实测）：

- **配置层**：`internal/config/config.go` 删除 `MainAuto`/`MainNode` 字段；`config_test.go` 相应改写（残留 main-node 字样的文件见下，多为注释/测试名）。
- **生成层**：`internal/proxy/core/gen.go` 删除主端口 listener 分支与 `resolveMainInbound`/`MainInboundIsListener`，mixed-port 恒顶层；PROXY 组成员 = 可用节点 + AUTO + DIRECT（`gen.go:190-192`），AUTO 组有节点即生成；`gen_test.go` 已改写。
- **编排层**：`internal/app/proxy_ports.go` 删除 SetMainAuto/SetMainNode；`proxy_groups.go` SetGroupSelected 特判 PROXY；`proxy_refresh.go` 删除两阶段释放；`app.go` 删除 mainListenerOn；`proxy_openvpn.go`/`proxy_tailscale.go` 删除 MainNode 清理块。
- **API 层**：`internal/api/proxy_ports.go` 删除 /api/main-auto、/api/main-node；`proxy_nodes.go` Overview 删除三个 main_* 字段；`proxy_groups.go` 增加内置 PROXY 项（`builtin: true`，见 `proxy_groups.go:27,40`）。
- **CLI/TUI**：`cmd/proxyd/main.go` 删除子命令分发与帮助；`proxy_ports.go` 删除 cmdMainAuto/cmdMainNode；`daemon.go`、`tui_view.go`、`tui_actions.go`、`tui_test.go`、`cli_test.go` 已改为 PROXY select 语义。

## 已修改的文件（git status 全量，含前两个功能）

```
M  cmd/proxyd/{cli_test,daemon,main,proxy_ports,tui_actions,tui_test,tui_view}.go
M  configs/config.example.yaml
M  docs/manual.md
M  internal/api/{api.go,api_test.go,proxy_groups.go,proxy_nodes.go,proxy_ports.go,proxy_vpn_test.go}
M  internal/app/{app.go,proxy_groups.go,proxy_openvpn.go,proxy_ports.go,proxy_refresh.go,proxy_tailscale.go}
M  internal/config/{config.go,config_test.go,proxy.go}
M  internal/proxy/core/{gen.go,gen_test.go}
M  web/src/main.jsx
M  web/src/pages/{ProxySettingsPage,RulesPage}.jsx
?? internal/api/proxy_adblock.go          (广告拦截，新)
?? internal/app/proxy_adblock.go          (广告拦截，新)
?? internal/app/proxy_listen.go           (局域网共享，新)
?? internal/app/proxy_listen_test.go      (局域网共享，新)
M/?? internal/api/dist/*                  (dist 重建过一次，但 web 改动未完成，最终必须再 make)
```

## 尚未完成的工作

1. **e2e 测试（当前编译失败，最优先）**：
   - `e2e/e2e_test.go:856-1330` main-auto/main-node 段落未改，`vet` 报 `e2e_test.go:937: saved.MainAuto undefined`。需按计划重写为 PROXY select 语义（选定节点后规则仍生效、REJECT 依旧拦截、未命中流量走所选节点；CLI 段落改 `groups select PROXY`）。
   - `e2e/proxy-overview-ui.cjs` 未改（fixture 含 main_auto/main_node）。
2. **Web 前端（只改了一半）**：
   - 已改：`main.jsx`、`ProxySettingsPage.jsx`、`RulesPage.jsx`（但其中仍有 main-node 残留字样，需复查）。
   - **未改**：`web/src/pages/ProxyOverviewPage.jsx`（三策略模型 resolveMainPolicy/resolveActiveNode/PolicyOption 仍在）、`NodesPage.jsx`（「设为主端口」按钮）、`GroupsPage.jsx`（内置项禁编辑/删除）、`components/FixedNodeDialog.jsx`（泛化为出口选择，注意 groupstate 存节点**名**不是 key）。
3. **文档**：`docs/manual.md` 是否已补 PROXY 组章节需复查；`docs/adr/0002`、`docs/CONTEXT.md`、`docs/management-navigation.md` 中 main-node/固定节点表述未处理；`configs/config.dev.yaml` 待查。
4. **注释清理**：`internal/proxy/node/{tunnel,node}.go`、`internal/proxy/pool/alloc.go`、`internal/config/config.go` 等仍含 main-node 字样的注释/测试引用。
5. **dist 最终重建**（web 全部改完后 `make`）。
6. **冒烟验证**（见下）。

## 当前阻塞问题

- 无外部阻塞。唯一硬性问题：e2e 包编译失败导致 `go vet/test ./...` 不能全绿（主包 build 通过）。
- 工作区是「三个功能未提交改动」的混合状态，提交时需注意取舍或一次性提交。

## 已运行的测试及结果

- 2026-09-19（交接时实测）：
  - `go build -tags "with_gvisor ts_omit_acme" ./...` ✅ 通过。
  - `go vet -tags "with_gvisor ts_omit_acme" ./...` ❌ 仅 e2e 失败：`e2e/e2e_test.go:937:12: saved.MainAuto undefined`。
  - `go test` / `make` / 冒烟：**未运行**。
- 前两个功能（广告拦截、局域网共享）在此前会话已完成全量验证（build/vet/test 全绿 + 实际 serve 冒烟），可信。

## 下一步应该执行什么

1. 先定工作区策略：把当前中间态跑通（推荐续做），或 `git checkout` 回退本次任务相关文件（注意与广告拦截/局域网共享文件有交叉，不能整目录回退）。
2. 续做顺序：修 e2e 编译（重写 main 段落）→ 完成 Web 四个未改文件 → 文档与注释清理 → `make` 重建 dist → 全量 `build/vet/test` → 冒烟。
3. 冒烟要点（与计划一致）：实际启动 `bin/proxyd serve`；`POST /api/groups/PROXY/select` 选节点后经主端口 curl 验证（本机 shell 有 `http_proxy=127.0.0.1:7890`，curl 本机 API 必须 `--noproxy '*'`）；AUTO/DIRECT 各验一次；重启验证持久化；含 main-node 键的旧配置启动正常。
4. 可 resume 原 coder subagent（agent_id: **agent-2**）继续，它有本次改动的完整上下文；它最后是被用户主动停止的，恢复前请向用户确认。

## 不要重复做什么

- 不要重新设计或重开已确认的决策（删 main-node/main-auto、不迁移、PROXY 成员构成与顺序、复用 groupstate）。
- 不要重做后端/CLI 已完成的改动（先读代码确认现状再动手，主包已编译通过）。
- 不要迁移旧 main-node 配置值（用户明确不保留）。
- 不要暴露 mihomo GLOBAL 组选择；不要改动 auto-port、节点映射端口、自定义分组行为。
- 不要在 web 改完前提交 dist；dist 产物随仓库提交，最终必须 `make` 重建。

## 相关 Plan 文件路径

- 本次任务（已批准）：`/Users/tp/.kimi-code/sessions/wd_proxy_965b7a3f410d/session_ea9a52dd-70fb-4270-9cc9-94f570a09847/agents/main/plans/yellowjacket-black-lightning-america-chavez.md`
- 广告拦截（已完成）：同目录 `plans/kid-flash-supergirl-guy-gardner.md`
- 局域网共享（已完成）：同目录 `plans/she-hulk-nightwing-dove.md`
