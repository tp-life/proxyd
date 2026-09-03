# proxyd 开发指南

proxyd 是多节点端口映射代理工具：Go 守护进程内嵌 mihomo 做数据面，内嵌 React Web 控制台。
附带独立的「远程连接」周边模块（基于 tailcat 的 SSH/SCP 隧道）。

## 构建与测试

- `make`（= `make web build`）：先构建前端到 `internal/api/dist`，再编译内嵌它的 `bin/proxyd`。
- `make build`：仅编译 Go（无 Node 环境用，需 dist 已存在）。
- `go test ./...`：全量测试（含 e2e，约 40s）；`go vet ./...`。
- Web 单独构建：`npm --prefix web run build`。
- 提交前至少跑 `go build ./... && go test ./...`；动了 `web/src` 必须重新 `make`，dist 产物随仓库提交。

## 模块划分（新增功能的落位规则）

功能按「垂直模块」划分，一个模块横跨各层的同名文件，层次职责固定：

```
internal/<module>/        领域层：纯业务逻辑，不 import 其他模块；外部重依赖只允许在这里出现
                          （如 internal/remote 是 tailcat 的唯一 import 点）
internal/config/<module>.go   配置段类型 + 校验 + 脱敏（proxy 段在 proxy.go，共享骨架在 config.go）
internal/app/<module>.go      编排层：用例 + 配置变更事务（克隆→调和→落盘→失败回滚）
internal/api/<module>.go      HTTP 层：handler + register<Module>Routes(mux)，在 api.go 的 Start() 里登记一行
cmd/proxyd/<module>.go        CLI 子命令
web/src/pages/<Module>Page.jsx + web/src/hooks/use<Module>Feed.js   控制台页面与数据 hook
```

现有模块：`proxy`（代理主功能，域服务在 `internal/proxy/{core,node,pool,subscribe,ruleurl,sysproxy,tunperm}`，
编排文件以 `proxy_` 前缀命名）与 `remote`（远程连接隧道）。平台通用服务（`logbuf`/`autostart`/`updatecheck`）不属于任何模块。

新增模块时按上表各层各加一个文件，不要塞进 proxy 的文件里。

## 约定

- 包注释、导出函数 doc comment 用中文；代码标识符用英文。
- 配置变更必须走事务模式（参考 `internal/app/remote.go` 的 `mutateRemote`）：改内存 → 调和运行态 →
  `persistLocked()` 落盘 → 任一步失败整体回滚。
- API 路由注册只写在各域文件的 `register*Routes` 里，`api.go` 的 `Start()` 只保留调用清单。
- 凭据类字段（token/secret）在列表/状态接口一律打码，完整值只通过专用 GET 端点返回（参考 `internal/api/remote.go`）。
- 平台相关代码按 `*_darwin.go` / `*_linux.go` / `*_windows.go` 分文件。
