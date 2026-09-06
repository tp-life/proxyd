# 状态、诊断与配置历史

系统菜单新增诊断中心与配置历史。模块管理同时显示期望开关和实际运行阶段，可分别重试代理与远程访问。

## 模块状态

`GET /api/modules` 返回 `enabled`、`phase`、`running`、`error`、`updated_at`、`next_retry_at` 与 `attempts`。

| 阶段 | 含义 |
| --- | --- |
| `disabled` | 已禁用，保留配置 |
| `idle` | 已启用，但没有运行中的子服务 |
| `starting` | 正在应用配置或刷新 |
| `running` | 核心或已开启的子服务正在运行 |
| `degraded` | 部分能力可用，刷新或端口转发异常 |
| `retrying` | 远程服务启动失败，等待重试 |
| `failed` | 当前应用失败且没有可用运行实例 |

远程启动失败、转发监听冲突会安排 30 秒后的重试。远程维护独立于代理订阅刷新，网络恢复或监听端口释放后可恢复。`running` 代表进程内服务已启动，不等于每个远端设备或中继均可达；实际连接使用诊断验证。`attempts` 为运行配置应用与刷新尝试的累计计数，不是 HTTP 重试次数。

```sh
proxyd modules list
proxyd modules remote retry
proxyd modules proxy retry
```

手动重试不会启用已禁用模块。失败状态使用固定提示，详细排障从诊断中心开始。

## 诊断

```sh
proxyd diagnose
proxyd diagnose home
proxyd diagnose home --json > diagnosis.json
```

Web 与上述 CLI 共用 `POST /api/diagnostics`，请求为 `{"peer":"home"}`；空名称检查本机，非空名称必须是已保存设备。一次只允许一个诊断，可取消；总检查期限为 50 秒，各网络步骤还有独立超时。

检查内容依次为模块、地图 DNS、DERP 地图、指定设备的隧道 22 端口、SSH 标识、SSH 握手与认证、交互登录 shell。失败后的依赖步骤显示“未检查”。主地图失败仍检查官方备用地图；私有地图不回退到公共来源。地图读取成功不代表其中的中继已连通。

检查从**守护进程所在机器**发起，使用该实例的持久客户端身份和可访问的 SSH agent。浏览器不会上传或保存用户私钥。需要使用客户端自己的私钥、agent、SSH 配置或不同用户名时，继续使用原有命令：

```sh
proxyd ssh home --diagnose -i ~/.ssh/id_ed25519 -o SetEnv=TERM=xterm-256color
```

远端提供 `proxyd-diagnostics` 子系统时才检查真实交互登录环境。普通系统 sshd 不提供此子系统时显示未检查。环境检查仅确认启动完成及 PATH/SHELL 非空，不保证用户的每个命令都已安装。

诊断导出仅包含固定阶段、状态、耗时、时间和提示，不包含 token、私钥、API secret、环境变量原文或外部错误正文。CLI 有失败项时先输出报告，再返回非零退出码；主来源失败但备用可用也会保留主来源的失败证据。

## 配置历史

配置写入前自动归档当前磁盘文件，最多保留 30 份。归档失败时停止主配置写入。首次保存会包含默认配置格式补全，因此差异段可能较多。

完整快照使用 AES-GCM 加密，放在 `<state-dir>/config-history/`；本机解密密钥为该目录下的 `vault.key`，权限 `0600`，目录首次创建权限 `0700`。已有历史而密钥遗失时拒绝生成替代密钥。不要把解密密钥与历史文件一起对外分享；此机制避免直接阅读历史 JSON 得到凭据，不抵御已取得该系统账户权限的进程。

Web 提供列表、涉及配置段、脱敏下载和“预检并恢复”。CLI：

```sh
proxyd config history list
proxyd config history export <ID> > redacted.yaml
proxyd config history preview <ID>
proxyd config history restore <ID>          # 只预检
proxyd config history restore <ID> --yes    # 重新预检并执行恢复
```

恢复预检校验完整历史配置，并绑定历史内容及当前磁盘内容的摘要。确认时任一内容发生变化，API 返回冲突，必须重新预检。恢复前再次归档当前配置。

恢复与配置导入均在**重启后生效**。待重启期间拒绝普通配置落盘，防止旧运行配置覆盖新文件。管理地址或 API 凭据变化后，请按恢复后的配置重新连接。配置历史不包括独立的 tailcat/SSH 身份密钥文件、订阅缓存或会话；恢复 `state-dir` 也不会自动搬移历史和身份文件。备份完整实例时需要另外保留状态目录。

HTTP 接口：

- `GET /api/config/history`：版本列表与 `pending_restart`。
- `GET /api/config/history/{id}/export`：仅脱敏 YAML。
- `POST /api/config/history/{id}/preview`：校验、配置段差异、`digest` 与 `base_digest`。
- `POST /api/config/history/{id}/restore`：提交 `digest` 和 `base_digest`；冲突返回 409。

## 开发回归

```sh
make
go build ./...
go test ./...
go vet ./...
go test -race ./internal/lifecycle ./internal/infrastructure/confighistory ./internal/proxy/core ./internal/app ./internal/api ./internal/remote ./cmd/proxyd
```

浏览器回归使用 `e2e/web-reliability.cjs`，需要已安装的 `playwright-core` 和 Chromium。通过环境变量指定模块目录和浏览器可执行文件，不固定开发者电脑路径：

```sh
PLAYWRIGHT_MODULE=/absolute/path/to/playwright-core \
CHROMIUM_PATH=/absolute/path/to/chromium \
node e2e/web-reliability.cjs
```

脚本使用 `bin/proxyd`，可用 `PROXYD_BINARY` 指定另一个二进制。它自行创建临时状态目录、凭据和回环端口，只停止自己启动的子进程；覆盖任务菜单、真实终端最小化/恢复、最小化后服务端断开、诊断与脱敏导出、配置恢复及移动端宽度，并输出截图目录。

mihomo v1.19.30 的日志级别与入站 IP 策略在热更新和连接处理之间存在共享变量竞态，当前以仓库内源码替换修复。补丁位置与升级步骤见 [第三方补丁说明](../third_party/README.md)。


总览展示模型可通过 `node --test e2e/dashboard-model.test.mjs` 验证，覆盖禁用隔离、未知指标、连接去重和密钥到期边界。浏览器脚本同时覆盖全局总览与代理运行概览的页面归属、无侧栏布局、纯远程/全部禁用状态、终端跨总览保活及明暗主题和移动端。双模块异常和局部接口故障使用明确的只读响应夹具，避免为展示测试建立外网隧道；系统待重启状态另由真实配置恢复流程验证。
