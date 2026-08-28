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
    http://127.0.0.1:19091/   Web 控制台 + proxyd 自有 API（订阅管理、映射表、模式切换）
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
- `exclude` 配置项按节点名正则过滤机场信息节点（如"到期""剩余流量"），默认建议：`到期|剩余流量|套餐|官网|订阅`

## 三、入门（快速开始）

### 安装

从 Release 下载对应平台压缩包解压，或源码编译：

```sh
make build      # 产出 bin/proxyd（单文件，无外部依赖）
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
| `proxyd status` | 运行中显示 pid、主端口/映射区间/auto-port、Web 地址与 API 健康状态 |
| `proxyd check ...` | 一次性自检：打印节点/端口映射表，参数同 serve |
| `proxyd sysproxy [-c 配置] on\|off\|status` | 开关/查看系统代理（指向主端口；flag 需放在操作前） |
| `proxyd autostart [-c 配置] on\|off\|status` | 开关/查看开机自启（flag 需放在操作前） |

### 本地管理命令（CLI ↔ Web 对齐）

以下命令全部作为**本地 API 客户端**实现：读取配置文件拿 `api-listen` 地址，HTTP 调用运行中实例；实例未运行时提示「请先 proxyd start」，不产生离线改配置的旁路。错误信息原样透传 API 报错。

| 命令 | 说明 |
|---|---|
| `proxyd mode [rule\|global\|direct]` | 无参查看当前模式；带参切换（持久化） |
| `proxyd refresh` / `proxyd test` | 触发刷新订阅 / 手动测速（后台执行） |
| `proxyd subs list\|add <名> <url>\|del <名>` | 订阅管理 |
| `proxyd nodes` | 按订阅分组列出节点、端口、延迟/失败原因 |
| `proxyd nodes add <url> [名称]` | 添加手动节点（http(s)/socks5/分享链接） |
| `proxyd nodes del <名称\|下标>` | 删除手动节点 |
| `proxyd rules list\|add "<规则>"\|del <下标>` | 自定义规则 |
| `proxyd rule-urls list\|add <名> <url>\|del <名>\|show <名>` | 远程规则源；`show` 打印原始内容（未解析） |
| `proxyd groups list\|add <名> <端口> <节点名...>\|del <名>` | 节点分组 |
| `proxyd port-range <起-止>` | 修改节点映射端口区间 |
| `proxyd auto-port <端口\|off>` | 设置/关闭自动选优端口；无参查看 |
| `proxyd main-auto [on\|off]` | 开关「主端口使用最优节点」（跳过规则）；无参查看 |
| `proxyd main-node [节点key\|off]` | 设置主端口固定节点（跳过规则、直达该节点）；无参查看，`off` 清除 |
| `proxyd main-port <端口>` | 修改主端口（热更新；系统代理开启时自动重绑）；无参查看 |

### 开机自启

`proxyd autostart on` 注册当前二进制（`os.Executable` 绝对路径）+ 当前配置文件为登录自启项，并立即启动一次：

- **macOS**：`~/Library/LaunchAgents/com.proxyd.plist`——RunAtLoad + KeepAlive，`serve -c <配置绝对路径>`，StandardOut/Err 指向 `state-dir/proxyd.log`；launchd 直接托管 serve 前台进程（崩溃自动拉起）
- **Linux**：`~/.config/systemd/user/proxyd.service`（Restart=on-failure）+ `systemctl --user enable --now`；日志走 `journalctl --user -u proxyd`
- **Windows**：注册表 `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` 写 `proxyd start -c <配置>`（登录时派生后台进程后退出，不弹控制台窗口）

`off` 移除对应项（不停止当前运行中的实例）；`status` 检测自启项是否存在。不支持的平台返回明确错误。

## 五、日常管理（Web 控制台）

浏览器打开 **http://127.0.0.1:19091/**，顶部导航切换五个面板（扁平化设计，内容居中限宽，窄屏自动换行、表格横向可滚动）：

- **概览**：状态摘要（主端口/auto-port/节点/映射统计）+ 已映射端口 chip 网格（节点映射端口折叠为区间如 `42000-42051`，主端口/优选/分组端口逐个展示，按端口升序）+ 全部设置——规则 / 全局 / 直连 模式切换（即时生效并写入配置文件）；主端口修改（热更新，系统代理开启时自动重绑）；「主端口使用最优节点」开关；「固定节点」下拉（主端口直达指定节点，main-auto 开启时禁用；所选节点失效时提示已回退规则模式）；节点映射端口区间（保存后立即重新分配并热更新，不重新测速）；自动选优端口开关与端口号；系统代理开关；开机自启开关（布尔项均为 toggle 开关样式）
- **订阅与节点**：面板顶部是「手动测速」「刷新订阅」操作区（带 loading 状态，自动轮询结果）；其下是「手动节点」管理区（添加/删除自有代理节点并展示健康状态）；再往下是一排订阅快捷导航标签（名称 + 可用/总数，含「手动节点」一项），点击标签平滑滚动定位并展开对应订阅（短暂高亮，「全部」恢复）；再往下可添加/删除订阅（增删自动写入配置文件并触发后台刷新；不允许删除最后一个来源）；每个订阅可展开/折叠查看其节点（名称、类型、端口、延迟按快慢着色、状态与失败原因，失效节点置灰但可见）；每个订阅（含手动节点表）有独立排序控件（默认/延迟升序/延迟降序，失效/未测速排最后，仅显示顺序、内存态不持久化）；节点表「操作」列可一键「设为主端口」（等价 `main-node`，失效或未分配端口的节点按钮置灰，当前主端口行显示标识）
- **端口映射**：端口 → 节点 → 延迟 → 状态；点击端口号复制代理地址（`http://127.0.0.1:端口`）；左上角排序控件可在默认/按延迟排序间切换（仅显示顺序，内存态）
- **自定义规则**：追加式规则逐条增删（前置到内置规则之前，格式 `类型,内容,策略`，策略可填 DIRECT / REJECT / 节点名 / 分组名）；规则 URL 管理（添加/删除、显示条目数与拉取状态；「查看内容」可折叠查看原始文本，优先读本地缓存、无缓存时现场拉取一次）
- **节点分组**：勾选节点（失效节点也可勾选，恢复后自动加入；已被其他分组占用的节点右侧显示组名 tag）+ 填名称/端口即可添加分组；待选列表可用上方排序控件按延迟排序（仅显示顺序，内存态）；该端口固定走组内 url-test 自动选优出口
- 页面每 10 秒自动刷新数据

### 自有 API（`api-listen`，默认 19091）

| 接口 | 说明 |
|---|---|
| `GET /api/overview` | 总览：模式、主端口/main_auto、auto-port、订阅聚合、手动节点、端口映射、全部节点（含类型/失败原因）、自定义规则、节点分组、系统代理与开机自启状态 |
| `POST /api/mode` `{"mode":"global"}` | 切换代理模式（rule/global/direct，持久化） |
| `POST /api/refresh` | 触发一轮完整刷新：拉订阅 + 规则源 + 测速（异步，返回 202） |
| `POST /api/test` | 手动测速：只对现有节点做延迟/可用性检测，不拉订阅（异步，返回 202） |
| `POST /api/subscriptions` `{"url":"..."}` | 添加订阅（name 可选，默认按域名命名） |
| `DELETE /api/subscriptions/{name}` | 删除订阅 |
| `GET /api/manual-nodes` | 列出手动节点（index/url/解析出的名称） |
| `POST /api/manual-nodes` `{"url":"http://user:pass@host:8080","name":"可选"}` | 添加手动节点（解析校验 + 持久化 + 后台刷新） |
| `DELETE /api/manual-nodes/{index}` | 按下标删除手动节点 |
| `POST /api/port-range` `{"range":"43000-43200"}` | 修改节点映射端口区间（同步：重新分配端口 + 热更新，不重新测速） |
| `POST /api/auto-port` `{"port":41998}` | 开启自动选优端口；`{"port":0}` 关闭（持久化 + 热更新） |
| `POST /api/main-auto` `{"enabled":true}` | 开关「主端口使用最优节点」（主端口跳过规则、固定走 AUTO 组；持久化 + 热更新） |
| `POST /api/main-node` `{"node":"<节点key>"}` | 设置主端口固定节点（跳过规则、直达该节点；空串清除；持久化 + 热更新） |
| `POST /api/main-port` `{"port":42999}` | 修改主端口（校验 1-65535 且不与 api 端口/节点区间/分组/auto-port 冲突；持久化 + 热更新；系统代理开启时自动重绑） |
| `POST /api/system-proxy` `{"enabled":true}` | 开关系统代理（指向主端口，持久化） |
| `POST /api/autostart` `{"enabled":true}` | 注册/移除开机自启项（OS 级状态，不写配置文件；overview 实时反映） |
| `GET /api/rules` | 列出自定义规则 |
| `POST /api/rules` `{"rule":"DOMAIN-SUFFIX,example.com,DIRECT"}` | 追加自定义规则（前置到内置规则之前） |
| `DELETE /api/rules/{index}` | 按下标删除自定义规则 |
| `GET /api/rule-urls` | 列出规则源（含条目数与拉取状态） |
| `POST /api/rule-urls` `{"name":"gfwlist","url":"https://..."}` | 新增规则源（持久化 + 立即拉取 + 热更新） |
| `DELETE /api/rule-urls/{name}` | 删除规则源 |
| `GET /api/rule-urls/{name}/content` | 规则源原始文本（text/plain；优先缓存，无缓存现场拉取一次并写缓存；源不存在/拉取失败返回 404） |
| `GET /api/groups` | 列出节点分组 |
| `POST /api/groups` `{"name":"hk","port":43000,"nodes":["香港 01"]}` | 新增节点分组（组内 url-test 自动选优） |
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
2. **固定节点**（`main-node`，默认空）：主端口切换为同端口的 mixed listener，`proxy` 固定指向指定节点——跳过规则、直达该节点。配置里存节点 **Key**（协议+地址+凭据），重命名/重名时仍稳定；Web 概览页主端口卡片的「固定节点」下拉（列出全部当前可用节点，格式 `节点名 (端口)`）或 `proxyd main-node <节点key|off>` 设置，选择即保存。节点当前不可用（失效/订阅刷新后消失）时本轮自动回退规则模式并打日志，**配置保留不删**，节点恢复后自动再生效（Web 上下拉旁会提示"当前节点不可用，已回退规则模式"）。
3. **自动优选**（`main-auto`，默认关闭）：主端口 listener 固定走 `AUTO` url-test 组——全部可用节点中自动选延迟最低者。与独立的 auto-port 可并存（共用 AUTO 组、各占端口）。无可用节点时本轮跳过该设置（主端口回退规则模式，日志有提示）。

优先级：**`main-auto` 开启时 `main-node` 被忽略**（auto 优先，日志提示一句）。从规则模式切换到 listener 形态（开 main-auto / 设 main-node / 失效节点恢复后自动再生效）时内部统一做两阶段热更新（先短暂关闭主端口入口再切换形态），避免 mihomo 先监听后释放导致的同端口 bind 冲突；listener 之间互切（main-auto ↔ main-node）同名 `L<port>` 仅换 proxy 目标，由 mihomo 按 关闭→监听 顺序安全处理。节点映射端口、分组端口、auto-port 完全不受影响。

**主端口在线修改**（Web 概览页 / `POST /api/main-port` / `proxyd main-port <端口>`）：校验 1-65535 且不与 api 端口、节点区间、分组端口、auto-port 冲突；保存后持久化 + 热更新；系统代理当前已开启时自动重新绑定到新端口。

**自动选优端口**（`auto-port`，默认关闭）：开启后额外监听一个独立 mixed 端口，固定走 `AUTO` url-test 组——全部当前可用节点中自动选延迟最低者（探测地址用 `health-url`，间隔 300s，容差 50ms）；无可用节点时本轮跳过该 listener（日志有提示）。与主端口的规则模式完全独立。不能与主端口、api 端口、节点区间、分组端口冲突。旧版 `mode: auto` 配置加载时自动迁移为 `mode: rule` + `auto-port: 41998`（日志有提示）。

**自定义规则**（`custom-rules`，Web/API/配置文件均可管理）：追加式，生成 mihomo 配置时**前置到内置 `rules` 之前**——规则匹配按顺序生效，追加在 GEOSITE/GEOIP/MATCH 之后永远不会命中，所以自定义规则必须前置；内置规则原样保留在后面。每条格式 `类型,内容,策略`（至少 3 段），支持 DOMAIN / DOMAIN-SUFFIX / DOMAIN-KEYWORD / IP-CIDR / GEOSITE / GEOIP 等 mihomo 语法，策略可填 DIRECT / REJECT / 节点名 / 分组名；非法规则在 API 层直接报错（含 mihomo 自检失败的原因），不会静默生效。

**规则 URL 导入**（`rule-urls`）：从远程 URL 导入规则（如 gfwlist），跟随订阅刷新一起拉取（`refresh-interval` 周期），内容缓存到 `state-dir/cache/`，**不会写回配置文件**（config 只存 URL）；拉取失败降级用缓存，都没有则跳过该源打日志。原始内容可在 Web 控制台「查看内容」或 `proxyd rule-urls show <名>` 查看（优先读缓存，无缓存时现场拉取一次并写缓存）。按内容自动识别两种格式：

- mihomo 规则文本：每行 `类型,内容,策略`（≥3 段）原样采用，支持 `#`/`//` 注释与空行
- gfwlist / AutoProxy（base64 编码）：`||domain` → `DOMAIN-SUFFIX,domain,PROXY`；`@@||domain` → `DOMAIN-SUFFIX,domain,DIRECT`；`!` 注释、`[AutoProxy]` 段头、含 `*`/`/` 的复杂条目跳过

合并顺序：custom-rules 最前 → 规则 URL 导入规则 → 内置规则。全部来源合并去重后上限 10000 条，超出截断打日志。

**节点分组端口**（`groups`）：把若干节点聚合成一个 url-test 组并绑定到指定端口，该端口固定走组内延迟最低的成员：

```yaml
groups:
  - name: hk                # 组名（不能与节点名或 AUTO/PROXY/DIRECT 等保留名冲突）
    port: 43000             # 不能与主端口、api 端口、port-range 区间、auto-port 或其他分组冲突
    nodes: ["香港 01", "香港 02"]  # 节点名列表，与当前可用节点取交集
```

成员取 `nodes` 与当前可用节点的交集——刷新后节点集合变化时分组自动收缩，交集为空则该组本轮跳过（日志有提示）。分组与按节点映射端口完全并存、互不影响。

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
| 系统代理开关、排除正则、周期等 | `system-proxy` / `exclude` / `refresh-interval` / ... |

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
exclude: "到期|剩余流量"

mode: rule                  # rule | global | direct
rules: [...]
custom-rules:               # 可选，追加式自定义规则，前置到 rules 之前
  - DOMAIN-SUFFIX,example.com,DIRECT
rule-urls:                  # 可选，远程规则源（mihomo 文本 / gfwlist），内容不写回配置
  - name: gfwlist
    url: https://...
groups:                     # 可选，节点分组端口（组内 url-test 自动选优）
  - name: hk
    port: 43000
    nodes: ["香港 01"]
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
