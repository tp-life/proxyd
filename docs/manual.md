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
                 │        调度器（app）：定时刷新订阅 + 定时健康检测，变化时热更新核心        │
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

### 最快启动

```sh
proxyd serve https://你的订阅地址
```

就这一条命令。首次运行会：

1. 拉取订阅并解析节点
2. 对所有节点做健康检测（延迟测试）
3. 把可用节点映射到默认端口区间 `42000-42100`
4. 自动生成默认配置并保存到 `~/.config/proxyd/config.yaml`

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
| `proxyd restart` | stop + start |
| `proxyd status` | 运行中显示 pid、端口、Web 地址、API 健康状态，并追加实例汇总（模式/节点存活/端口映射/主端口策略/系统代理/TUN/DNS/自启/新版本提醒） |
| `proxyd check ...` | 一次性自检：打印节点/端口映射表，参数同 serve |
| `proxyd sysproxy [-c 配置] on\|off\|status` | 开关/查看系统代理（指向主端口；flag 需放在操作前） |
| `proxyd tun [-c 配置] on\|off\|status` | 开关/查看 TUN 模式及当前进程权限（操作运行中实例） |
| `proxyd autostart [-c 配置] on\|off\|status` | 开关/查看开机自启（flag 需放在操作前） |

### 本地管理命令（CLI ↔ Web 对齐）

以下命令全部作为**本地 API 客户端**实现：读取配置文件拿 `api-listen` 地址，HTTP 调用运行中实例；实例未运行时提示「请先 proxyd start」，不产生离线改配置的旁路。错误信息原样透传 API 报错。

| 命令 | 说明 |
|---|---|
| `proxyd status` | 运行状态汇总：pid、端口、模式、节点存活数、端口映射、主端口策略、系统代理/TUN/DNS/自启开关、新版本提醒 |
| `proxyd mode [rule\|global\|direct]` | 无参查看当前模式；带参切换（持久化） |
| `proxyd refresh` / `proxyd test` | 触发刷新订阅 / 手动测速（后台执行） |
| `proxyd subs list\|add <名> <url>\|del <名>` | 订阅管理（list 含状态列：正常/部分可用/全部失效/无节点/已禁用） |
| `proxyd subs set [--rename 新名] [--url 地址] [--type 类型] [--enable\|--disable] <名>` | 修改订阅；未给出的字段保持原值 |
| `proxyd subs refresh <名>` / `proxyd subs test <名>` | 只刷新 / 只测速单个订阅（同步执行，失败原因直接返回） |
| `proxyd nodes` | 按订阅分组列出节点、端口、延迟/失败原因 |
| `proxyd nodes add <url> [名称]` | 添加手动节点（http(s)/socks5/分享链接） |
| `proxyd nodes del <名称\|下标>` | 删除手动节点 |
| `proxyd rules list\|add "<规则>"\|set <下标> "<规则>"\|move <从> <到>\|del <下标>` | 自定义规则的增改删与优先级调整 |
| `proxyd rule-urls list\|add <名> <url>\|del <名>\|show <名>` | 远程规则源；`show` 打印原始内容（未解析） |
| `proxyd groups list\|add [--type 类型] [--subscription 订阅名] <名> <端口> [节点名...]\|del <名>` | 节点分组 |
| `proxyd groups set [--type 类型] [--subscription 订阅名] [--port 端口] <名> [节点名...]` | 修改分组；未给出的字段保持原值，给出节点名时整体替换成员 |
| `proxyd logs [--tail N] [--level debug\|info\|warning\|error]` | 查看运行中实例的内存日志尾部 |
| `proxyd port-mapping [on\|off\|status]` | 热开关或查看逐节点端口映射；关闭时保留稳定端口分配，不启动对应监听 |
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

`-c` 等 flag 必须放在子命令参数之前（Go flag 解析遇位置参数即停止）；写在后面的 `-c` 会被检测到并直接报错，避免静默操作到默认配置对应的实例。

### 开机自启

`proxyd autostart on` 注册当前二进制（`os.Executable` 绝对路径）+ 当前配置文件为登录自启项，并立即启动一次：

- **macOS**：`~/Library/LaunchAgents/com.proxyd.plist`——RunAtLoad + KeepAlive，`serve -c <配置绝对路径>`，StandardOut/Err 指向 `state-dir/proxyd.log`；launchd 直接托管 serve 前台进程（崩溃自动拉起）
- **Linux**：`~/.config/systemd/user/proxyd.service`（Restart=on-failure）+ `systemctl --user enable --now`；日志走 `journalctl --user -u proxyd`
- **Windows**：注册表 `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` 写 `proxyd start -c <配置>`（登录时派生后台进程后退出，不弹控制台窗口）

`off` 移除对应项（不停止当前运行中的实例）；`status` 检测自启项是否存在。不支持的平台返回明确错误。

### 配置备份与恢复

Web 设置页提供两种导出：默认的“导出（打码）”会隐藏 `secret`、订阅/规则 URL 的用户信息和敏感查询参数，并整体隐藏编码型分享链接，适合排障分享；“完整备份”保留全部凭据，只应存放在可信位置。导入只接受不超过 1 MiB 的 YAML，并复用启动时的迁移、默认值和完整配置校验。前端会先调用预览接口展示新增、删除和变化项；用户确认时携带预览摘要，后端只有在文件内容与预览完全一致时才通过临时文件加 rename 原子替换当前配置文件，避免“预览后文件被替换”的竞态。

导入不会在当前 HTTP 请求中热替换运行配置。备份可能同时改变 `api-listen`、`state-dir`、监听端口和 TUN 权限要求，局部热更新会造成磁盘配置与运行状态不一致，因此接口明确返回 `restart_required: true`，必须重启 proxyd 后整份配置才生效。校验或写入失败时原配置保持不变。

### 版本检查

`check-updates` 默认 `true`。proxyd 启动后在后台请求官方 GitHub 仓库的 latest release，使用八秒 HTTP 超时并把结果缓存在应用内存；Web 的 overview 轮询只读取缓存，不会重复访问 GitHub。发现高于当前构建版本的稳定版时，概览页显示 Release 链接；请求失败、限流或 JSON 异常只记录状态和日志，不影响 API、代理核心或订阅刷新。

当前版本由构建时 `ldflags` 注入。正式 `vX.Y.Z` tag 和 `git describe` 版本可比较；`dev` 或裸提交哈希没有可靠版本基线，会显示“当前构建版本不可比较”并跳过请求，避免字符串比较误报。可在配置写 `check-updates: false`，也可在 Web 设置页关闭。

## 五、日常管理（Web 控制台）

浏览器打开 **http://127.0.0.1:19091/**，React 控制台使用按任务分组的侧边栏信息架构（窄屏自动收起，数据表在手机上转换为带字段标签的纵向条目）：

- **概览**：实时上下行速率条 + 状态摘要（主端口/auto-port/节点/映射统计）+ 入口端口 chip（主端口、auto-port、分组端口、节点映射区间）+ 规则 / 全局 / 直连 模式切换 + 系统代理快捷开关；发现新版时显示 GitHub Release 链接
- **代理节点**：全局搜索并按来源、协议、状态筛选节点；表格展示名称、来源、协议、稳定分配端口、延迟与失败原因，可添加/删除手动节点并设置主端口固定节点
- **订阅管理**：以卡片管理订阅的新增、编辑、启停、单独刷新与测速；禁用订阅时保留本地缓存，重新启用后可继续刷新
- **代理入口**：集中展示主端口、自动选优端口、分组端口与逐节点稳定分配；支持一键复制地址，并独立热开关 `port-mapping`
- **策略分组**：展示健康摘要、复制分组端口、新增/编辑/删除分组；可选择类型（url-test/fallback/load-balance）并按手选节点或订阅来源配置成员
- **访问规则**：搜索、新增、编辑、删除和调整自定义规则顺序；规则 URL 保持只读内容视图并可展开查看可读原文（优先缓存，无缓存时现场拉取；整体 Base64 gfwlist 自动解码）
- **运行日志**：查看进程内约 1000 条环形缓冲日志，支持搜索、暂停刷新、复制和下载
- **活动连接**：仅在页面打开且未暂停时每 2 秒读取 mihomo 连接快照；按域名/IP/进程/出口链搜索，查看入口端口、累计流量与开始时间，并可关闭单条或全部连接
- **系统设置**：使用页内分区导航管理主端口、`main-auto`、`main-node`、节点映射端口区间、`auto-port`、`port-mapping`、DNS 预设、TUN、系统代理、开机自启，以及带差异预览的配置导入
- 页面每 10 秒自动刷新数据；`⌘K` / `Ctrl+K` 命令面板支持跳页、刷新、测速和模式切换

### 自有 API（`api-listen`，默认 19091）

| 接口 | 说明 |
|---|---|
| `GET /api/overview` | 总览：模式、主端口/main_auto、auto-port、订阅聚合（含类型、启用状态、userinfo）、手动节点、端口映射开关与稳定分配、全部节点（含类型/失败原因）、自定义规则、节点分组、TUN 权限、系统代理与开机自启状态 |
| `GET /api/traffic` | 代理 mihomo `/traffic` 流，返回 NDJSON 实时速率；后端自动附加 `secret` 鉴权 |
| `GET /api/connections` | 代理 mihomo `/connections` 快照，返回活动连接、累计上下行和内存占用；后端自动附加 `secret` 鉴权 |
| `DELETE /api/connections/{id}` | 关闭指定活动连接；连接 ID 作为单个安全路径段转发 |
| `DELETE /api/connections` | 关闭全部活动连接 |
| `POST /api/mode` `{"mode":"global"}` | 切换代理模式（rule/global/direct，持久化） |
| `POST /api/refresh` | 触发一轮完整刷新：拉订阅 + 规则源 + 测速（异步，返回 202） |
| `POST /api/test` | 手动测速：只对现有节点做延迟/可用性检测，不拉订阅（异步，返回 202） |
| `POST /api/subscriptions` `{"url":"..."}` | 添加订阅（name 可选，默认按域名命名） |
| `PUT /api/subscriptions/{name}` | 编辑订阅名称、URL、格式或启用状态；改名时同步修正引用该订阅的策略分组 |
| `DELETE /api/subscriptions/{name}` | 删除订阅 |
| `POST /api/subscriptions/{name}/refresh` | 只刷新该订阅：重新拉取 + 只检测其节点 + 热更新（同步，最长 3 分钟，失败直接返回原因） |
| `POST /api/subscriptions/{name}/test` | 只对该订阅的现有节点测速（同步，不拉订阅） |
| `GET /api/manual-nodes` | 列出手动节点（index/url/解析出的名称） |
| `POST /api/manual-nodes` `{"url":"http://user:pass@host:8080","name":"可选"}` | 添加手动节点（解析校验 + 持久化 + 后台刷新） |
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
| `GET /api/groups` | 列出节点分组 |
| `GET /api/logs?tail=200&level=error` | 返回内存日志尾部；`level` 可选 `debug/info/warning/error` |
| `POST /api/groups` `{"name":"hk","port":43000,"type":"fallback","subscription":"airport-a"}` | 新增节点分组；`type` 支持 `url-test/fallback/load-balance`，成员可来自 `nodes` 或 `subscription` |
| `PUT /api/groups/{name}` | 编辑分组端口、类型与成员来源；为保护 `dialer-proxy` 引用，暂不支持在线改名 |
| `DELETE /api/groups/{name}` | 删除节点分组 |
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

使用 Web 设置页或 `proxyd tun [-c 配置] on|off|status` 热切换。开启流程先检查权限，再让 mihomo 热更新，并读取实际 listener 状态二次确认；生成、应用或实际启用失败会恢复旧 TUN 配置。配置文件启动时已经是 `enable: true` 但权限不足，proxyd 会在启动 API 和修改路由之前退出并打印修复指引。

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

**规则 URL 导入**（`rule-urls`）：从远程 URL 导入规则（如 gfwlist），跟随订阅刷新一起拉取（`refresh-interval` 周期），内容缓存到 `state-dir/cache/`，**不会写回配置文件**（config 只存 URL）；拉取失败降级用缓存，都没有则跳过该源打日志。原始内容可在 Web 控制台「查看内容」或 `proxyd rule-urls show <名>` 查看（优先读缓存，无缓存时现场拉取一次并写缓存）。按内容自动识别两种格式：

- mihomo 规则文本：每行 `类型,内容,策略`（≥3 段）原样采用，支持 `#`/`//` 注释与空行
- gfwlist / AutoProxy（base64 编码）：`||domain` → `DOMAIN-SUFFIX,domain,PROXY`；`@@||domain` → `DOMAIN-SUFFIX,domain,DIRECT`；`!` 注释、`[AutoProxy]` 段头、含 `*`/`/` 的复杂条目跳过

合并顺序：custom-rules 最前 → 规则 URL 导入规则 → 内置规则。全部来源合并去重后上限 10000 条，超出截断打日志。

**节点分组端口**（`groups`）：把若干节点聚合成一个 mihomo proxy-group 并绑定到指定端口，该端口固定走该组；`type` 可选 `url-test`（自动测速择优）、`fallback`（按顺序故障转移）、`load-balance`（负载均衡）。旧配置没有 `type` 时默认迁移为 `url-test`，新 UI 默认推荐 `fallback`：

```yaml
groups:
  - name: hk                # 组名（不能与节点名或 AUTO/PROXY/DIRECT 等保留名冲突）
    port: 43000             # 不能与主端口、api 端口、port-range 区间、auto-port 或其他分组冲突
    type: fallback          # url-test | fallback | load-balance；旧配置缺省为 url-test
    nodes: ["香港 01", "香港 02"]  # 节点名列表，与当前可用节点取交集

  - name: airport-a-auto
    port: 43001
    type: url-test
    subscription: airport-a # 成员动态取该订阅当前可用节点，刷新后自动跟随
```

成员来源二选一：配置 `nodes` 时取节点名与当前可用节点的交集；配置 `subscription` 时取该订阅当前可用节点，`manual` 表示手动节点来源。刷新后节点集合变化时分组自动收缩，成员为空则该组本轮跳过（日志有提示）。分组与按节点映射端口完全并存、互不影响。

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

**geo 数据**：GEOSITE/GEOIP 规则需要数据文件，proxyd 默认从 jsDelivr 镜像（Loyalsoldier 规则仓库）自动下载，无需 GitHub 直连。下载失败时自动降级为本轮不含 GEO 规则运行（日志有提示），下一轮刷新自动重试。可用 `geox-url` 配置换成自己的镜像。

## 七、刷新与检测机制

| 机制 | 默认周期 | 配置项 | 行为 |
|---|---|---|---|
| 订阅刷新 | 1 天 | `refresh-interval` | 重新拉取所有订阅与规则源（rule-urls）→ 测速（可访问性检测）→ 重新分配端口 → 热更新核心 |
| 健康检测 | 5 分钟 | `health-interval` | 复用现有节点列表测速 → 死节点下端口、恢复的节点补位 → 热更新核心 |
| 单次检测超时 | 5 秒 | `health-timeout` | 经节点出口对 `health-url` 发 HTTP 探测 |
| 探测地址 | gstatic 204 | `health-url` | 可换，如 `http://cp.cloudflare.com/generate_204` |

**端口映射稳定性**：映射快照持久化在 `state-dir/mapping.json`——同一节点在刷新/重启后尽量保持原端口；新节点按延迟从低到高填空闲端口；可用节点多于端口容量时按延迟截断（日志会提示）。

**节点快照（nodes.json）**：每轮刷新后，把合并后的全量节点（完整 proxy 配置、来源订阅、最近测速结果）持久化到 `state-dir/nodes.json`。下次启动时**立即加载该快照生成配置提供服务**，不必等首次订阅刷新完成；随后刷新成功再覆盖。刷新失败/无网时快照保持可用。快照带格式版本号，解析失败或版本不兼容仅打日志丢弃，不影响启动。

**失败兜底**：订阅拉取失败时自动使用本地缓存（`state-dir/cache/`），网络抖动不会清空节点；全部订阅失败或全部节点死亡时保持现有配置不动，下一轮再试。

## 八、存储布局

**配置文件**（`~/.config/proxyd/config.yaml`，`-c` 可改）——用户配置，Web/CLI 的变更自动落盘到这里：

| 内容 | 字段 |
|---|---|
| 订阅列表 | `subscriptions` |
| 手动节点（自有代理 URL/分享链接） | `manual-nodes` |
| 端口区间/主端口/auto-port/分组端口 | `port-range` / `mixed-port` / `main-auto` / `main-node` / `auto-port` / `groups` |
| 代理模式、自定义规则、规则源 URL | `mode` / `custom-rules` / `rule-urls`（只存 URL，不存规则内容） |
| 系统代理开关、节点正则过滤、周期等 | `system-proxy` / `include` / `exclude` / `refresh-interval` / ... |

**状态目录**（`state-dir`，默认 `~/.local/state/proxyd`）——运行时状态，删了只会丢缓存/快照，不影响配置：

| 文件 | 内容 |
|---|---|
| `nodes.json` | 最近一次合并后的节点快照（完整 proxy 配置 + 来源 + 测速结果），启动时立即恢复 |
| `mapping.json` | 节点 → 端口的稳定映射快照 |
| `cache/<订阅名>.cache` | 各订阅的原始响应缓存（拉取失败时降级用） |
| `cache/rules-<名>.cache` | 各规则源的原始内容缓存 |
| `proxyd.pid` | 运行中实例的 pid（serve 启动时登记、退出时清理；供 stop/status/防重复启动） |
| `proxyd.log` | 后台模式（start）与开机自启的日志文件 |
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

listen: 127.0.0.1         # 映射端口监听地址；改成 0.0.0.0 可共享给局域网
port-range: [42000, 42100]
mixed-port: 41999         # 主端口（规则模式），Web/CLI 可在线修改
# main-auto: false        # true 时主端口跳过规则、固定走最优节点
# main-node: ""           # 主端口固定节点（节点 Key，见 overview/API）；空=跟随规则；
                          # main-auto 开启时被忽略；节点失效自动回退规则模式
# auto-port: 41998        # 自动选优端口（固定走延迟最低节点），0=关闭
# system-proxy: false     # serve 启动时把系统代理指向主端口
refresh-interval: 24h
health-interval: 5m
health-url: http://www.gstatic.com/generate_204
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
groups:                     # 可选，节点分组端口（支持 url-test/fallback/load-balance）
  - name: hk
    port: 43000
    type: fallback
    nodes: ["香港 01"]
  - name: airport-a-auto
    port: 43001
    type: url-test
    subscription: airport-a
external-controller: 127.0.0.1:19090   # mihomo API
api-listen: 127.0.0.1:19091            # Web 控制台
# secret: ...             # mihomo API 鉴权
state-dir: ~/.local/state/proxyd       # 状态目录（快照/缓存/pid/日志/geo），见「八、存储布局」
```

## 十、常见问题

- **启动后没有映射端口**：看日志——订阅拉取失败会用缓存与 nodes.json 快照；全部节点测速失败检查 `health-url` 是否可达。
- **geo 下载慢/失败**：已内置镜像，仍失败可在配置 `geox-url` 换源；失败不影响代理本体（自动降级）。
- **改了配置文件什么时候生效**：模式经 Web/API 切换即时生效；其他改动重启进程生效（`mapping.json` 保证端口不漂）。
- **节点数多于端口数**：按延迟保留最快的一批，其余节点仍在主端口的 PROXY 选择组里可用。
- **端口被占**：换 `port-range` / `mixed-port` / `auto-port` / `api-listen` / `external-controller`（分组端口同理）。
- **异常退出后系统代理没恢复**：`proxyd sysproxy off` 手动关闭（正常退出会自动恢复）。
- **proxyd stop 提示未在运行但进程还在**：异常退出可能留下过期 pid 文件，stop 会自动清理；确认进程残留时手动 kill。
- **重启后节点还在吗**：在。配置里有订阅/手动节点；`state-dir/nodes.json` 快照让启动即刻可用，`mapping.json` 保证端口不漂。
