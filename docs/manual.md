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
- 主端口是普通 mixed 端口，走完整的规则匹配，行为与 Clash 一致。

## 二、支持的订阅格式与协议

**订阅格式**（`type` 默认 `auto` 自动嗅探，也可显式指定）：

| 格式 | 说明 |
|---|---|
| Clash YAML | 含顶层 `proxies:` 的标准 Clash 订阅，字段原样透传给 mihomo |
| 分享链接 | base64 编码的多行链接列表（v2ray 风格），也兼容明文 |

**节点协议**：

- Clash YAML 订阅：支持 mihomo 全部出站协议——ss、ssr、vmess、vless、trojan、hysteria2、tuic、anytls、snell、shadowtls、wireguard、socks5、http、direct 等
- 分享链接订阅：ss（SIP002 及旧格式）、ssr、vmess、vless（含 reality）、trojan、hysteria2 / hy2、tuic

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
| `proxyd serve` | 读默认配置 `~/.config/proxyd/config.yaml` 常驻运行 |
| `proxyd serve <url>...` | 快捷启动；新订阅地址自动合并保存进配置文件 |
| `proxyd serve -c xxx.yaml` | 指定配置文件 |
| `proxyd serve -range A-B <url>` | 指定映射端口区间 |
| `proxyd <url>` | `serve <url>` 的快捷形式 |
| `proxyd check ...` | 一次性自检：打印节点/端口映射表，参数同 serve |
| `proxyd sysproxy on\|off\|status` | 开关/查看系统代理（指向主端口） |

### 常驻运行

- **macOS (launchd)**：`~/Library/LaunchAgents/com.proxyd.plist` 的 `ProgramArguments` 填 `/usr/local/bin/proxyd serve`，加 `RunAtLoad` + `KeepAlive`。
- **Linux (systemd)**：

  ```ini
  [Unit]
  Description=proxyd
  After=network-online.target

  [Service]
  ExecStart=/usr/local/bin/proxyd serve
  Restart=always

  [Install]
  WantedBy=multi-user.target
  ```

## 五、日常管理（Web 控制台）

浏览器打开 **http://127.0.0.1:19091/**，左侧菜单切换六个面板：

- **概览**：主端口/自动选优端口/节点与映射统计；规则 / 全局 / 直连 模式切换（即时生效并写入配置文件）；手动测速与刷新订阅按钮（带 loading 状态，自动轮询结果）
- **订阅与节点**：添加/删除订阅（增删自动写入配置文件并触发后台刷新；不允许删除最后一个订阅）；每个订阅可展开/折叠查看其节点（名称、类型、端口、延迟、状态与失败原因，失效节点置灰但可见）
- **端口映射**：端口 → 节点 → 延迟 → 状态；点击端口号复制代理地址（`http://127.0.0.1:端口`）
- **自定义规则**：追加式规则逐条增删（前置到内置规则之前，格式 `类型,内容,策略`，策略可填 DIRECT / REJECT / 节点名 / 分组名）；规则 URL 管理（添加/删除、显示条目数与拉取状态）
- **节点分组**：勾选节点（失效节点也可勾选，恢复后自动加入）+ 填名称/端口即可添加分组；该端口固定走组内 url-test 自动选优出口
- **设置**：节点映射端口区间（保存后立即重新分配并热更新，不重新测速）；自动选优端口开关与端口号；系统代理开关
- 页面每 10 秒自动刷新数据

### 自有 API（`api-listen`，默认 19091）

| 接口 | 说明 |
|---|---|
| `GET /api/overview` | 总览：模式、端口、订阅聚合、端口映射、全部节点（含类型/失败原因）、自定义规则、节点分组 |
| `POST /api/mode` `{"mode":"global"}` | 切换代理模式（rule/global/direct，持久化） |
| `POST /api/refresh` | 触发一轮完整刷新：拉订阅 + 规则源 + 测速（异步，返回 202） |
| `POST /api/test` | 手动测速：只对现有节点做延迟/可用性检测，不拉订阅（异步，返回 202） |
| `POST /api/subscriptions` `{"url":"..."}` | 添加订阅（name 可选，默认按域名命名） |
| `DELETE /api/subscriptions/{name}` | 删除订阅 |
| `POST /api/port-range` `{"range":"43000-43200"}` | 修改节点映射端口区间（同步：重新分配端口 + 热更新，不重新测速） |
| `POST /api/auto-port` `{"port":41998}` | 开启自动选优端口；`{"port":0}` 关闭（持久化 + 热更新） |
| `POST /api/system-proxy` `{"enabled":true}` | 开关系统代理（指向主端口，持久化） |
| `GET /api/rules` | 列出自定义规则 |
| `POST /api/rules` `{"rule":"DOMAIN-SUFFIX,example.com,DIRECT"}` | 追加自定义规则（前置到内置规则之前） |
| `DELETE /api/rules/{index}` | 按下标删除自定义规则 |
| `GET /api/rule-urls` | 列出规则源（含条目数与拉取状态） |
| `POST /api/rule-urls` `{"name":"gfwlist","url":"https://..."}` | 新增规则源（持久化 + 立即拉取 + 热更新） |
| `DELETE /api/rule-urls/{name}` | 删除规则源 |
| `GET /api/groups` | 列出节点分组 |
| `POST /api/groups` `{"name":"hk","port":43000,"nodes":["香港 01"]}` | 新增节点分组（组内 url-test 自动选优） |
| `DELETE /api/groups/{name}` | 删除节点分组 |
| `GET /ports` | 端口映射表（兼容旧接口） |

### mihomo API（`external-controller`，默认 19090）

兼容 metacubexd / yacd 面板：`/proxies`（节点与测速）、`/configs`（模式/日志等级）、`/connections`、`/rules` 等。在面板设置里填 `http://127.0.0.1:19090` 即可接入（设置了 `secret` 时面板里也要填）。

### 系统代理

把系统 HTTP/HTTPS/SOCKS 代理指向主端口（`127.0.0.1:<mixed-port>`）：

- **CLI**：`proxyd sysproxy on|off|status`（`-c` 指定配置文件以读取主端口）
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

**自动选优端口**（`auto-port`，默认关闭）：开启后额外监听一个独立 mixed 端口，固定走 `AUTO` url-test 组——全部当前可用节点中自动选延迟最低者（探测地址用 `health-url`，间隔 300s，容差 50ms）；无可用节点时本轮跳过该 listener（日志有提示）。与主端口的规则模式完全独立。不能与主端口、api 端口、节点区间、分组端口冲突。旧版 `mode: auto` 配置加载时自动迁移为 `mode: rule` + `auto-port: 41998`（日志有提示）。

**自定义规则**（`custom-rules`，Web/API/配置文件均可管理）：追加式，生成 mihomo 配置时**前置到内置 `rules` 之前**——规则匹配按顺序生效，追加在 GEOSITE/GEOIP/MATCH 之后永远不会命中，所以自定义规则必须前置；内置规则原样保留在后面。每条格式 `类型,内容,策略`（至少 3 段），支持 DOMAIN / DOMAIN-SUFFIX / DOMAIN-KEYWORD / IP-CIDR / GEOSITE / GEOIP 等 mihomo 语法，策略可填 DIRECT / REJECT / 节点名 / 分组名；非法规则在 API 层直接报错（含 mihomo 自检失败的原因），不会静默生效。

**规则 URL 导入**（`rule-urls`）：从远程 URL 导入规则（如 gfwlist），跟随订阅刷新一起拉取（`refresh-interval` 周期），内容缓存到 `state-dir/cache/`，**不会写回配置文件**（config 只存 URL）；拉取失败降级用缓存，都没有则跳过该源打日志。按内容自动识别两种格式：

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

**失败兜底**：订阅拉取失败时自动使用本地缓存（`state-dir/cache/`），网络抖动不会清空节点；全部订阅失败或全部节点死亡时保持现有配置不动，下一轮再试。

## 八、配置文件参考

默认路径 `~/.config/proxyd/config.yaml`（`-c` 可指定其他路径）。完整示例见 `configs/config.example.yaml`，关键项：

```yaml
subscriptions:            # 订阅列表，CLI/Web 添加的会自动写在这里
  - name: airport-a
    url: https://...
    type: auto            # auto | clash | share

listen: 127.0.0.1         # 映射端口监听地址；改成 0.0.0.0 可共享给局域网
port-range: [42000, 42100]
mixed-port: 41999         # 主端口（规则模式）
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
state-dir: ~/.local/state/proxyd       # 映射快照、订阅缓存、geo 数据
```

## 九、常见问题

- **启动后没有映射端口**：看日志——订阅拉取失败会用缓存；全部节点测速失败检查 `health-url` 是否可达。
- **geo 下载慢/失败**：已内置镜像，仍失败可在配置 `geox-url` 换源；失败不影响代理本体（自动降级）。
- **改了配置文件什么时候生效**：模式经 Web/API 切换即时生效；其他改动重启进程生效（`mapping.json` 保证端口不漂）。
- **节点数多于端口数**：按延迟保留最快的一批，其余节点仍在主端口的 PROXY 选择组里可用。
- **端口被占**：换 `port-range` / `mixed-port` / `auto-port` / `api-listen` / `external-controller`（分组端口同理）。
- **异常退出后系统代理没恢复**：`proxyd sysproxy off` 手动关闭（正常退出会自动恢复）。
