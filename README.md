# proxyd

多节点端口映射代理工具。与 Clash / mihomo 等客户端"一次只用一个节点"不同，proxyd 把订阅里的**每个可用节点各映射到一个本地端口**（HTTP + SOCKS5 混合端口，固定出口），让所有节点同时可用；另保留一个走常规规则模式（rule/global/direct）的主端口。

核心基于 [mihomo](https://github.com/MetaCubeX/mihomo)（Clash.Meta）以 Go 库方式内嵌运行，单二进制交付，**无需单独安装 mihomo**。

📖 完整文档：[docs/manual.md](docs/manual.md)（架构、协议、规则、刷新机制、配置参考、FAQ）

## 功能

- 多个订阅源（Clash YAML 和 base64 分享链接两种格式，自动嗅探）
- 可用节点自动映射到指定端口区间，映射关系稳定（重启/刷新后同一节点尽量保持原端口）
- 定时刷新订阅（`refresh-interval`）+ 定时健康检测（`health-interval`），死节点自动下端口、新节点自动补位
- 主端口完整支持 Clash 规则与三种代理模式（rule / global / direct），可热切换
- 可选「自动选优端口」（`auto-port`）：独立端口固定走全部可用节点中延迟最低者（url-test 组）
- 自定义规则（追加式，前置到内置规则之前）+ 规则 URL 导入（mihomo 规则文本 / gfwlist）+ 节点分组端口（一组节点 → 指定端口，组内自动选优）
- 系统代理开关：CLI `proxyd sysproxy on|off|status` 或 Web 设置页，把系统代理指向主端口（macOS networksetup / Linux gsettings / Windows 注册表）
- REST API 与 Web 控制台：
  - `http://127.0.0.1:19091/` 内嵌 Web 控制台（概览、按订阅查看节点、端口映射、规则/分组/端口区间/系统代理管理）
  - `http://127.0.0.1:19090` mihomo external-controller（兼容 metacubexd / yacd 面板）
- 订阅拉取失败时自动降级到本地缓存
- 单二进制，跨平台（macOS / Linux / Windows，amd64 / arm64）

## 安装

从 Release 下载对应平台压缩包，或自行编译：

```sh
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

**订阅地址会自动保存到默认配置文件 `~/.config/proxyd/config.yaml`**，之后直接 `proxyd serve`（不带任何参数）即可。也可以在 Web 控制台里随时增删订阅（同样自动落盘）。

不给端口区间时默认用 `42000-42100`（主端口 `41999`，规则模式入口），内置默认规则（私网/国内直连，其余走代理）。

启动后打开 **Web 控制台 `http://127.0.0.1:19091/`**：左侧菜单切换 概览 / 订阅与节点 / 端口映射 / 自定义规则 / 节点分组 / 设置，可增删订阅、按订阅展开查看节点（含失败原因）、一键切换 规则/全局/直连 模式、管理自定义规则与规则 URL、配置节点分组、调整端口区间与自动选优端口、开关系统代理。

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
- `port-range`：节点映射端口区间；`mixed-port`：主端口（默认区间前一位）
- `auto-port`：自动选优端口（0=关闭），固定走全部可用节点中延迟最低者，与主端口模式互不影响
- `system-proxy`：true 时 serve 启动即把系统代理指向主端口，退出自动恢复
- `mode`：`rule | global | direct`
- `custom-rules`：追加式自定义规则，生成时前置到 `rules` 之前（如 `DOMAIN-SUFFIX,example.com,DIRECT`，策略可填节点名/分组名）
- `rule-urls`：远程规则源（`name`/`url`），支持 mihomo 规则文本与 gfwlist（base64），内容跟随订阅刷新拉取并缓存到 `state-dir`，不写回配置文件
- `groups`：节点分组端口——`name`/`port`/`nodes`，组内 url-test 自动选优，端口独占一个 mixed listener
- `exclude`：按节点名正则过滤机场信息节点（"到期""剩余流量"之类）
- `rules` / `rule-providers` / `dns`：Clash 语义原样透传 mihomo
- `state-dir`：端口映射快照与订阅缓存，默认 `~/.local/state/proxyd`

注意：GEOIP/GEOSITE 规则需要 geo 数据文件。proxyd 默认从 jsDelivr 镜像（Loyalsoldier 主流规则仓库）下载，开箱即用；下载失败时会在日志提示并**自动降级为不含 GEO 规则运行**（其余规则照常），下一轮刷新自动重试恢复。也可以在配置里用 `geox-url` 换成自己的镜像：

```yaml
geox-url:
  geosite: "https://你的镜像/geosite.dat"
  geoip: "https://你的镜像/geoip.dat"
  mmdb: "https://你的镜像/Country.mmdb"
```

## 常驻运行

- macOS (launchd)：把 `proxyd serve -c /path/proxyd.yaml` 写入 `~/Library/LaunchAgents/com.proxyd.plist` 的 `ProgramArguments`，`RunAtLoad` + `KeepAlive`。
- Linux (systemd)：

  ```ini
  [Unit]
  Description=proxyd
  After=network-online.target

  [Service]
  ExecStart=/usr/local/bin/proxyd serve -c /etc/proxyd.yaml
  Restart=always

  [Install]
  WantedBy=multi-user.target
  ```

## 开发

```sh
go test ./...     # 单元测试 + 端到端测试（e2e/，本地假节点全流程验证）
```

项目结构：`cmd/proxyd`（CLI）、`internal/config`（配置）、`internal/subscribe`（订阅拉取与解析）、`internal/ruleurl`（规则 URL 拉取与解析）、`internal/pool`（健康检测与端口分配）、`internal/core`（mihomo 配置生成与内嵌运行）、`internal/app`（调度编排）、`internal/api`（自有 REST API 与 Web 控制台）、`internal/sysproxy`（系统代理开关）。
