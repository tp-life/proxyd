# proxyd 使用手册

proxyd 是一个多节点端口映射代理工具：把订阅里的**每个可用节点各映射到一个本地端口**（HTTP + SOCKS5 混合），让所有节点同时可用；另有一个主端口走常规 Clash 规则模式（rule / global / direct），以及可选的自动选优端口（`auto-port`，固定走延迟最低的可用节点）。

---

## 一、整体架构

```
                 ┌────────────────────────── proxyd（单二进制）──────────────────────────┐
                 │                                                                      │
  订阅地址 1 ──► │  订阅拉取/解析 ──► 健康检测 ──► 端口分配 ──► 生成 mihomo 配置 ──► 内嵌核心  │
  订阅地址 2 ──► │  (subscribe)      (pool)      (alloc)      (core/gen)         (mihomo) │
                 │                                                  │                     │
                 │       调度器（app）：订阅仅手动同步 + 定时健康检测，变化时热更新核心       │
                 └──────────────────────────────────────────────────┼─────────────────────┘
                                                                    │
        ┌───────────────────────────────────────────────────────────┼──────────┐
        │  主端口 41999（mixed，走规则）   映射端口 42000..（每个固定走一个节点）   │
        │  curl -x 127.0.0.1:41999      curl -x 127.0.0.1:42000 → 节点A        │
        │                               curl -x 127.0.0.1:42001 → 节点B        │
        └──────────────────────────────────────────────────────────────────────┘

  管理面：
    http://127.0.0.1:19091/   React Web 控制台 + proxyd 自有 API（订阅管理、映射表、模式切换）
    http://127.0.0.1:19090    mihomo external-controller（兼容 metacubexd / yacd 面板）
```

- 流量转发由内嵌的 [mihomo](https://github.com/MetaCubeX/mihomo)（Clash.Meta）核心完成，**无需单独安装 mihomo**。
- 多节点同时代理的原理：每个映射端口是 mihomo 的一个 `mixed` listener，带 `proxy: <节点名>` 字段，该端口的所有流量固定走对应节点出口，绕过规则匹配。
- 主端口默认是普通 mixed 端口，走完整的规则匹配，行为与 Clash 一致；开启 `main-auto` 后改由 listener 固定走 AUTO 选优组（同端口，规则被跳过）；设置 `main-node` 后改由 listener 固定直达指定节点（同端口，规则被跳过）。

## 二、支持的订阅格式与协议

**订阅格式**（`type` 默认 `auto` 自动嗅探，也可显式指定）：

| 格式 | 说明 |
|---|---|
| Clash YAML | 含顶层 `proxies:` 的标准 Clash 订阅，字段原样透传给 mihomo |
| 分享链接 | base64 编码的多行链接列表（v2ray 风格），也兼容明文 |

**节点协议**：

- Clash YAML 订阅：支持 mihomo 全部出站协议——ss、ssr、vmess、vless、trojan、hysteria2、tuic、anytls、snell、shadowtls、wireguard、socks5、http、direct 等
- 分享链接订阅：ss（SIP002 及旧格式）、ssr、vmess、vless（含 reality）、trojan、hysteria2 / hy2、tuic

**手动节点**（`manual-nodes`，持久化在配置文件，来源标记为 `manual`）：没有订阅时也可以直接添加自有代理，与订阅节点一样参与去重、测速、端口分配、分组与 auto-port：

- `http://[user:pass@]host:port[#名称]` / `https://...`（mihomo http 出站，https 自动启用 TLS）
- `socks5://[user:pass@]host:port[#名称]`
- 全部分享链接格式：ss / ssr / vmess / vless / trojan / hy2 / tuic（节点名取 `#fragment`）
- 结构化隧道类（VPN）出站映射：`tailscale` / `openvpn` / `zerotier` / `wireguard` / `ssh`（无分享链接标准，以映射形式录入；Tailscale 与 OpenVPN 另提供一体化向导；OpenVPN 可直接导入 inline `.ovpn`）。这类节点不参与逐节点端口映射，经分组端口使用，详见「六、规则与代理模式 → 隧道类（VPN）出站」

**去重与过滤**：

- 多订阅的节点按"协议+地址+端口+凭证"去重；同名节点自动改名（追加订阅名）
- `include` / `exclude` 按节点名正则过滤：非空 include 先保留匹配节点，再由 exclude 剔除匹配项；exclude 默认建议 `到期|剩余流量|套餐|官网|订阅`

## 三、入门（快速开始）

### 安装

从 Release 下载对应平台压缩包解压，或源码编译：

```sh
make            # 一次性完整构建：Web → internal/api/dist → bin/proxyd
make all        # 与 make 等价，适合显式写入构建脚本或 CI
make build      # 产出 bin/proxyd（单文件，无外部依赖）
make web        # 仅重建 Web 控制台 embed 产物（internal/api/dist）
```

Go 依赖直接按 `go.mod` 的固定版本从模块源下载，项目不再通过 `replace`、`third_party` 或源码补丁修改 mihomo。mihomo 内部问题由上游负责修复，本项目在上游版本包含修复后通过常规依赖升级获取。

全部构建与测试统一带 `-tags "with_gvisor ts_omit_acme"`（ADR 0002）：`with_gvisor` 解锁 mihomo 的 tailscale 出站与 TUN gvisor/mixed 协议栈；`ts_omit_acme` 使用 Tailscale 上游提供的构建裁剪 seam，移除本项目未使用的 tsnet ACME 入口，并避免 mihomo 专用 Tailscale 分支与 tailcat 依赖重复注册进程级 expvar。绕过 Makefile 直接运行 `go build/test/vet` 时必须携带同一组标签。

从干净检出或源码发布包可以直接执行带上述标签的 `go build`、`go test` 和 `go vet`；Go 会按 `go.mod` 与 `go.sum` 下载并校验依赖。仓库中的 `docs` 仅跟踪本手册，其余设计和排障文档保留在本地。

### 最快启动

```sh
proxyd serve https://你的订阅地址
```

这条命令会启动服务并保存订阅配置，但不会自动访问订阅地址。首次运行请继续：

1. 打开 Web 控制台并点击“刷新订阅”，或在另一个终端执行 `proxyd refresh`
2. 手动同步会拉取订阅、解析并检测节点
3. 可用节点会映射到默认端口区间 `42000-42100`
4. 配置保存到 `~/.config/proxyd/config.yaml`，节点保存到 `state-dir/nodes.json`

之后**直接 `proxyd serve` 即可**（不带参数），订阅地址已记住。

```sh
proxyd serve <url1> <url2>          # 多订阅直接追加
proxyd serve -range 43000-43300     # 自定义端口区间
proxyd check <url>                  # 不常驻：拉订阅、测速、打印映射表后退出
proxyd version                      # 打印版本
```

### 验证代理可用

```sh
curl -x http://127.0.0.1:42000 https://api.ipify.org   # 走"42000 端口对应的节点"
curl -x http://127.0.0.1:41999 https://api.ipify.org   # 走主端口（规则模式）
```

每个映射端口同时支持 HTTP 代理和 SOCKS5：`curl --socks5 127.0.0.1:42000 ...` 也可以。

## 四、启动方式汇总

| 命令 | 说明 |
|---|---|
| `proxyd` | 无参数等价于 `proxyd serve`（读默认配置） |
| `proxyd serve` | 前台常驻运行，日志直接输出到终端 |
| `proxyd serve <url>...` | 快捷启动；新订阅地址自动合并保存进配置文件 |
| `proxyd serve -c xxx.yaml` | 指定配置文件 |
| `proxyd serve -range A-B <url>` | 指定映射端口区间 |
| `proxyd <url>` | `serve <url>` 的快捷形式 |
| `proxyd start [-c 配置]` | 后台守护模式：派生 detached 子进程执行 serve，日志落 `state-dir/proxyd.log`，pid 写 `state-dir/proxyd.pid`；启动后做就绪等待（轮询 API，最长 10s）并打印 Web 地址；已运行则报错 |
| `proxyd stop` | 读 pid 文件发 SIGTERM 优雅退出，等待最长 10s，清理 pid 文件；stale pid 自动清理 |
| `proxyd restart` | 重启当前实例；macOS 同配置由系统托管时，请求旧实例退出并等待系统拉起替代进程 |
| `proxyd status` | 运行中显示 pid、端口、Web 地址、API 健康状态，并追加实例汇总（模式/节点存活/端口映射/主端口策略/系统代理/TUN/DNS/自启/新版本提醒） |
| `proxyd check ...` | 一次性自检：打印节点/端口映射表，参数同 serve |
| `proxyd sysproxy [-c 配置] on\|off\|status` | 开关/查看系统代理（指向主端口；flag 需放在操作前） |
| `proxyd tun [-c 配置] on\|off\|status` | 开关/查看 TUN 模式及当前进程权限（操作运行中实例） |
| `proxyd autostart [-c 配置] on\|off\|status` | 开关/查看开机自启（macOS 为系统级 LaunchDaemon，flag 需放在操作前） |

### 本地管理命令（CLI ↔ Web 对齐）

以下命令全部作为**本地 API 客户端**实现：读取配置文件拿 `api-listen` 地址，HTTP 调用运行中实例；实例未运行时提示「请先 proxyd start」，不产生离线改配置的旁路。错误信息原样透传 API 报错。

| 命令 | 说明 |
|---|---|
| `proxyd status` | 运行状态汇总：pid、端口、模式、节点存活数、端口映射、主端口策略、系统代理/TUN/DNS/自启开关、新版本提醒 |
| `proxyd mode [rule\|global\|direct]` | 无参查看当前模式；带参切换（持久化） |
| `proxyd refresh` / `proxyd test` | 触发刷新订阅 / 手动测速（后台执行） |
| `proxyd subs list\|add <名> <url>\|del <名>` | 订阅管理（list 含状态列：正常/部分可用/全部失效/无节点/已禁用） |
| `proxyd subs set [--rename 新名] [--url 地址] [--type 类型] [--enable\|--disable] [--mapping on\|off] <名>` | 修改订阅；未给出的字段保持原值；`--mapping` 是该订阅的端口映射开关（只控制其节点的一对一监听） |
| `proxyd subs refresh <名>` / `proxyd subs test <名>` | 只刷新 / 只测速单个订阅（同步执行，失败原因直接返回） |
| `proxyd nodes` | 按订阅分组列出节点、端口、延迟/失败原因；隧道类（VPN）节点带 `vpn` 标记且无端口映射 |
| `proxyd nodes add <url> [名称]` | 添加手动节点（http(s)/socks5/分享链接） |
| `proxyd nodes add --proxy '<出站JSON>' [名称]` | 添加隧道类（VPN）手动节点：`<出站JSON>` 是 mihomo 出站映射，type 限 `tailscale\|openvpn\|zerotier\|wireguard\|ssh`（tailscale 的 auth-key 可选，缺失时由 tsnet 输出交互注册链接；openvpn 必填 server/port/ca） |
| `proxyd nodes del <名称\|下标>` | 删除手动节点 |
| `proxyd rules list\|add "<规则>"\|set <下标> "<规则>"\|move <从> <到>\|del <下标>` | 自定义规则的增改删与优先级调整 |
| `proxyd rule-urls list\|add <名> <url>\|del <名>\|show <名>` | 远程规则源；`show` 打印原始内容（未解析） |
| `proxyd groups list\|add [--type 类型] [--subscription 订阅名] <名> <端口> [节点名...]\|del <名>` | 节点分组（list 含类型与 select 组的当前选中项；type 支持 `url-test\|fallback\|load-balance\|select`） |
| `proxyd groups set [--type 类型] [--subscription 订阅名] [--port 端口] <名> [节点名...]` | 修改分组；未给出的字段保持原值，给出节点名时整体替换成员 |
| `proxyd groups select <组名> <节点名>` | 选择 select 分组的手动出口节点（持久化到 state-dir，重启/刷新后保持） |
| `proxyd logs [--tail N] [--level debug\|info\|warning\|error]` | 查看运行中实例的内存日志尾部 |
| `proxyd port-mapping [on\|off\|status]` | 热开关或查看逐节点端口映射；关闭时保留稳定端口分配，不启动对应监听。单个订阅可用 `subs set --mapping on\|off` 单独开关（只影响该订阅节点） |
| `proxyd port-range <起-止>` | 修改节点映射端口区间 |
| `proxyd auto-port <端口\|off>` | 设置/关闭自动选优端口；无参查看 |
| `proxyd main-auto [on\|off]` | 开关「主端口使用最优节点」（跳过规则）；无参查看 |
| `proxyd main-node [节点名\|key\|off]` | 设置主端口固定节点（跳过规则、直达该节点）；可直接给节点名（重名时按提示改用 key）；无参查看，`off` 清除 |
| `proxyd main-port <端口>` | 修改主端口（热更新；系统代理开启时自动重绑）；无参查看 |
| `proxyd tun on\|off\|status` | 热开关 TUN 或查看权限；权限不足时输出平台修复命令 |
| `proxyd dns-preset [off\|fake-ip\|redir-host]` | 查看/切换 DNS 预设；配置文件存在手写 `dns` 段时会提示预设不生效 |
| `proxyd update-check [on\|off]` | 查看/开关启动版本检查；无参显示当前/最新版本与检查状态 |
| `proxyd conn list` / `proxyd conn close <id\|all>` | 查看活动连接（出站、规则、目标、上下行、存活时长与内存占用）/ 关闭单条或全部连接 |
| `proxyd traffic` | 实时上/下行速率（每秒刷新，Ctrl-C 退出） |
| `proxyd config path` | 打印当前使用的配置文件绝对路径 |
| `proxyd config export [--full] [-o 文件]` | 导出配置；默认打码（隐藏凭据），`--full` 完整备份；默认打印到标准输出 |
| `proxyd config import [--yes] <文件>` | 导入配置：先预检并展示数量/字段差异，确认后原子写入；需 `proxyd restart` 生效 |
| `proxyd remote status\|on\|off\|token` | 远程连接（tailcat 隧道）：查看状态、热开关服务端、打印完整本机 token（见「十、远程连接」） |
| `proxyd remote serve [端口,...]` | 查看/设置经隧道暴露的本机端口 |
| `proxyd remote allow list\|add <公钥> [别名] [--ttl 1h] [--ports 22,8080]\|del <别名\|公钥>` | 管理带有效期和目标端口限制的客户端授权 |
| `proxyd remote audit [--tail N]` | 查看连接建立、拒绝与断开审计记录（最多 500 条） |
| `proxyd remote keyfile [路径\|-]\|export <路径>\|import <路径>` | 设置自定义密钥路径，或迁移内置托管的服务端身份 |
| `proxyd remote web-terminal [on\|off] [--yes]` | 查看/切换高权限浏览器终端；非回环 API 监听需确认，非交互环境用 `--yes` |
| `proxyd remote remotes list\|add <名> <token>\|del <名>` | 管理保存的远端；列表会探测在线状态、连接路径与 RTT |
| `proxyd remote forwards list\|add <名> <监听> <远端> <端口>\|del <名>\|on\|off <名>` | 管理本地常驻转发；监听地址可填 `auto`（或留空），自动从 10022 起分配空闲端口 |
| `proxyd ssh <远端>` / `proxyd scp <源> <目标>` | 经隧道直连 SSH / scp 传文件（纯客户端命令，远端可填保存的名称或 token，见「十、远程连接」） |
| `proxyd desk <rdp\|vnc> <远端>` | 创建临时回环转发并打开当前用户的系统远程桌面客户端；窗口退出后自动清理 |

`proxyd ssh`、`proxyd scp`、`proxyd desk` 与 `proxyd remote pipe` 是例外：它们是**纯客户端命令**，不经过本地 API、不需要守护进程运行（直接读配置文件解析远端名）。

`-c` 等 flag 必须放在子命令参数之前（Go flag 解析遇位置参数即停止）；写在后面的 `-c` 会被检测到并直接报错，避免静默操作到默认配置对应的实例。

### 开机自启

`proxyd autostart on` 注册当前二进制（`os.Executable` 绝对路径）+ 当前配置文件为开机自启项，并立即启动一次：

- **macOS**：经管理员授权安装 `/Library/LaunchDaemons/com.proxyd.plist`，注册到 `system` 域；LaunchDaemon 通过 `UserName` 以注册用户运行 `serve -c <配置绝对路径>`，StandardOut/Err 指向 `state-dir/proxyd.log`。它不依赖用户登录，冷启动、断电恢复或系统重启后均会启动，KeepAlive 负责崩溃拉起；启用或关闭时也会清理旧版 `~/Library/LaunchAgents/com.proxyd.plist`
- **Linux**：`~/.config/systemd/user/proxyd.service`（Restart=on-failure）+ `systemctl --user enable --now`；日志走 `journalctl --user -u proxyd`
- **Windows**：注册表 `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` 写 `proxyd start -c <配置>`（登录时派生后台进程后退出，不弹控制台窗口）

`off` 移除对应项，但**不影响正在运行的实例**：macOS 只删除 plist（launchd 内存中的定义保留到本次开机结束，进程继续运行，重启后不再拉起）；Linux 只 `disable` 不停止服务；Windows 同样不影响已运行实例。`status` 分别显示自启项注册状态和服务状态；macOS 会查询系统托管 PID、运行状态及最近退出码，其他平台明确提示尚未查询进程状态。不支持的平台返回明确错误。

macOS 已加载同配置的系统服务时，`proxyd start` 等待该服务就绪，`proxyd restart` 请求旧实例退出并由 KeepAlive 拉起新实例，不再自行派生竞争进程。等待最多 60 秒，只有系统 PID、配置 PID 与认证健康检查同时匹配才算成功。若独立启动的旧实例占用资源，可执行 `proxyd restart` 交回系统托管；查询失败或等待超时不会回退到独立启动。

Web 通用设置页的开关表示自启注册状态，下方单独显示系统服务运行信息；`/api/overview` 的 `autostart_runtime` 包含该快照。启动日志通过 `[startup]` 记录 PID 和配置加载、目录检查、应用初始化、API 监听的累计耗时。生成的 LaunchDaemon 使用 `ProcessType=Standard`；旧安装可执行 `proxyd autostart on` 更新磁盘 plist，已加载的服务定义会在下次开机时读取新版。

### 配置备份与恢复

Web 通用设置页提供两种导出：默认的“导出（打码）”会隐藏 `secret`、订阅/规则 URL 的用户信息和敏感查询参数，并整体隐藏编码型分享链接，适合排障分享；“完整备份”保留全部凭据，只应存放在可信位置。导入只接受不超过 1 MiB 的 YAML，并复用启动时的迁移、默认值和完整配置校验。前端会先调用预览接口展示新增、删除和变化项；用户确认时携带预览摘要，后端只有在文件内容与预览完全一致时才通过临时文件加 rename 原子替换当前配置文件，避免“预览后文件被替换”的竞态。

导入不会在当前 HTTP 请求中热替换运行配置。备份可能同时改变 `api-listen`、`state-dir`、监听端口和 TUN 权限要求，局部热更新会造成磁盘配置与运行状态不一致，因此接口明确返回 `restart_required: true`，必须重启 proxyd 后整份配置才生效。校验或写入失败时原配置保持不变。

### 版本检查

`check-updates` 默认 `true`。proxyd 启动后在后台请求官方 GitHub 仓库的 latest release，使用八秒 HTTP 超时并把结果缓存在应用内存；Web 的 overview 轮询只读取缓存，不会重复访问 GitHub。发现高于当前构建版本的稳定版时，概览页显示 Release 链接；请求失败、限流或 JSON 异常只记录状态和日志，不影响 API、代理核心或订阅刷新。

当前版本由构建时 `ldflags` 注入。正式 `vX.Y.Z` tag 和 `git describe` 版本可比较；`dev` 或裸提交哈希没有可靠版本基线，会显示“当前构建版本不可比较”并跳过请求，避免字符串比较误报。可在配置写 `check-updates: false`，也可在 Web 通用设置页关闭。

## 五、日常管理（Web 控制台）

浏览器打开 **http://127.0.0.1:19091/**，React 控制台使用按任务分组的侧边栏信息架构（窄屏自动收起，数据表在手机上转换为带字段标签的纵向条目）：

- **概览**：实时上下行速率条 + 状态摘要（主端口/auto-port/节点/映射统计）+ 入口端口 chip（主端口、auto-port、分组端口、节点映射区间）+ 规则 / 全局 / 直连 模式切换 + 系统代理快捷开关；发现新版时显示 GitHub Release 链接
- **代理节点**：全局搜索并按来源、协议、状态筛选节点；表格展示名称、来源、协议、稳定分配端口、延迟与失败原因，可添加/删除手动节点并设置主端口固定节点
- **订阅管理**：以卡片管理订阅的新增、编辑、启停、单独同步与测速；新增、编辑和重新启用只保存设置，不访问远端；点击“同步”后直接拉取、检测并热更新，无确认弹窗，完成或失败均显示通知
- **代理入口**：集中展示主端口、自动选优端口、分组端口与逐节点稳定分配；支持一键复制地址，并独立热开关 `port-mapping`
- **策略分组**：展示健康摘要、复制分组端口、新增/编辑/删除分组；可选择类型（url-test/fallback/load-balance）并按手选节点或订阅来源配置成员
- **访问规则**：搜索、新增、编辑、删除和调整自定义规则顺序；规则 URL 保持只读内容视图并可展开查看可读原文（优先缓存，无缓存时现场拉取；整体 Base64 gfwlist 自动解码）
- **运行日志**：查看进程内约 1000 条环形缓冲日志，支持搜索、暂停刷新、复制和下载
- **活动连接**：仅在页面打开且未暂停时每 2 秒读取 mihomo 连接快照；按域名/IP/进程/出口链搜索，查看入口端口、累计流量与开始时间，并可关闭单条或全部连接
- **远程连接**：管理 tailcat 服务端身份、SSH、客户端白名单、远端 token 和通用 TCP 转发；“服务端 / 客户端”页签只处理隧道与 SSH，不再混放桌面配置
- **远程桌面**：独立的“服务端 / 客户端”页签；服务端检测 RDP/VNC 是否在本机真实监听并控制隧道开放，客户端保存不含密码的连接档案、建立临时回环转发并打开系统客户端
- **代理 → 代理设置**：管理主端口、`main-auto`、`main-node`、节点映射端口区间、`auto-port`、`port-mapping`、DNS 预设、TUN 与系统代理
- **系统 → 通用设置**：管理开机自启、版本检查、配置备份、带差异预览的配置导入与进程重启；代理禁用时仍可使用
- 页面每 60 秒自动刷新数据；`⌘K` / `Ctrl+K` 命令面板支持跳页、刷新、测速和模式切换

### 自有 API（`api-listen`，默认 19091）

默认只监听回环地址，可不配置认证。一旦将 `api-listen` 设为 `0.0.0.0`、
普通网卡地址或其它非回环地址，必须同时配置 `api-secret`；服务会对 Web 页面、
`healthz`、所有 `/api/*` 及 WebSocket 终端统一要求 HTTP Basic 认证，用户名固定为
`proxyd`、密码为 `api-secret`。CLI 会从配置文件自动携带凭据。Basic Auth 本身不加密
传输内容；跨主机访问时应放在 HTTPS 反向代理之后，并同时用防火墙或可信内网限制来源。

从交互终端首次执行 `proxyd serve`、`proxyd start` 或 `proxyd autostart on` 时，如果
配置中尚无 `api-secret`，CLI 会隐藏输入并要求确认，将至少 6 个字符的口令原子写回
当前配置文件。后续启动直接读取已保存值，不再提示。LaunchDaemon、自动重启等无终端
场景不会等待输入：回环监听兼容旧配置继续启动，非回环监听缺少口令则明确拒绝启动。

| 接口 | 说明 |
|---|---|
| `GET /api/overview` | 总览：模式、主端口/main_auto、auto-port、订阅聚合（含类型、启用状态、订阅级映射开关、userinfo）、手动节点、端口映射开关与稳定分配、全部节点（含类型/失败原因；隧道类节点带 `tunnel: true` 且 port 恒为 0）、自定义规则、节点分组（含 type 与 select 组的 selected）、TUN 权限、系统代理与开机自启状态 |
| `GET /api/traffic` | 代理 mihomo `/traffic` 流，返回 NDJSON 实时速率；后端自动附加 `secret` 鉴权 |
| `GET /api/connections` | 代理 mihomo `/connections` 快照，返回活动连接、累计上下行和内存占用；后端自动附加 `secret` 鉴权 |
| `DELETE /api/connections/{id}` | 关闭指定活动连接；连接 ID 作为单个安全路径段转发 |
| `DELETE /api/connections` | 关闭全部活动连接 |
| `POST /api/mode` `{"mode":"global"}` | 切换代理模式（rule/global/direct，持久化） |
| `POST /api/refresh` | 触发一轮完整刷新：拉订阅 + 规则源 + 测速（异步，返回 202） |
| `POST /api/test` | 手动测速：只对现有节点做延迟/可用性检测，不拉订阅（异步，返回 202） |
| `POST /api/subscriptions` `{"url":"..."}` | 添加订阅（name 可选，默认按域名命名） |
| `PUT /api/subscriptions/{name}` | 编辑订阅名称、URL、格式、启用状态或订阅级端口映射开关（`port_mapping`）；改名时同步修正引用该订阅的策略分组；未给出的字段保持原值；设置保存不下载订阅 |
| `DELETE /api/subscriptions/{name}` | 删除订阅 |
| `POST /api/subscriptions/{name}/refresh` | 只刷新该订阅：重新拉取 + 只检测其节点 + 热更新（同步，最长 3 分钟，失败直接返回原因） |
| `POST /api/subscriptions/{name}/test` | 只对该订阅的现有节点测速（同步，不拉订阅） |
| 以下 `draft` 接口 | 仅供需要“预览后确认”的程序化客户端选用；当前 Web 控制台手动同步不会调用这些接口，也不会显示确认弹窗 |
| `POST /api/subscriptions/{name}/draft` | 生成单订阅刷新草稿：拉取 + 检测 + 安全 diff；不修改运行态与缓存，草稿 30 分钟后过期 |
| `GET /api/subscriptions/{name}/draft` | 读取当前待确认草稿的脱敏差异视图（不返回节点 Mapping、稳定 Key 或订阅正文） |
| `POST /api/subscriptions/{name}/draft/{id}/apply` | 确认应用草稿；若预览后的节点健康状态、稳定身份、订阅配置或策略组发生变化则返回 409，要求重新生成 |
| `DELETE /api/subscriptions/{name}/draft/{id}` | 按随机 ID 丢弃草稿；旧页面的 ID 不会误删后来生成的新草稿 |
| `GET /api/manual-nodes` | 列出手动节点（index/url 或结构化映射 type + 已打码的 proxy/解析出的名称） |
| `POST /api/manual-nodes` `{"url":"http://user:pass@host:8080","name":"可选"}` | 添加手动节点（解析校验 + 持久化 + 后台重建现有节点，不下载订阅） |
| `POST /api/manual-nodes` `{"proxy":{"name":"ts-exit","type":"tailscale","auth-key":"..."},"name":"可选覆盖"}` | 添加结构化隧道类（VPN）手动节点；type 限 `tailscale/openvpn/zerotier/wireguard/ssh`，与 `url` 二选一，其余字段透传 mihomo |
| `POST /api/tailscale/setups` | 一体化创建 Tailscale 出站、单成员 select 分组、固定代理入口与可选 TUN 规则；`auth_mode` 支持 `approval`/`auth-key`，`port: 0` 自动分配 |
| `DELETE /api/tailscale/setups/{name}` | 终止失败、启动中或等待审批的接入，并以事务方式删除 Tailscale 出站、引用该节点且已清空的组、受管 TUN 路由和内存注册状态；同名随后可重新创建 |
| `GET /api/tailscale/enrollments[/{name}]` | 读取当前进程观察到的注册状态；管理员审批模式包含一次性 `registration_url`，Headscale 的 `/register/<Auth ID>` 链接还会返回独立的 `auth_id`；二者均不写入配置文件 |
| `POST /api/openvpn/import` `{"profile":"..."}` | 解析 `.ovpn` 并只返回 server/port/proto、认证需求与兼容性警告；不回显证书、私钥或内联密码 |
| `POST /api/openvpn/setups` | 一体化创建 OpenVPN 出站、单成员 select 分组、固定代理入口与可选 TUN 规则；支持原始 `profile`、`auth-user-pass` 与 `private_key_passphrase` |
| `DELETE /api/openvpn/setups/{name}` | 以事务方式删除 OpenVPN 出站、所有成员引用、已清空的组及该组受管 TUN 路由 |
| `DELETE /api/manual-nodes/{index}` | 按下标删除手动节点 |
| `POST /api/port-mapping` `{"enabled":false}` | 热开关逐节点端口映射；关闭时保留稳定分配，核心不再生成对应 listener |
| `POST /api/port-range` `{"range":"43000-43200"}` | 修改节点映射端口区间（同步：重新分配端口 + 热更新，不重新测速） |
| `POST /api/auto-port` `{"port":41998}` | 开启自动选优端口；`{"port":0}` 关闭（持久化 + 热更新） |
| `POST /api/main-auto` `{"enabled":true}` | 开关「主端口使用最优节点」（主端口跳过规则、固定走 AUTO 组；持久化 + 热更新） |
| `POST /api/main-node` `{"node":"<节点key>"}` | 设置主端口固定节点（跳过规则、直达该节点；空串清除；持久化 + 热更新） |
| `POST /api/main-port` `{"port":42999}` | 修改主端口（校验 1-65535 且不与 api 端口/节点区间/分组/auto-port 冲突；持久化 + 热更新；系统代理开启时自动重绑） |
| `POST /api/system-proxy` `{"enabled":true}` | 开关系统代理（指向主端口，持久化） |
| `GET /api/tun` | 返回 TUN 开关、平台、当前进程是否具备权限以及修复指引 |
| `POST /api/tun` `{"enabled":true}` | 权限检查后热开关 TUN；失败恢复旧配置，成功后持久化 |
| `POST /api/dns-preset` `{"preset":"fake-ip"}` | 切换 `off/fake-ip/redir-host` 预设并热更新；手写 `dns:` 存在时仍优先生效 |
| `POST /api/update-check` `{"enabled":false}` | 开关启动版本检查并持久化；重新开启时立即异步检查一次 |
| `GET /api/config/export` | 下载默认打码的 YAML；加 `?mask_tokens=false` 下载含真实凭据的完整备份 |
| `POST /api/config/import/preview` | 上传不超过 1 MiB 的 YAML，仅校验并返回变更摘要、警告与内容摘要，不写磁盘 |
| `POST /api/config/import` | 携带预览返回的 `X-Proxyd-Config-Digest` 确认导入；摘要一致后原子替换配置文件，返回 `restart_required: true` |
| `POST /api/restart` | 重启 proxyd 进程：先返回 200，再异步派生 detached `restart` 子进程完成 stop→start；调用方轮询 `/healthz` 判断恢复，监听地址变更后需访问新地址 |
| `POST /api/autostart` `{"enabled":true}` | 注册/移除开机自启项（OS 级状态，不写配置文件；overview 实时反映） |
| `GET /api/rules` | 列出自定义规则 |
| `POST /api/rules` `{"rule":"DOMAIN-SUFFIX,example.com,DIRECT"}` | 追加自定义规则（前置到内置规则之前） |
| `PUT /api/rules/{index}` `{"rule":"..."}` | 编辑指定自定义规则并热更新 |
| `POST /api/rules/reorder` `{"from":2,"to":0}` | 调整自定义规则优先级并热更新 |
| `DELETE /api/rules/{index}` | 按下标删除自定义规则 |
| `GET /api/rule-urls` | 列出规则源（含条目数与拉取状态） |
| `POST /api/rule-urls` `{"name":"gfwlist","url":"https://..."}` | 新增规则源（持久化 + 立即拉取 + 热更新） |
| `DELETE /api/rule-urls/{name}` | 删除规则源 |
| `GET /api/rule-urls/{name}/content` | 规则源可读文本（text/plain；整体 Base64 gfwlist 自动解码；优先缓存，无缓存现场拉取一次并写缓存；源不存在/拉取失败返回 404） |
| `GET /api/groups` | 列出节点分组（含归一化后的 `type`；select 分组附带 `selected` 当前选中节点） |
| `GET /api/logs?tail=200&level=error` | 返回内存日志尾部；`level` 可选 `debug/info/warning/error` |
| `POST /api/groups` `{"name":"hk","port":43000,"type":"fallback","subscription":"airport-a"}` | 新增节点分组；`type` 支持 `url-test/fallback/load-balance/select`，成员可来自 `nodes` 或 `subscription` |
| `PUT /api/groups/{name}` | 编辑分组端口、类型与成员来源；为保护 `dialer-proxy` 引用，暂不支持在线改名 |
| `DELETE /api/groups/{name}` | 删除节点分组 |
| `POST /api/groups/{name}/select` `{"node":"节点名"}` | 选择 select 分组的手动出口节点；节点须为该组当前可用成员，选中项持久化到 state-dir 并热更新，失败整体回滚 |
| `GET /api/desktop` | 返回 RDP/VNC 本机监听与隧道开放状态、已保存连接和当前临时会话；不返回 token 或密码 |
| `POST /api/desktop/services/{rdp\|vnc}` | 原子更新协议实际服务端口及其 `remote.serve` 开放状态，失败时同时回滚桌面配置和 remote 运行态 |
| `POST /api/desktop/connections` / `PUT` / `DELETE` | 新增、更新或删除不含密码的桌面连接档案；远端字段引用 `remote.remotes` 名称 |
| `POST /api/desktop/sessions` / `DELETE /api/desktop/sessions/{id}` | 创建（或复用）和断开临时桌面回环转发；遗忘的会话会按首次连接宽限、空闲期和最长寿命自动回收 |
| `GET /api/desktop/sessions/{id}/rdp` | 下载不含密码的临时 `.rdp` 文件；VNC 会话由状态接口返回 `vnc://127.0.0.1:端口` |
| `GET /ports` | 端口映射表（兼容旧接口） |

### mihomo API（`external-controller`，默认 19090）

兼容 metacubexd / yacd 面板：`/proxies`（节点与测速）、`/configs`（模式/日志等级）、`/connections`、`/rules` 等。在面板设置里填 `http://127.0.0.1:19090` 即可接入（设置了 `secret` 时面板里也要填）。

### 系统代理

把系统 HTTP/HTTPS/SOCKS 代理指向主端口（`127.0.0.1:<mixed-port>`）：

- **CLI**：`proxyd sysproxy [-c 配置文件] on|off|status`（`-c` 需放在操作之前，Go flag 解析遇位置参数即停止）
- **Web**：设置面板的「系统代理」开关（`POST /api/system-proxy`）
- **配置**：`system-proxy: true` 时 `serve` 启动即自动应用，**进程退出（SIGINT/SIGTERM）时自动恢复关闭**；异常退出（kill -9 等）时用 `proxyd sysproxy off` 手动恢复

实现：macOS 用 `networksetup`（遍历所有活动网络服务）；Linux 用 gsettings（GNOME，best-effort）；Windows 改注册表 `HKCU\...\Internet Settings`（best-effort）。不支持的平台返回明确错误。

### TUN 模式

TUN 由 mihomo 创建虚拟网卡并配置系统路由，可接管不支持 HTTP/SOCKS 代理设置的应用以及 UDP 流量。它与系统代理是两个独立入口：`system-proxy` 只修改操作系统的 HTTP/HTTPS/SOCKS 代理设置，TUN 则在网络层接管流量；通常开启 TUN 后无需再开系统代理，同时开启也不会改变规则模式与节点选择逻辑。

```yaml
tun:
  enable: false
  stack: system
  auto-route: true
  auto-detect-interface: true
  dns-hijack:
    - 0.0.0.0:53
  # strict-route: true        # 其余 mihomo TUN 字段会原样保留并透传
```

使用 Web「代理 → 代理设置」或 `proxyd tun [-c 配置] on|off|status` 热切换。开启流程先检查权限，再让 mihomo 热更新，并读取实际 listener 状态二次确认；生成、应用或实际启用失败会恢复旧 TUN 配置。配置文件启动时已经是 `enable: true` 但权限不足，proxyd 会在启动 API 和修改路由之前退出并打印修复指引。

- **macOS**：进程必须以 root 运行。停止普通实例后用 `sudo proxyd serve -c <配置文件>` 或 `sudo proxyd start -c <配置文件>` 启动。
- **Linux**：可直接以 root 运行，或对当前二进制执行 `sudo setcap cap_net_admin=+ep /path/to/proxyd` 后重启。替换/升级二进制会丢失 capability，需要重新执行 `setcap`。
- **Windows**：必须从“以管理员身份运行”的 PowerShell/终端启动 proxyd；普通登录启动项不会自动提升权限。

`dns-hijack` 只负责把指定 DNS 流量交给 mihomo，实际解析策略由顶层 DNS 配置决定。`dns-preset` 提供 `off|fake-ip|redir-host` 三档，TUN 开启且未配置 DNS 时 Web 会建议 `fake-ip`；手写 `dns:` 会完整透传并拥有最高优先级。`off` 且没有手写 DNS 时沿用系统 DNS，是否劫持以及解析效果取决于系统和 mihomo 当前配置。

## 六、规则与代理模式

主端口（默认 41999）支持三种模式，与 Clash 语义一致：

| 模式 | 行为 |
|---|---|
| `rule`（默认） | 按 `rules` 规则匹配；内置默认规则：私网直连 → 国内（GEOSITE/GEOIP cn）直连 → 其余走代理 |
| `global` | 全部走 PROXY 选择组（可用 mihomo 面板切换组内节点） |
| `direct` | 全部直连 |

切换方式：Web 控制台按钮 / `POST /api/mode` / mihomo 面板 `PUT /configs`，轻量热切换并持久化。映射端口、分组端口、auto-port 不受模式影响——它们永远固定走自己的出口。

**主端口的三种状态**（同端口，热切换；自定义规则与内置规则只对第一种生效）：

1. **规则模式**（默认）：顶层 mixed-port，走完整规则匹配，行为与 Clash 一致。
2. **固定节点**（`main-node`，默认空）：主端口切换为同端口的 mixed listener，`proxy` 固定指向指定节点——跳过规则、直达该节点。配置里存节点 **Key**（协议+地址+凭据），重命名/重名时仍稳定；Web 概览页主端口卡片的「固定节点」下拉（列出全部当前可用节点，格式 `节点名 (端口)`）或 `proxyd main-node <节点名|key|off>` 设置（CLI 可直接给节点名，重名时会列出候选要求改用 key），选择即保存。节点当前不可用（失效/订阅刷新后消失）时本轮自动回退规则模式并打日志，**配置保留不删**，节点恢复后自动再生效（Web 上下拉旁会提示"当前节点不可用，已回退规则模式"）。
3. **自动优选**（`main-auto`，默认关闭）：主端口 listener 固定走 `AUTO` url-test 组——全部可用节点中自动选延迟最低者。与独立的 auto-port 可并存（共用 AUTO 组、各占端口）。无可用节点时本轮跳过该设置（主端口回退规则模式，日志有提示）。

优先级：**`main-auto` 开启时 `main-node` 被忽略**（auto 优先，日志提示一句）。从规则模式切换到 listener 形态（开 main-auto / 设 main-node / 失效节点恢复后自动再生效）时内部统一做两阶段热更新（先短暂关闭主端口入口再切换形态），避免 mihomo 先监听后释放导致的同端口 bind 冲突；listener 之间互切（main-auto ↔ main-node）同名 `L<port>` 仅换 proxy 目标，由 mihomo 按 关闭→监听 顺序安全处理。节点映射端口、分组端口、auto-port 完全不受影响。

**主端口在线修改**（Web 概览页 / `POST /api/main-port` / `proxyd main-port <端口>`）：校验 1-65535 且不与 api 端口、节点区间、分组端口、auto-port 冲突；保存后持久化 + 热更新；系统代理当前已开启时自动重新绑定到新端口。

**自动选优端口**（`auto-port`，默认关闭）：开启后额外监听一个独立 mixed 端口，固定走 `AUTO` url-test 组——全部当前可用节点中自动选延迟最低者（探测地址用 `health-url`，间隔 300s，容差 50ms）；无可用节点时本轮跳过该 listener（日志有提示）。与主端口的规则模式完全独立。不能与主端口、api 端口、节点区间、分组端口冲突。旧版 `mode: auto` 配置加载时自动迁移为 `mode: rule` + `auto-port: 41998`（日志有提示）。

**自定义规则**（`custom-rules`，Web/API/配置文件均可管理）：追加式，生成 mihomo 配置时**前置到内置 `rules` 之前**——规则匹配按顺序生效，追加在 GEOSITE/GEOIP/MATCH 之后永远不会命中，所以自定义规则必须前置；内置规则原样保留在后面。每条格式 `类型,内容,策略`（至少 3 段），支持 DOMAIN / DOMAIN-SUFFIX / DOMAIN-KEYWORD / IP-CIDR / GEOSITE / GEOIP 等 mihomo 语法，策略可填 DIRECT / REJECT / 节点名 / 分组名；非法规则在 API 层直接报错（含 mihomo 自检失败的原因），不会静默生效。

**规则 URL 导入**（`rule-urls`）：从远程 URL 导入规则（如 gfwlist），仅在用户手动执行全局“刷新订阅”时与订阅一起拉取，内容缓存到 `state-dir/cache/`，**不会写回配置文件**（config 只存 URL）；拉取失败降级用缓存，都没有则跳过该源打日志。原始内容可在 Web 控制台「查看内容」或 `proxyd rule-urls show <名>` 查看（优先读缓存，无缓存时现场拉取一次并写缓存）。按内容自动识别两种格式：

- mihomo 规则文本：每行 `类型,内容,策略`（≥3 段）原样采用，支持 `#`/`//` 注释与空行
- gfwlist / AutoProxy（base64 编码）：`||domain` → `DOMAIN-SUFFIX,domain,PROXY`；`@@||domain` → `DOMAIN-SUFFIX,domain,DIRECT`；`!` 注释、`[AutoProxy]` 段头、含 `*`/`/` 的复杂条目跳过

合并顺序：custom-rules 最前 → 规则 URL 导入规则 → 内置规则。全部来源合并去重后上限 10000 条，超出截断打日志。

**节点分组端口**（`groups`）：把若干节点聚合成一个 mihomo proxy-group 并绑定到指定端口，该端口固定走该组；`type` 可选 `url-test`（自动测速择优）、`fallback`（按顺序故障转移）、`load-balance`（负载均衡）、`select`（手动选择出口）。旧配置没有 `type` 时默认迁移为 `url-test`，新 UI 默认推荐 `fallback`：

```yaml
groups:
  - name: hk                # 组名（不能与节点名或 AUTO/PROXY/DIRECT 等保留名冲突）
    port: 43000             # 不能与主端口、api 端口、port-range 区间、auto-port 或其他分组冲突
    type: fallback          # url-test | fallback | load-balance | select；旧配置缺省为 url-test
    nodes: ["香港 01", "香港 02"]  # 节点名列表，与当前可用节点取交集

  - name: airport-a-auto
    port: 43001
    type: url-test
    subscription: airport-a # 成员动态取该订阅当前可用节点，刷新后自动跟随
```

成员来源二选一：配置 `nodes` 时取节点名与当前可用节点的交集；配置 `subscription` 时取该订阅当前可用节点，`manual` 表示手动节点来源。刷新后节点集合变化时分组自动收缩，成员为空则该组本轮跳过（日志有提示）。分组与按节点映射端口完全并存、互不影响。

`select` 类型的分组由用户手动指定出口节点：`POST /api/groups/{name}/select`（`{"node":"节点名"}`）或 `proxyd groups select <组名> <节点名>`；节点须为该组当前可用成员。选中项持久化在 `state-dir/group-selected.json`，生成配置时写入 mihomo 的 `default-selected` 字段，因此重启与订阅刷新后仍保持；选中节点消失时 mihomo 按原生语义回退到组成员首位。分组列表接口与 overview 的 `groups` 条目用 `selected` 字段返回当前选中项。

**隧道类（VPN）出站**：mihomo 内置的整网隧道出站——`tailscale`（tsnet）、`openvpn`、`zerotier`、`wireguard`、`ssh`——与普通代理节点行为差异很大（无常规 server:port 五元组、首次拨号慢、延迟天然偏高），因此 proxyd 对它们**单独归类**（ADR 0002）：

- **不参与逐节点端口映射**：不占用 `port-range` 区间，overview/节点列表中带 `tunnel: true` 标识且 `port` 恒为 0；使用方式是配置一个分组（`subscription: manual` 或 `nodes: [...]`），把应用指向该分组绑定的固定端口。
- **故障断流（kill switch）**：组内无可用成员时该分组本轮不生成监听端口，连接直接被拒，**不会**回退到 DIRECT 或规则模式——VPN 出口失效时不会明文漏流。
- **mihomo 统一管理 Tailscale**：proxyd 不直接调用 Tailscale SDK，也不在预检查阶段启动第二个 tsnet。预检查只验证 mihomo 能否解析出站；配置了 `exit-node` 时，正式加载后再通过 mihomo 代理表访问 `health-url`，未配置 Exit Node 的 Tailnet/子网路由出站不使用公网目标判死，由 mihomo 在首次匹配流量时懒启动。其它需要网络探测的隧道节点仍把超时放宽为 `health-timeout` 的 3 倍。
- **main-node 不禁止引用** VPN 节点（高级用户兼容路径），但主端口常被系统代理指向，误选会把整机流量送进隧道，默认请走分组。
- **录入方式**：Web 的 Tailscale 与 OpenVPN 页面都提供一体化向导，以单个事务创建出站、单成员 `select` 分组、固定代理入口和可选 TUN 规则；任何一步失败都会恢复旧配置与运行态。OpenVPN 可直接上传 inline `.ovpn`，也可继续手工填写 `server`/`port`/`ca`；高级用户仍可通过 `proxyd nodes add --proxy '<出站JSON>'`、API 的 `POST /api/manual-nodes`（`proxy` 字段）或直接在 `manual-nodes` 写 YAML 映射。Tailscale 的 `auth-key` 可省略；省略时 mihomo/tsnet 生成注册链接，控制台显示后交给 Headscale 管理员批准。
- **tailscale 状态目录固定**：tsnet 出站的 `state-dir` 由 proxyd 统一改写为 `state-dir/tsnet/<安全节点名>-<身份哈希>/`，按节点隔离，防止重启后重新认证与节点身份漂移；若节点映射里显式设置了 `state-dir`，以用户值为准并打警告日志。
- **凭据打码**：`auth-key`、私钥、证书材料等纳入与 remote 模块相同的打码清单——节点/手动节点列表、状态接口与默认配置导出中显示为 `***`，完整值只经完整备份导出返回。

```yaml
manual-nodes:
  - name: ts-exit
    type: tailscale
    auth-key: tskey-auth-...        # 凭据只交给 mihomo tsnet，不由 proxyd 调用 SDK
    exit-node: auto:any             # 可选；留空表示仅访问 Tailnet/子网路由
    accept-routes: true
    udp: true
    ip-version: ipv4-prefer

groups:
  - name: vpn-exit
    port: 43002                     # 应用指向 127.0.0.1:43002 即走选中的 VPN 出口
    type: select                    # url-test/fallback 也可用于组内容灾
    subscription: manual
```

```sh
proxyd nodes add --proxy '{"name":"ts-exit","type":"tailscale","auth-key":"tskey-auth-..."}'
proxyd groups add --type select --subscription manual vpn-exit 43002
proxyd groups select vpn-exit ts-exit
```

**Headscale 管理员审批示例**：在「代理节点 → 添加节点 → VPN 节点 → Tailscale」选择“管理员审批”，填写 `https://hs.example.com`、设备名与访问方式后点击“创建并发起注册”。proxyd 会主动触发 mihomo 的懒加载 tsnet 出站，并在同一对话框显示注册链接、Auth ID 与可复制的 `headscale auth register --user <USER> --auth-id <AUTH_ID>` 审批命令；管理员需把 `<USER>` 替换为实际 Headscale 用户。解析器只接受 tsnet 明确的用户登录提示，不会把 `/machine/register` 等控制面 DEBUG 地址误显示为注册链接。启动中、等待审批或注册失败时可点击“终止并删除”：操作会取消主动探测，并以单个事务清理出站、已清空的引用组、受管 TUN 路由和启动快照，完成后允许同名重新创建；单纯“关闭”只退出对话框，不会删除已经提交的配置。该流程等价于 Tailscale 客户端首次执行 `tailscale up --login-server=...` 的控制面注册，但使用独立的 tsnet 设备身份，不复用本机 Tailscale 客户端状态。首次注册不需要 `--force-reauth`；后续重启复用隔离的 `state-dir/tsnet/...` 身份目录。

**OpenVPN `.ovpn` 导入与认证**：在「代理 → OpenVPN → 添加 OpenVPN」选择文件后，proxyd 解析 `remote`、`proto`、算法、keepalive 及 `<ca>/<cert>/<key>/<tls-auth>/<tls-crypt*>` 内联块，并在提交时转换成 mihomo 原生出站。单文件导入无法读取 `.ovpn` 所引用的 `ca.crt`、`client.key` 等外部相对路径；这类配置会明确拒绝，请先转换成 inline block。多个 `remote` 只使用第一项，mihomo 不支持的非关键指令会在导入摘要中列出。

- `auth-user-pass`：支持。外部凭据文件不会从 proxyd 主机读取，页面会要求填写用户名与登录密码；若 profile 使用 `<auth-user-pass>` 内联内容则直接导入。
- `askpass`：支持传统加密 PEM 与现代 PKCS#8 `ENCRYPTED PRIVATE KEY`。mihomo 没有 askpass 配置字段，因此 proxyd 只在创建请求内使用该口令解密客户端私钥，**askpass 本身不落盘**；解密后的私钥写入权限为 0600 的 proxyd 配置文件，并在列表、状态和默认导出中打码。
- `.ovpn` 的系统路由指令不会直接修改宿主机路由；透明访问范围由向导的“访问方式 + TUN 目标 IP/CIDR”统一管理。只用代理入口时，支持 SOCKS/HTTP 代理的应用连接分组端口；数据库、远程桌面等直连私网 IP 的应用通常选择 TUN 或 both。

> 注意区分「远程连接」（第十章）：那是 tailcat 的**入方向**管理隧道（SSH/SCP/RDP），与本节 mihomo 的**出方向** VPN 出口互不相关，两者共存且不使用对方的网络栈。

**链式代理**（订阅 Clash YAML 节点的 `dialer-proxy`）：proxyd 保留节点映射中的 mihomo 标准字段，可让一个代理节点通过另一个节点或 proxyd 策略组建立连接。订阅合并发生同名重命名时会同步修正同订阅内的引用；被引用的健康节点即使因端口范围容量限制没有独立本地入口，也会作为 proxy-only 出站进入 mihomo 配置。

```yaml
# 以下片段位于订阅返回的 Clash YAML `proxies:` 中，而不是 proxyd 顶层配置。
proxies:
  - name: 链路出口
    type: socks5
    server: 1.2.3.4
    port: 1080
  - name: 链路入口
    type: socks5
    server: 5.6.7.8
    port: 1080
    dialer-proxy: 链路出口
```

链式节点采用两阶段健康检查：先验证普通上游与依赖关系，加载完整 mihomo 代理表后再对链式节点执行端到端 URLTest；完整链路失败时不会保留最终映射端口。`dialer-proxy` 也可以填写 proxyd `groups[].name`，用于通过 `fallback`、`url-test` 或 `load-balance` 上游组拨号。mihomo 已移除旧 `relay` 组，因此 proxyd 不再生成该类型，使用节点级 `dialer-proxy` 是当前替代方案。

**规则配置**（配置文件中，Clash 语法原样支持）：

```yaml
mode: rule
rules:
  - GEOSITE,private,DIRECT
  - GEOIP,private,DIRECT,no-resolve
  - GEOSITE,cn,DIRECT
  - GEOIP,CN,DIRECT,no-resolve
  - MATCH,PROXY
rule-providers:   # 可选，外部规则集
  reject:
    type: http
    url: "https://..."
    behavior: domain
dns:              # 可选，mihomo dns 配置原样透传
  enable: true
```

**geo 数据**：GEOSITE/GEOIP 规则需要数据文件，proxyd 默认从 jsDelivr 镜像（Loyalsoldier 规则仓库）自动下载，无需 GitHub 直连。下载失败时自动降级为本轮不含 GEO 规则运行（日志有提示），后续手动同步或其它配置热更新时会再次尝试。可用 `geox-url` 配置换成自己的镜像。

## 七、刷新与检测机制

| 机制 | 默认周期 | 配置项 | 行为 |
|---|---|---|---|
| 订阅与规则源同步 | 仅手动 | Web“刷新订阅”或对应 API/CLI | 明确操作后才重新下载订阅与规则源 → 测速 → 重新分配端口 → 热更新核心；启动和后台定时任务不会下载 |
| 健康检测 | 5 分钟 | `health-interval` | 复用现有节点列表测速 → 死节点下端口、恢复的节点补位 → 热更新核心 |
| 单次检测超时 | 5 秒 | `health-timeout` | 经节点出口对 `health-url` 发 HTTP 探测；隧道类（VPN）节点放宽为 3 倍 |
| 探测地址 | gstatic 204（HTTPS） | `health-url` | 可换，建议保持 HTTPS（如 `https://cp.cloudflare.com/generate_204`）；HTTP 地址易被机场劫持，导致重复 HEAD 探测失败 |

**端口映射稳定性**：映射快照持久化在 `state-dir/mapping.json`——同一节点在刷新/重启后尽量保持原端口；新节点按延迟从低到高填空闲端口；可用节点多于端口容量时按延迟截断（日志会提示）。

**订阅级端口映射**：除全局 `port-mapping` 总开关外，每个订阅还有自己的映射开关（订阅 YAML 的 `port-mapping` 字段，默认开启；Web 订阅卡片 / `proxyd subs set --mapping on|off` / `PUT /api/subscriptions/{name}` 均可切换）。两者是叠加关系：全局关闭时全部不监听；全局开启而某订阅关闭时，只有该订阅的节点不生成一对一监听。被关闭的节点仍保留稳定端口分配，并继续参与主端口、auto-port 与策略组选路；手动节点没有订阅级开关，只跟随全局开关。

**节点快照（nodes.json）**：每轮手动同步或健康检测后，把合并后的全量节点（完整 proxy 配置、来源订阅、最近测速结果）持久化到 `state-dir/nodes.json`。下次启动时**只加载该快照生成配置提供服务**，不会自动访问订阅地址；用户下一次手动同步成功后再覆盖。快照带格式版本号，解析失败或版本不兼容时记录日志并按空节点启动，不影响控制台使用。

**失败兜底**：手动同步时订阅拉取失败会自动使用本地缓存（`state-dir/cache/`），网络抖动不会清空节点；全部订阅失败或全部节点死亡时保持现有配置不动，等待用户再次手动同步。后台健康检测只检查已有节点，不会触发订阅下载。

**手动同步反馈**：Web 订阅卡片的“同步”会直接下载、解析、测速并应用该订阅，不弹确认框；成功后显示完成通知，失败时显示具体错误。`POST /api/subscriptions/{name}/refresh` 与 `proxyd subs refresh` 具有相同的立即应用语义。全局“刷新订阅”会同步所有启用订阅及规则 URL，接受请求后显示已开始通知；后台不会周期执行该动作。

## 八、存储布局

**配置文件**（`~/.config/proxyd/config.yaml`，`-c` 可改）——用户配置，Web/CLI 的变更自动落盘到这里：

| 内容 | 字段 |
|---|---|
| 订阅列表 | `subscriptions` |
| 手动节点（自有代理 URL/分享链接/结构化 VPN 出站映射） | `manual-nodes` |
| 端口区间/主端口/auto-port/分组端口 | `port-range` / `mixed-port` / `main-auto` / `main-node` / `auto-port` / `groups` |
| 代理模式、自定义规则、规则源 URL | `mode` / `custom-rules` / `rule-urls`（只存 URL，不存规则内容） |
| 系统代理开关、节点正则过滤、健康检测周期等 | `system-proxy` / `include` / `exclude` / `health-interval` / ... |
| 远程连接（隧道开关、暴露端口、远端与转发） | `remote`（token 属凭据，导出默认打码） |
| 远程桌面（服务端口与不含密码的连接档案） | `desktop`（开放状态仍以 `remote.serve` 为准） |

**状态目录**（`state-dir`，默认 `~/.local/state/proxyd`）——运行时状态，删了只会丢缓存/快照，不影响配置：

| 文件 | 内容 |
|---|---|
| `nodes.json` | 最近一次合并后的节点快照（完整 proxy 配置 + 来源 + 测速结果），启动时立即恢复 |
| `mapping.json` | 节点 → 端口的稳定映射快照 |
| `group-selected.json` | select 分组的持久化选中项（分组名 → 节点名） |
| `tsnet/<节点>-<哈希>/` | tailscale 出站按节点隔离的 tsnet 状态目录（配置历史不备份这些文件） |
| `cache/<订阅名>.cache` | 各订阅的原始响应缓存（拉取失败时降级用） |
| `cache/rules-<名>.cache` | 各规则源的原始内容缓存 |
| `proxyd.pid` | 运行中实例的 pid（serve 启动时登记、退出时清理；供 stop/status/防重复启动） |
| `proxyd.log` | 后台模式（start）与开机自启的日志文件 |
| `remote/server.private.json` | 远程连接服务端密钥（0600）：决定本机 token，文件在则 token 重启不变；删除即换全新 token。配置 `remote.key-file` 时改用指定路径，此文件不再使用 |
| `remote/ssh_host_ed25519_key` | Web Terminal 与 builtin-ssh 共用的 SSH host key（0600）；首次使用时原子生成，重启后保持稳定 |
| `cache.db`、`geo*` 等 | mihomo 自身的缓存与 geo 数据文件 |

## 九、配置文件参考

默认路径 `~/.config/proxyd/config.yaml`（`-c` 可指定其他路径）。完整示例见 `configs/config.example.yaml`，关键项：

```yaml
subscriptions:            # 订阅列表，CLI/Web 添加的会自动写在这里
  - name: airport-a
    url: https://...
    type: auto            # auto | clash | share
manual-nodes:             # 手动节点（自有代理），CLI/Web 添加的也会写在这里
  - socks5://user:pass@1.2.3.4:1080#我的节点
  - name: ts-exit         # 结构化隧道类（VPN）出站：tailscale/openvpn/zerotier/wireguard/ssh
    type: tailscale       # 不占端口映射，经分组端口使用；auth-key 等凭据在导出/列表中打码
    auth-key: tskey-auth-...

listen: 127.0.0.1         # 映射端口监听地址；改成 0.0.0.0 可共享给局域网
port-range: [42000, 42100]
mixed-port: 41999         # 主端口（规则模式），Web/CLI 可在线修改
# main-auto: false        # true 时主端口跳过规则、固定走最优节点
# main-node: ""           # 主端口固定节点（节点 Key，见 overview/API）；空=跟随规则；
                          # main-auto 开启时被忽略；节点失效自动回退规则模式
# auto-port: 41998        # 自动选优端口（固定走延迟最低节点），0=关闭
# system-proxy: false     # serve 启动时把系统代理指向主端口
refresh-interval: 24h  # 兼容旧配置保留，当前不再驱动自动订阅同步
health-interval: 5m
health-url: https://www.gstatic.com/generate_204
health-timeout: 5s
include: "香港|日本"          # 可选：只保留匹配节点
exclude: "到期|剩余流量"     # include 之后再排除

mode: rule                  # rule | global | direct
rules: [...]
custom-rules:               # 可选，追加式自定义规则，前置到 rules 之前
  - DOMAIN-SUFFIX,example.com,DIRECT
rule-urls:                  # 可选，远程规则源（mihomo 文本 / gfwlist），内容不写回配置
  - name: gfwlist
    url: https://...
groups:                     # 可选，节点分组端口（支持 url-test/fallback/load-balance/select）
  - name: hk
    port: 43000
    type: fallback
    nodes: ["香港 01"]
  - name: airport-a-auto
    port: 43001
    type: url-test
    subscription: airport-a
  - name: vpn-exit          # select 手动选择出口：proxyd groups select vpn-exit <节点名>，
    port: 43002             # 选中项持久化在 state-dir/group-selected.json
    type: select
    subscription: manual
external-controller: 127.0.0.1:19090   # mihomo API
api-listen: 127.0.0.1:19091            # Web 控制台
# api-secret: ...        # proxyd 管理面 HTTP Basic 口令（用户名 proxyd）；
                         # api-listen 非回环时必填，本地回环也可选开启
# secret: ...             # mihomo API 鉴权
state-dir: ~/.local/state/proxyd       # 状态目录（快照/缓存/pid/日志/geo），见「八、存储布局」

remote:                     # 远程连接（tailcat 隧道），与代理功能独立，详见「十、远程连接」
  enabled: false            # 隧道服务端开关
  serve: [22]               # 经隧道暴露的本机端口
  # key-file: ~/Library/Application Support/tailcat/keys/default.private.json
                            # 可选：自定义服务端密钥文件（tailcat *.private.json），
                            # 指向 tailcat genkey --key=default 的密钥可让两边 token 一致；
                            # 缺省用内置托管密钥 state-dir/remote/server.private.json
  # builtin-ssh: false        # 内嵌免密 SSH：隧道 22 由进程内 SSH 处理（隧道即认证），
                            # 无需系统 sshd；持有 token 即可登录，建议配合白名单
  # web-terminal: false      # 浏览器本机 shell，默认关闭；独立于远程服务端运行（不开
                            # 服务端也可用），非回环 api-listen 上开启必须二次确认
  # allow: [{name: 家里, key: nodekey:..., expires-at: 2026-09-10T12:00:00Z, ports: [22]}]
                            # 客户端最小权限白名单：name/expires-at/ports 均可省；
                            # ports 为空=可访问全部 serve 端口，兼容旧写法 ["nodekey:..."]
  # allow-restricted: false # 过期清扫后防止空列表退化为开放模式的内部状态，通常无需手工设置
  remotes: []               # 保存的远端：name + token
  forwards: []              # 本地常驻转发：listen → remote:remote-port；listen 可留空或填 "auto" 自动分配端口

desktop:                    # 独立远程桌面管理；数据通道复用 remote，不经过 mihomo
  rdp-port: 3389            # 本机操作系统 RDP 服务真实端口
  vnc-port: 5900            # 本机屏幕共享 / VNC 服务真实端口
  connections:              # 客户端常用连接；只引用远端名称，不保存 token 副本或密码
    - name: 办公室电脑
      remote: office        # 引用 remote.remotes 中的 name
      protocol: rdp         # rdp | vnc
      remote-port: 3389     # 对端真实桌面端口；省略时使用协议默认值
      username: DOMAIN\\user # 可选，密码继续由系统桌面客户端管理
```

## 十、远程连接（tailcat 隧道）

「远程连接」是与代理功能**完全独立**的周边模块：内嵌 [tailcat](https://github.com/tailscale/tailcat)（Tailscale 数据面，无控制面），在两台机器之间建立 WireGuard 端到端加密隧道，NAT 打洞失败时走 DERP 中继。**不需要 Tailscale 账号、不需要安装 Tailscale 客户端、不需要 root/TUN 权限**，也不经过 mihomo——代理节点、规则、端口映射与它互不影响。

典型场景：把家里 NAS 的 SSH 安全暴露给外网的自己，无需公网 IP / 端口映射 / frp。

### 概念

- **token（连接凭据）**：服务端启动后生成 `tc...` 字符串，由服务端 WireGuard 公钥 + DERP 区域信息派生。谁拿到 token 谁就能连到服务端的暴露端口——**像密码一样保管**（Web/CLI 默认只显示摘要，配置导出默认打码）。
- **密钥与 token 寿命**：密钥持久化在 `state-dir/remote/server.private.json`（0600），重启后 token 不变；删除该文件即生成全新身份，旧 token 永久失效。配置 `remote.key-file` 可改用自定义密钥文件（如 tailcat 的 default key），此时内置文件不再使用。
- **DERP 中继**：默认使用 tailcat 公共中继（免费、限速、无 SLA）；打洞成功后会升级为直连，中继只是兜底。`remote.region` 留空时自动就近选择，且**进程内保持粘性**——配置变更（白名单/端口等）引发的隧道重建沿用首次探测结果，已分发的 token 不会因区域漂移而失效；彻底固定可显式填区域 ID（如 `302`）。跨区域迁移或脱离公共中继时可填自建 derper 主机名。
- **连接观测**：`proxyd remote remotes list` 会主动探测已保存远端并显示在线状态、直连/DERP 路径与 RTT；Web 展开远端行后立即探测，并每 30 秒刷新。服务端入站客户端可显示在线近似状态、路径与累计收发流量；tailcat 当前不提供服务端侧 RTT，因此该列显示“—”。

### 服务端：暴露本机端口

```sh
proxyd remote on              # 同时开启隧道服务端与 builtin-ssh（单事务热切换并持久化）
proxyd remote serve 22        # 设置暴露端口（可逗号分隔多个）
proxyd remote token           # 打印完整 token，发给要连接的人

# 远程桌面也可直接用 CLI 操作 remote.serve；Web 推荐到独立「远程桌面 → 服务端」页，
# 页面会同时检测系统端口是否真实监听，避免只开放端口却没有运行桌面服务。
proxyd remote serve 22,3389   # Windows RDP
proxyd remote serve 22,5900   # macOS 屏幕共享或其它 VNC 服务
```

隧道内访问 `22` 端口的连接会被转发到本机 `127.0.0.1:22`，因此需要系统 sshd 已在运行。Web 控制台「远程连接」页提供同样能力：顶部是两步快速上手指引（开启服务端 → 开放端口），并有「开放 SSH（22 端口）」快捷按钮一键把 22 加入 serve 列表；serve/转发列表中端口 22 的条目带 SSH 标识。

**内嵌 SSH**默认跟随 remote 总开关：CLI 的 `proxyd remote on|off` 与 Web 的「启用远程连接服务」会在同一事务中同步开启/关闭 builtin-ssh。仍可用 `proxyd remote builtin-ssh on|off` 或 Web 独立开关单独调整。开启后隧道 22 端口改由 proxyd 进程内 SSH 服务器直接处理，**无需系统 sshd（如 macOS 远程登录）**。默认保持隧道免密模式，通过隧道认证即可获得本机 shell（以 proxyd 运行用户身份）；也可按下文额外启用 SSH 公钥认证，并配合 `remote allow` 白名单收窄来源。

**Web Terminal**（`remote.web-terminal`）把进程内 SSH/PTY 会话接到浏览器全屏终端，适合没带 SSH 客户端时应急维护。它默认关闭，由独立的进程内 shell 服务承载，**不要求远程连接服务端运行、也不依赖 builtin-ssh**——只使用客户端功能（远程设备/本地转发）时同样可用。Web 服务状态卡开启后即显示「打开终端」。终端使用 `TERM=xterm-256color`，窗口变化会实时同步 PTY 行列，关闭弹层或网络断开后立即结束子 shell。关闭开关后 `GET /api/remote/terminal` 返回 404。

Web Terminal 等价于把 **proxyd 进程用户的本机 shell** 交给控制台访问者，因此安全边界比普通只读状态页高得多。`api-listen` 为非回环地址（例如 `0.0.0.0:19091`）时，除了必须配置 `api-secret` 的统一认证，Web 开启动作仍会在危险确认框中再次确认；CLI 会在交互终端提示，自动化环境必须显式执行 `proxyd remote web-terminal on --yes`。优先保持 `api-listen: 127.0.0.1:19091`；若确需远程暴露控制台，仍应在网络边界增加访问控制，并仅在使用期间开启 Web Terminal。

```sh
proxyd remote web-terminal          # 查看开关与 API 监听范围
proxyd remote web-terminal on       # 回环监听直接开启；非回环监听要求交互确认
proxyd remote web-terminal on --yes # 非交互环境显式承担非回环暴露风险
proxyd remote web-terminal off      # 关闭入口，新的 WebSocket 握手立即返回 404
```

**客户端最小权限白名单**（`remote allow`，Web 服务状态卡同样可管理）：按客户端 WireGuard 公钥（`nodekey:...`）限制谁能连入。每条授权可设置别名、TTL 与允许访问的目标端口；未设置 TTL 表示永久，`ports` 为空表示可访问全部 `serve` 端口。TTL 在连接建立时实时校验，后台每分钟清扫过期条目并落盘；如果最后一项因过期被清扫，系统保持“拒绝全部”而不会意外退回开放模式。

```sh
proxyd remote allow add nodekey:... 家里                       # 永久授权全部 serve 端口
proxyd remote allow add nodekey:... 临时维护 --ttl 1h          # 一小时后自动失效
proxyd remote allow add nodekey:... 运维 --ports 22,8080       # 仅允许两个目标端口
proxyd remote allow list                                      # NAME / KEY / EXPIRES / PORTS
proxyd remote allow del 家里                                  # 按别名删除，也可填完整公钥
```

客户端公钥在客户端机器上查看：`proxyd remote status`（或 Web 状态卡）里的「本机公钥」。配置文件中写作 `allow: [{name: 家里, key: nodekey:..., expires-at: 2026-09-10T12:00:00Z, ports: [22]}]`，兼容旧格式纯字符串列表（无别名、永久且不限制端口）。用户显式删除最后一项会恢复开放模式；自动过期清扫则保留受限空列表，等待重新添加授权或显式清空。

**连接审计**保存在 remote 独立的 500 条内存环形缓冲中，不会被代理访问日志挤掉。它记录连接建立、拒绝、断开、客户端身份、目标端口、持续时间与双向字节数；重启进程后清空，不包含 payload：

```sh
proxyd remote audit               # 最近 100 条
proxyd remote audit --tail 500    # 最多查询 500 条
```

Web 服务端区域的「连接记录」面板提供同样的只读视图。

### 客户端：连接远端

```sh
# 先保存远端 token（只需一次；也可直接用 token 不保存）
proxyd remote remotes add nas tc...

# 方式一：直接 SSH（调用系统 ssh，隧道作为 ProxyCommand；无需 proxyd 守护进程）
proxyd ssh nas                # 等价 ssh 到 nas 的 22 端口
proxyd ssh root@nas -p 2222   # 指定用户/远端端口；其余参数原样透传 ssh
proxyd ssh nas ls -la         # 带远端命令

# 文件传输直接用 proxyd scp（包装系统 scp，注入 -o ProxyCommand='proxyd remote pipe <token> 22'；
# 远端操作数以对端名称/token 作主机名；因此对端需 serve 22 端口。无独立的文件传输功能）
proxyd scp ./file nas:/tmp/           # 上传
proxyd scp tc-xxxx:/var/log/a.log ./  # 也可直接用 token 不保存远端
proxyd scp -r ./dir nas:/tmp/         # 其余 scp 选项原样透传

# 远程桌面：纯客户端命令，随机绑定 127.0.0.1 端口且不写入配置
proxyd desk rdp office-pc      # 转发远端 3389，打开 Windows/macOS/Linux 已安装的 RDP 客户端
proxyd desk vnc mac-studio     # 转发远端 5900，macOS 直接打开系统“屏幕共享”
# 客户端窗口退出或 Ctrl+C 后，临时 listener、活动连接与 tailcat 客户端全部释放

# 方式二：本地常驻转发（守护进程内运行，适合长期挂载）
proxyd remote forwards add nas-ssh 127.0.0.1:2222 nas 22
proxyd remote forwards add nas-ssh auto nas 22   # listen 留空或填 auto：自动从 10022 起分配空闲本地端口
                                                  #（跳过已被现有转发占用的端口；API 响应与配置落盘均为实际地址）
ssh -p 2222 localhost         # 之后任何 TCP 客户端都能用这条转发
```

SSH 主机指纹提示被禁用（`StrictHostKeyChecking no` + 独立 known_hosts）：隧道本身已完成 WireGuard 双向认证，对端身份由 token 唯一决定。

内嵌 SSH 在 Linux/macOS 上按 **服务端 proxyd 运行用户**的账户配置启动登录 shell，并加载该 shell 的用户启动文件；`user@` 不会切换系统用户。PTY 会话提供真实 `SSH_TTY`，并允许 `proxyd ssh home -o SetEnv=TERM=xterm-256color` 覆盖客户端终端类型，便于远端缺少对应 terminfo 时使用兼容终端。若遇到登录卡住或个人命令缺失，可在登录后检查 `whoami`、`echo "$SHELL"`、`echo "$TERM"`、`echo "$SSH_TTY"`；此类服务端会话修复需要更新并重启服务端 proxyd 才生效。

### SSH 私钥文件登录与服务端公钥管理

tailcat 已升级到 **v0.6.0**，构建需要 **Go 1.27.1 或更新版本**。内嵌 SSH 增加可选的 SSH 公钥认证，默认仍保持原来的隧道免密模式。开启后，客户端既要通过 tailcat 隧道的 token/nodekey 授权，也要持有服务端登记公钥所对应的 SSH 私钥。它与 `remote keyfile`（服务端 WireGuard 身份）及 `--client-key`（客户端 WireGuard 身份）是不同的密钥，不能混用。

客户端使用自己的 SSH 密钥；没有密钥时，可先执行 `ssh-keygen -t ed25519`。将生成的 **`.pub` 公钥文件**交给服务端管理员，私钥留在客户端。服务端执行：

```sh
# 导入一把公钥，也支持包含多把公钥的 authorized_keys 文件。
proxyd remote ssh-keys import ./id_ed25519.pub 工作电脑

# 查看名称、算法和 SHA256 指纹，再开启附加认证。
proxyd remote ssh-keys list
proxyd remote ssh-keys on

# 客户端提供匹配的私钥文件，其他 SSH 参数仍可同时使用。
proxyd ssh home -i ~/.ssh/id_ed25519 -o SetEnv=TERM=xterm-256color

# 按唯一名称或完整 SHA256 指纹撤销授权。
proxyd remote ssh-keys del 工作电脑

# 导出已登记的公钥；只导出公钥，不包含任何私钥。
proxyd remote ssh-keys export ./authorized_keys

# 显式恢复原来的隧道免密登录，已登记的公钥继续保留。
proxyd remote ssh-keys off
```

也可使用 `proxyd remote ssh-keys add 'ssh-ed25519 AAAA…' 名称` 粘贴公钥。Web「远程访问 → 访问授权 → SSH 公钥」提供同样的开关、文件读取、添加、查看、复制和删除功能。文件按内容导入到 proxyd 配置中，后续修改原文件需要重新导入；不读取系统 `~/.ssh/authorized_keys`，不接收私钥，也不支持 `command=`、`from=` 等限制选项，以免忽略限制后授予更大权限。

删除最后一把公钥时，开启中的公钥认证会**继续拒绝全部 SSH 登录**；只有显式关闭附加认证才恢复旧模式。SSH 公钥及认证开关通过配置事务热更新，失败整体回滚；不重建隧道、不改变 token，默认保留已有 SSH 连接。禁用、过期和删除公钥只阻止后续认证，必要时可显式断开已有连接。Web Terminal 仍使用管理 API 和一次性回环令牌认证，不要求上传 SSH 私钥。

### 公钥有效期、会话撤销与诊断

```sh
# 临时禁用与恢复，保留公钥和到期时间。
proxyd remote ssh-keys disable 工作电脑
proxyd remote ssh-keys enable 工作电脑

# 绝对到期时间（含时区）；never 恢复永久。
proxyd remote ssh-keys expire 工作电脑 2026-10-01T18:00:00+08:00
proxyd remote ssh-keys expire 工作电脑 never

# 状态包含禁用/过期、最近认证时间、活动连接数与完整指纹。
proxyd remote ssh-keys list

# 如需禁止重连先禁用公钥，再按 list 或审计中的完整指纹断开现有会话。
proxyd remote ssh-keys disconnect 'SHA256:完整指纹'
proxyd remote audit --tail 100

# 在客户端运行；无需向服务器上传私钥，使用系统 OpenSSH 的密钥与 agent。
proxyd ssh home --diagnose -i ~/.ssh/id_ed25519 -o SetEnv=TERM=xterm-256color
```

到期时间在每次认证时检查，到期时刻本身即失效；无需等待后台清扫。最近使用时间只在完成签名验证后更新，公钥探测不算成功登录。最近认证时间及审计记录仅保留于当前进程，重启后清空；连接审计最多保留 500 条。公钥文本导出不包含禁用/到期等管理元数据，完整策略应通过配置备份恢复。

诊断依次检查隧道端口、SSH 版本响应、SSH 握手与认证、真实交互登录 shell。它会启动用户的登录 shell 并只输出 USER、SHELL、TERM、PATH、SSH_TTY；用户启动脚本会照常执行。隧道连接最多 30 秒，SSH 版本响应最多 5 秒，认证与 shell 检查最多 30 秒。为避免无人值守时等待密码，诊断启用 BatchMode；加密私钥可先通过 `ssh-add` 加入 agent。需要支持 `proxyd-diagnostics` 子系统的新版内嵌 SSH 服务端；系统 sshd 或旧版服务端会提示不支持。诊断包含一次只读取 SSH 版本的连接探测，因此审计可能出现一条未完成认证的连接记录。

Web 在「设备与连接」提供复制诊断命令入口；「访问授权 → SSH 公钥」可设置到期时间、禁用/启用及断开已有会话。「连接审计」集中展示隧道及 SSH 事件。

v0.6.0 新生成的服务端身份会持久化 WireGuard PSK，重启与公钥配置变更不会重新生成 PSK。旧密钥文件缺少 PSK 时保留兼容模式及原 token；导入带 PSK 的新版 tailcat 密钥文件则保留其中的 PSK，客户端需要使用支持 PSK 的版本。

Web 控制台把两类任务分为两个侧边栏页面。「远程连接」继续按“服务端 / 客户端”管理 tailcat 身份、SSH、token 和通用端口转发；设备的“连接”对话框只提供 SSH/scp 用法，不再混放 RDP/VNC。「远程桌面」则专门管理桌面：服务端按 RDP/VNC 展示“系统服务是否真实监听”和“隧道是否开放”两个状态，端口可按操作系统实际配置修改；客户端保存常用连接档案，一键创建守护进程内的临时转发并下载 `.rdp` 文件或打开 `vnc://` 系统处理器。档案不保存密码，token 也只在 `remote.remotes` 保留一份。

Web 临时会话绑定在**运行 proxyd 的机器**的 `127.0.0.1`，因此一键打开系统客户端只适合浏览器与 proxyd 同机的场景；若通过局域网远程访问 Web，页面会显示警告。纯 CLI 的 `proxyd desk` 仍从当前登录用户会话启动 GUI，适合不希望由 root/system 开机守护进程直接拉起桌面窗口的场景。Web 会话若始终没有客户端连接、客户端断开后长期空闲或超过最长寿命，会由单个清扫协程自动关闭 listener、活动连接和 tailcat 客户端。

RDP/VNC 的应用层流量进入临时 TCP 转发后仍由 tailcat 承载：DERP 负责初始引导与打洞失败兜底，magicsock 会持续尝试 NAT 穿透；成功后升级为 WireGuard 点对点直连。RDP 可以在 TCP-only 模式运行，但中继路径的延迟与吞吐通常不如直连。服务端应同时保留 RDP/VNC 自身账号认证，并用 `remote allow --ports 3389,5900` 将桌面端口限制给可信客户端。

### 与 tailcat CLI 的互通

proxyd 的 token 与官方 `tailcat` CLI 完全互通：对方可以用 `tailcat ssh <token>` 连你的 `proxyd remote serve 22`，你也可以 `proxyd ssh <tailcat token>` 连官方服务端。

注意 token 由服务端密钥决定，proxyd 默认使用自己的内置密钥（`state-dir/remote/server.private.json`），与 `tailcat genkey --key=default` 生成的密钥不同，两边 token 自然不一样。若想让两者一致（例如已把 tailcat genkey 的 token 发给对端），让 proxyd 复用同一把密钥即可：

```sh
# macOS 上 tailcat 的 default key 通常在 ~/Library/Application Support/tailcat/keys/default.private.json
proxyd remote keyfile "~/Library/Application Support/tailcat/keys/default.private.json"
proxyd remote keyfile -     # 恢复内置托管密钥
proxyd remote keyfile       # 查看当前生效的密钥文件
proxyd remote keyfile export ./server.private.json  # 导出当前实际身份，文件权限收紧为 0600
proxyd remote keyfile import ./server.private.json  # 校验并事务导入到内置托管路径
```

Web 控制台「远程连接」页的服务状态卡中也可设置、导出和导入。导出走专用下载端点，私钥不会进入普通状态接口；导入会先校验 tailcat 格式，再以原子写入和完整回滚切换到内置托管密钥。切换密钥即更换身份，运行中的隧道会重建，token 随导入身份更新。

### 限制

- tailcat 上游不承诺 API 与 wire format 稳定性，proxyd 锁定依赖版本升级；跨版本互联失败时先对齐版本。
- 公共 DERP 中继限速，大流量场景（如长时间文件传输）建议自建 derper。
- 文件传输通过 `proxyd scp`（包装系统 scp 走隧道）完成，不提供独立的文件传输子协议；不含 tailcat cp/recv、SOCKS 与 exit-node。

## 十一、LAN 网关（旁路由）

把运行 proxyd 的主机变成局域网设备的网关：设备把「网关/路由器」（和 DNS）改指本机后，其流量经本机 mihomo 分流，与控制台规则/分组共用同一出口体系。定位是**旁路由**：不接管 DHCP，设备指向错误只影响该设备本身，不会拖垮全家网络。

### 平台矩阵与特权模型

| 平台 | 执行层 | 特权要求 |
|---|---|---|
| macOS | pf anchor：下游 TCP `rdr` 到 mihomo `redir-port` | root 特权 helper（launchd 常驻，unix socket 白名单指令，主进程永远普通用户） |
| Linux | nftables redirect（TCP）/ tproxy（UDP） | setcap 能力位：`cap_net_admin,cap_net_raw,cap_net_bind_service` |
| Windows | 不支持 | — |

数据面**不依赖 TUN**（ADR 0003 勘误）：redir/tproxy 入口与设备规则都写进 mihomo 配置，设备级分流用 `SRC-IP-CIDR` 规则前置于 custom-rules。启用网关要求代理模块已启用；禁用代理会先自动停用网关（联动持久化）。

Linux 启用网关时会自动安装 UDP TPROXY 所需的策略路由：报文使用 proxyd 专用 `fwmark 0x7078/0xffff`，规则优先级为 `12026`，本地路由表为 `20260`。重复应用会精确替换同一条规则，停用/退出时只清理这些专用对象，不会刷新用户的其他策略路由。宿主机需同时提供 `nft` 与 iproute2 的 `ip` 命令；proxyd 会把自身的 `CAP_NET_ADMIN` 以 ambient capability 传给这两个子进程，因此仍只需对 proxyd 二进制执行上面的 `setcap`。

### 启用步骤

1. 启用前检查：`proxyd gateway precheck`（或 Web「网关」页预检卡）。
   - macOS 未就绪时安装 helper：`sudo proxyd gateway helper install`（launchd 系统域常驻；`proxyd gateway helper status|uninstall` 查看/卸载）。helper 只接受 pf 应用/清除与转发开关的白名单指令，不读业务配置、不连网。
   - Linux 未就绪时按指引执行：`sudo setcap 'cap_net_admin,cap_net_raw,cap_net_bind_service=+ep' <proxyd 二进制路径>` 后重启 proxyd（每次替换二进制需重设，与 TUN 同一条指引）。
2. 启用模块：Web「网关」页开关，或 `proxyd modules gateway on`。
3. 登记设备：Web 页「登记设备」，或 `proxyd gateway devices add <名> <ip> [direct|proxy|group:<分组名>]`（策略缺省为 proxy）。设备表为空时网关不生效（零值配置不触碰系统）。
4. 在下游设备上把网关改指本机局域网 IP；开启 `dns-redirect` 时 DNS 也改指本机。

### DNS 劫持

`gateway.dns-redirect: true`（默认开）时，pf/nftables 把下游 53 端口 redirect 到 mihomo dns 的非特权监听 `0.0.0.0:1053`（helper 白名单不含绑端口操作，53 属特权端口，故监听落在 1053）。用户手写 `dns:` 段已有 `listen` 时不覆盖并打日志，请自行确认其与 redirect 目标一致。

### 已知限制

- **macOS UDP**：阶段一仅支持 DNS 劫持 + 直连（macOS 无 tproxy），QUIC/DoH 行为可能与直连不同；需要完整 UDP 分流请使用 Linux（tproxy）。
- **Linux 内核能力**：UDP 透明代理要求内核启用 nftables TPROXY 与 policy routing（主流 Linux 4.18+ 通常具备）；缺少 `NFT_TPROXY`、`nft` 或 `ip` 时，启用会失败并在网关状态/诊断中显示具体阶段。开启 DNS 劫持时 UDP/53 走专用 DNS redirect，其余 UDP 才进入通用 TPROXY。
- **看门狗**：主进程失联超过 90 秒（心跳间隔 30 秒），helper 自动清除 pf 规则并恢复 IPv4 转发原值，下游设备回退直连，避免主进程崩溃后下游断网。
- **pf 主规则集**：macOS 载入方式是在 /etc/pf.conf 基础上追加本模块 anchor 引用后 `pfctl -f` 整体载入（子 anchor 的 rdr 必须被主规则集引用才生效，与 ClashX 增强模式同）；其他软件写在 pf 主层的规则会被替换，`com.apple/*` anchors 不受影响。卸载（`proxyd gateway helper uninstall`）会清 anchor、恢复转发并删除全部文件。

### 故障排查

- Web「网关」页状态卡/预检卡，或 `proxyd gateway status` / `proxyd gateway precheck`。
- 诊断中心（`proxyd diagnose`）含 gateway 步骤：平台支持性、helper 握手与协议版本、转发开关实际值、规则应用、redir 入口监听。
- macOS helper 日志：`/var/log/com.proxyd.gateway-helper.log`；握手报版本不兼容时重新 `sudo proxyd gateway helper install`。
- Linux 报权限不足：确认 setcap 能力位是否在最近一次替换二进制后重设。
- Linux UDP 不通：在网关状态中确认 `policy_routing=present`；诊断项 `gateway_policy_route` 会同时检查 fwmark 规则与 lo 本地路由，避免只装好其中一半时误报可用。

## 十二、常见问题

- **启动后没有映射端口**：首次添加订阅后需手动点击“刷新订阅”或执行 `proxyd refresh`；已有快照仍为空时再检查同步错误与 `health-url` 是否可达。
- **geo 下载慢/失败**：已内置镜像，仍失败可在配置 `geox-url` 换源；失败不影响代理本体（自动降级）。
- **geo 报 `permission denied`**：多因曾用 `sudo`（如 TUN 授权）或其他用户运行过 proxyd，导致 `state-dir` 或其中 geo 文件属主异常。启动时 proxyd 会自动删除目录可写但不可读的 geo 文件让 mihomo 重新下载；若日志提示目录本身不可写，执行 `sudo chown -R $(id -un):$(id -gn) <state-dir>` 后重启即可。
- **启动报 pid/配置文件 `permission denied`**：同样是属主异常。终端里运行 `serve`/`start` 时 proxyd 会探测到权限不足并提示是否修复，确认后执行一次 `sudo chown -R`（sudo 会要求输入登录密码）把属主归还给当前用户，然后继续以普通用户运行；非终端环境（如开机自启）则打印可手动执行的 chown 命令。
- **改了配置文件什么时候生效**：模式经 Web/API 切换即时生效；其他改动重启进程生效（`mapping.json` 保证端口不漂）。
- **节点数多于端口数**：按延迟保留最快的一批，其余节点仍在主端口的 PROXY 选择组里可用。
- **端口被占**：换 `port-range` / `mixed-port` / `auto-port` / `api-listen` / `external-controller`（分组端口同理）。
- **异常退出后系统代理没恢复**：`proxyd sysproxy off` 手动关闭（正常退出会自动恢复）。
- **proxyd stop 提示未在运行但进程还在**：异常退出可能留下过期 pid 文件，stop 会自动清理；确认进程残留时手动 kill。
- **重启后节点还在吗**：在。配置里有订阅/手动节点；`state-dir/nodes.json` 快照让启动即刻可用，`mapping.json` 保证端口不漂。


## 管理菜单与功能归属

控制台顶部横向排列「概况、代理、网关、远程访问、系统」，侧栏只显示当前大类的子菜单，命令菜单（⌘K/Ctrl+K）可以按页面名、SSH、公钥等关键词直接跳转。远程访问拆为设备与连接、本机服务、访问授权、端口转发、连接审计和远程桌面；不再将全部功能放进服务端/客户端两个长页签。网关大类随 gateway 模块启用出现，集中承载状态、预检、设备登记表与使用指引。

页面地址如 `#/remote/access` 可直接打开，支持刷新定位及浏览器前进/后退；旧 `#/remote` 入口兼容到设备与连接。后续功能落位规则见 [管理导航规划](management-navigation.md)。


## 模块启停与终端最小化

Web 在“系统 → 模块管理”集中启停代理与远程访问，也可以使用业务侧栏底部的模块开关。禁用保留订阅、节点、端口、密钥和设备配置，管理 API/Web 始终可访问。重新启用后沿用各子功能的开关；已结束的连接需要重新建立。

```bash
proxyd modules list
proxyd modules proxy off
proxyd modules proxy on
proxyd modules remote off
proxyd modules remote on
proxyd modules gateway on
```

`gateway` 模块对应 LAN 网关（见「十一、LAN 网关」）：禁用会清除转发规则并停止调和，设备表保留；启用要求代理模块已启用，禁用代理会先自动停用网关。

配置中的 `proxy-disabled: true` 会停止代理入口、TUN、DNS、活动代理连接与周期健康检测，撤销已配置的系统代理，保留 `system-proxy` 的恢复偏好。恢复时只检测缓存/手动节点，不会下载订阅。纯远程服务可以使用该设置且不配置代理订阅。

`remote.disabled: true` 会停止隧道服务端、固定/临时转发、远程桌面连接和 Web Terminal；它不会覆盖原有的 `remote.enabled` 服务端开关。因此原来的纯客户端模式仍然可用。独立执行的 `proxyd ssh` / `remote pipe` 进程不由守护进程模块开关控制。

HTTP 管理入口：`GET /api/modules`，`POST /api/modules/proxy` 或 `/api/modules/remote`，请求体 `{"enabled":false}`。与其他管理 API 使用相同认证。

Web Terminal 工具栏的最小化按钮会将会话停靠在右下角。最小化期间可切换任意大类，恢复后保留连接与历史输出；停靠栏可以直接恢复或关闭。每个标签页保留一条会话，已有会话时再次点击终端入口会恢复它。刷新页面或关闭标签页仍会断开；关闭远程模块或 Web Terminal 开关会在后端结束对应会话。

## DERP 地图故障与开机恢复

默认地图 `https://tailcat.dev/derpmap.json` 的 DNS/HTTP 请求失败时，会尝试 [Tailscale 官方 DERP 地图](https://tailscale.com/docs/reference/derp-servers) `https://controlplane.tailscale.com/derpmap/default`。自动选定的完整区域保存在 `<state-dir>/remote/derp-region.json`，重启复用，避免重复依赖地图查询及 token 区域漂移。自定义 `remote.derp-map-url` 不会自动切换到公共地图。

开机网络尚未就绪导致远程服务启动失败时，控制台显示实际错误，后台每 30 秒重试；每次发现最多 15 秒，关闭远程模块或服务端开关后停止重试。缓存不会让不可达的中继变得可达；公共来源都无法访问时，仍需配置可达的 `remote.derp-map-url` 或 `remote.region` 自建中继主机名。

如需主动重新探测区域，停止远程服务后删除 `remote/derp-region.json` 再启动。仅删除区域缓存，保留 `server.private.json` 等身份文件。重新选区可能改变 token，需重新分发。

### 运行状态、诊断与配置恢复

系统菜单提供模块运行阶段、立即重试、诊断中心及配置历史。命令行可使用 `proxyd modules remote retry`、`proxyd diagnose home --json` 和 `proxyd config history list`。配置历史只在内容变化时归档变更前快照，跳过连续重复记录，最多保留 30 个版本；恢复必须预检，重启后生效。去重直接比较完整配置内容的 MD5，不递归解析 YAML；注释、排版和默认值补齐同样算内容变化，规范化保存后重复重启不会再增加记录。MD5 仅用于去重，恢复预检仍使用独立的 SHA-256 摘要。
