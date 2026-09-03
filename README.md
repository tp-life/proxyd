# proxyd

多节点端口映射代理工具。与 Clash / mihomo 等客户端"一次只用一个节点"不同，proxyd 把订阅里的**每个可用节点各映射到一个本地端口**（HTTP + SOCKS5 混合端口，固定出口），让所有节点同时可用；另保留一个走常规规则模式（rule/global/direct）的主端口。

核心基于 [mihomo](https://github.com/MetaCubeX/mihomo)（Clash.Meta）以 Go 库方式内嵌运行，单二进制交付，**无需单独安装 mihomo**。

📖 完整文档：[docs/manual.md](docs/manual.md)（架构、协议、规则、刷新机制、配置参考、FAQ）

## 功能

- 多个订阅源（Clash YAML 和 base64 分享链接两种格式，自动嗅探）
- 手动节点（`manual-nodes`）：自有 http(s)/socks5 代理或 ss/vmess 等分享链接，与订阅节点同等参与测速/映射/分组
- 可用节点自动映射到指定端口区间，映射关系稳定（重启/刷新后同一节点尽量保持原端口）
- 逐节点端口映射可独立热开关（`port-mapping`）：关闭时保留稳定分配关系，不启动对应监听端口，重新开启后恢复原映射
- 节点快照持久化（`state-dir/nodes.json`）：启动即恢复最近一次的可用节点提供服务，断网/订阅挂掉也不丢节点
- 定时刷新订阅（`refresh-interval`）+ 定时健康检测（`health-interval`），死节点自动下端口、新节点自动补位
- 主端口完整支持 Clash 规则与三种代理模式（rule / global / direct），可热切换；可在线修改端口号（`POST /api/main-port` / `proxyd main-port`）
- 可选「主端口使用最优节点」（`main-auto`）：主端口跳过规则、固定走 AUTO 选优组，与 auto-port 并存互不影响
- 可选「主端口固定节点」（`main-node`）：不开优选时主端口跳过规则、直达指定节点；节点失效自动回退规则模式，恢复后自动再生效
- 可选「自动选优端口」（`auto-port`）：独立端口固定走全部可用节点中延迟最低者（url-test 组）
- 自定义规则（追加式，前置到内置规则之前）+ 规则 URL 导入（mihomo 规则文本 / gfwlist）+ 节点分组端口（支持 url-test / fallback / load-balance，也可按订阅动态生成成员）
- 链式代理：透传 Clash 节点的 `dialer-proxy`，自动修正订阅合并后的名称引用并执行完整链路测速；支持指向节点或 proxyd 策略组，不再使用 mihomo 已移除的 `relay` 组
- 后台守护模式：`proxyd start|stop|restart|status`（日志落 `state-dir/proxyd.log`）；`proxyd` 无参数等价于 `serve`
- 开机自启：`proxyd autostart on|off|status`（macOS launchd / Linux systemd user / Windows 注册表）或 Web 设置页开关
- 系统代理开关：CLI `proxyd sysproxy on|off|status` 或 Web 设置页，把系统代理指向主端口（macOS networksetup / Linux gsettings / Windows 注册表）
- TUN 模式：配置/API/Web/CLI 热开关，支持完整系统 TCP/UDP 流量接管；macOS sudo、Linux root/CAP_NET_ADMIN、Windows 管理员权限不足时返回平台指引
- DNS 预设：`off | fake-ip | redir-host` 三档热切换，手写 `dns:` 始终优先；TUN 开启时控制台建议 fake-ip
- 配置备份与恢复：Web 设置页默认导出打码 YAML，也可显式导出含凭据的完整备份；导入通过完整校验并原子落盘，重启后生效
- 新版本提示：启动后异步检查官方 GitHub Releases，概览页仅在发现更新时提示；可用 `check-updates: false` 或设置页开关关闭
- 完整 CLI 管理命令（`mode/subs/nodes/rules/rule-urls/groups/logs/tun/port-range/auto-port/main-*/dns-preset/update-check/conn/traffic/config/refresh/test`），作为本地 API 客户端操作运行中的实例
- 远程连接（周边功能，与代理独立）：内嵌 [tailcat](https://github.com/tailscale/tailcat) 隧道（WireGuard 端到端加密 + DERP 中继，无需 Tailscale 账号/客户端），把本机端口（如 SSH 22）暴露给持有 token 的对端；`proxyd ssh <远端>` 一键经隧道连接，`proxyd scp` 直接经隧道传文件，支持本地常驻转发（listen 可留空自动分配端口），详见 [手册](docs/manual.md#十远程连接tailcat-隧道)
- REST API 与 Web 控制台：
  - `http://127.0.0.1:19091/` 内嵌 React 19 + Tailwind 4 Web 控制台（通过官方 Registry 集成 beUI Button、Switch、Tabs 与 Table 源码，不依赖 Radix UI；概览含实时速率条，节点页显示订阅流量/到期信息，活动连接页展示域名、入口、进程与出口链路）
  - `http://127.0.0.1:19090` mihomo external-controller（兼容 metacubexd / yacd 面板）
- 订阅拉取失败时自动降级到本地缓存
- 单二进制，跨平台（macOS / Linux / Windows，amd64 / arm64）

## 安装

从 Release 下载对应平台压缩包，或自行编译：

```sh
make              # 一次性完整构建：先构建 Web，再生成嵌入前端的 bin/proxyd
make all          # 与 make 等价，适合在脚本或 CI 中显式调用
make build        # 输出 bin/proxyd
make release      # 交叉编译全部平台到 dist/
```

## 使用

最快上手——直接给订阅地址，无需配置文件：

```sh
proxyd serve https://example.com/api/v1/client/subscribe?token=xxx
# 等价快捷形式：proxyd https://example.com/...
# 多订阅直接追加：proxyd serve <url1> <url2>
# 自定义区间：proxyd serve -range 43000-43100 <url>
```

**订阅地址会自动保存到默认配置文件 `~/.config/proxyd/config.yaml`**，之后直接 `proxyd`（不带任何参数，等价于 `proxyd serve`）即可。也可以在 Web 控制台里随时增删订阅（同样自动落盘）。

不给端口区间时默认用 `42000-42100`（主端口 `41999`，规则模式入口），内置默认规则（私网/国内直连，其余走代理）。

后台守护与开机自启：

```sh
proxyd start        # 后台运行（日志 state-dir/proxyd.log），自动等待就绪并打印 Web 地址
proxyd status       # 查看 pid / 端口 / Web 地址
proxyd restart      # 重启
proxyd stop         # 停止（SIGTERM 优雅退出）
proxyd autostart on   # 注册开机自启（macOS launchd / Linux systemd user / Windows 注册表）
```

日常管理也有全套 CLI（操作运行中实例，需先 start/serve）：

```sh
proxyd mode [rule|global|direct]      # 查看/切换模式
proxyd nodes                          # 按订阅分组列出节点/端口/延迟/失败原因
proxyd nodes add socks5://u:p@1.2.3.4:1080 我的节点   # 添加手动节点
proxyd subs list | proxyd subs add <名> <url> | proxyd subs del <名>
proxyd subs set --rename 新名 旧名 | proxyd subs set --disable <名>   # 修改/启停订阅
proxyd subs refresh <名> | proxyd subs test <名>                      # 单订阅刷新/测速
proxyd rules list | proxyd rules add "DOMAIN-SUFFIX,example.com,DIRECT" | proxyd rules del 0
proxyd rules set 0 "<规则>" | proxyd rules move 0 2                   # 改规则 / 调优先级
proxyd rule-urls list|add|del|show <名>   proxyd groups list|add|set|del
proxyd groups add --type fallback --subscription airport-a hk 43000
proxyd logs --tail 200 --level error
proxyd tun status | proxyd tun on | proxyd tun off
proxyd port-mapping [on|off|status] # 开关/查看逐节点端口映射
proxyd port-range 43000-43100    proxyd auto-port 41998|off
proxyd main-auto [on|off]        proxyd main-port 42999   # 主端口最优节点开关 / 改主端口
proxyd main-node <节点名|key|off>                         # 主端口固定节点（可按名称）/ 清除
proxyd conn list | proxyd conn close <id|all>             # 活动连接查看/关闭
proxyd traffic                                            # 实时上/下行速率
proxyd dns-preset [off|fake-ip|redir-host]   proxyd update-check [on|off]
proxyd config export [--full] -o 备份.yaml | proxyd config import 备份.yaml
proxyd refresh                   proxyd test

# 远程连接（tailcat 隧道，与代理独立）
proxyd remote on               proxyd remote serve 22      proxyd remote token
proxyd remote remotes add nas tc...                        # 保存远端
proxyd remote forwards add nas-ssh auto nas 22             # 本地常驻转发（listen 填 auto 自动分配端口）
proxyd ssh root@nas            # 经隧道 SSH（纯客户端命令，无需守护进程）
proxyd scp ./file nas:/tmp/    # 经隧道 scp 传文件（对端需 serve 22）
```

启动后打开 **Web 控制台 `http://127.0.0.1:19091/`**：侧边栏以 Clash Verge 风格拆分为 运行概况 / 代理节点 / 订阅管理 / 代理入口 / 策略分组 / 访问规则 / 运行日志 / 活动连接 / 系统设置。控制台可查看实时流量趋势、搜索与筛选节点、启停和编辑订阅、开关逐节点端口映射、编辑分组与自定义规则、暂停连接或日志刷新，以及一键切换 规则/全局/直连 模式。系统设置采用页内分区导航，可管理主端口、自动选优、TUN、系统代理、开机自启与配置备份；配置导入会先展示差异预览，确认摘要未变化后才原子写入。常规状态每 10 秒刷新，活动连接页停留期间每 2 秒更新；并提供 `⌘K` / `Ctrl+K` 命令面板执行跳页、刷新、测速与模式切换。

也可以命令行开关系统代理（指向主端口）：

```sh
proxyd sysproxy on       # 系统代理 → 127.0.0.1:41999
proxyd sysproxy status
proxyd sysproxy off
```

需要精细控制（规则、DNS、排除正则、周期等）时再上配置文件：

```sh
cp configs/config.example.yaml proxyd.yaml   # 编辑订阅地址等
proxyd check -c proxyd.yaml                  # 先跑一遍：拉订阅、测速、打印端口映射表
proxyd serve -c proxyd.yaml                  # 常驻运行（位置参数的订阅地址会追加到配置文件的订阅列表）
```

`check` 输出示例：

```
PORT   NODE              SUBSCRIPTION   DELAY
42000  香港 01           airport-a      120ms
42001  日本 03           airport-a      185ms
...
```

之后 `curl -x http://127.0.0.1:42000 https://api.ipify.org` 就走"香港 01"出口；`41999`（主端口）走规则模式。

## 配置要点

见 `configs/config.example.yaml` 注释。核心项：

- `subscriptions`：多个订阅，`type` 默认 auto 自动嗅探
- `manual-nodes`：手动节点（http(s)/socks5 URL 或分享链接），落盘在配置文件，与订阅节点同等待遇
- `port-range`：节点映射端口区间；`mixed-port`：主端口（默认区间前一位，Web/CLI 可在线修改）
- `port-mapping`：逐节点端口映射总开关（默认 true）；关闭后不启动映射监听，但稳定端口分配仍保留并可在 Web 查看
- `main-auto`：主端口使用最优节点（默认 false）——主端口跳过规则、固定走 AUTO 选优组，与 auto-port 并存互不影响
- `main-node`：主端口固定节点（默认空）——不开优选时主端口跳过规则、直达指定节点（存节点 Key）；`main-auto` 开启时被忽略；节点失效自动回退规则模式、配置保留
- `auto-port`：自动选优端口（0=关闭），固定走全部可用节点中延迟最低者，与主端口模式互不影响
- `system-proxy`：true 时 serve 启动即把系统代理指向主端口，退出自动恢复
- `tun`：完整透传 mihomo TUN 段；默认关闭，`stack: system`、自动路由/出口识别、劫持 `0.0.0.0:53`，可通过 Web 或 `proxyd tun` 热切换
- `dns-preset`：`off | fake-ip | redir-host`，默认 off；配置了手写 `dns:` 时整段优先于预设
- `mode`：`rule | global | direct`
- `custom-rules`：追加式自定义规则，生成时前置到 `rules` 之前（如 `DOMAIN-SUFFIX,example.com,DIRECT`，策略可填节点名/分组名）
- `rule-urls`：远程规则源（`name`/`url`），支持 mihomo 规则文本与 gfwlist（base64），内容跟随订阅刷新拉取并缓存到 `state-dir`，不写回配置文件
- `groups`：节点分组端口——`name`/`port`/`type`/`nodes` 或 `subscription`；`type` 支持 `url-test | fallback | load-balance`，端口独占一个 mixed listener
- 订阅节点中的 `dialer-proxy`：按 mihomo 语义透传，可引用同订阅节点或 `groups` 名称；被引用节点即使没有独立映射端口也会注册为出站
- `include` / `exclude`：按节点名正则先做白名单、再做黑名单过滤；用于只保留地区节点并剔除机场信息节点
- `rules` / `rule-providers` / `dns`：Clash 语义原样透传 mihomo；手写 `dns` 优先于 `dns-preset`
- `state-dir`：状态目录（快照/缓存/pid/日志/geo 数据），默认 `~/.local/state/proxyd`——见手册「存储布局」

注意：GEOIP/GEOSITE 规则需要 geo 数据文件。proxyd 默认从 jsDelivr 镜像（Loyalsoldier 主流规则仓库）下载，开箱即用；下载失败时会在日志提示并**自动降级为不含 GEO 规则运行**（其余规则照常），下一轮刷新自动重试恢复。也可以在配置里用 `geox-url` 换成自己的镜像：

```yaml
geox-url:
  geosite: "https://你的镜像/geosite.dat"
  geoip: "https://你的镜像/geoip.dat"
  mmdb: "https://你的镜像/Country.mmdb"
```

## 常驻运行

一条命令注册开机自启（推荐）：`proxyd autostart on`（`status`/`off` 查看与移除）。

- **macOS**：写 `~/Library/LaunchAgents/com.proxyd.plist`（RunAtLoad + KeepAlive，日志指向 state-dir/proxyd.log）并 `launchctl bootstrap` 立即启动
- **Linux**：写 `~/.config/systemd/user/proxyd.service` 并 `systemctl --user enable --now`（日志看 `journalctl --user -u proxyd`）
- **Windows**：写注册表 `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`，登录时执行 `proxyd start`（派生后台进程，不弹控制台窗口）

## 开发

```sh
go test ./...     # 单元测试 + 端到端测试（e2e/，本地假节点全流程验证）
make              # 推荐：先构建 Web，再编译嵌入最新前端资源的 bin/proxyd
make web          # 构建 React 控制台到 internal/api/dist，供 Go embed 使用
make build        # 只构建 Go 二进制，使用已生成的前端 dist，不强制依赖 Node
```

项目结构：`cmd/proxyd`（CLI/守护进程/本地 API 客户端；`cli.go` 提供共享 apiClient，功能按域拆分为 `proxy_*.go`、`system.go`、`remote.go`）、`internal/config`（配置；`config.go` + `proxy.go`/`remote.go`）、`internal/proxy`（代理域包：`subscribe` 订阅拉取与解析、`ruleurl` 规则 URL 拉取与解析、`node` 节点模型与 nodes.json 快照、`pool` 健康检测与端口分配、`core` mihomo 配置生成与内嵌运行、`sysproxy` 系统代理开关、`tunperm` TUN 跨平台权限检测）、`internal/remote`（tailcat 隧道远程连接，独立周边模块）、`internal/app`（调度编排；`app.go` 内核：生命周期/持久化/版本检查 + `proxy_*.go` 域逻辑）、`internal/api`（自有 REST API 与 Web 控制台 embed 产物；`api.go` Server 外壳 + 各域 `proxy_*.go`/`system.go`/`remote.go` 路由文件）、`web`（React 控制台源码；`main.jsx` App 外壳 + `pages/`/`hooks/`/`lib/`/`components/`）、`internal/autostart`（开机自启）。
