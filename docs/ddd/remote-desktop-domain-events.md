# 远程桌面领域事件与边界

## 需求摘要

将 RDP/VNC 从“远程连接”的 SSH/通用端口转发界面拆出，建立独立的“远程桌面”垂直模块。服务端需要知道操作系统桌面服务的真实端口及监听状态；客户端需要持久保存常用连接并管理临时会话。底层数据通道继续复用 tailcat，但桌面领域不得直接依赖 tailcat、HTTP 或配置文件。

## Bounded Context

| 上下文 | 责任 | 明确不负责 |
|---|---|---|
| `remote` | tailcat 身份、token、白名单、暴露端口、通用转发 | RDP/VNC 档案和桌面客户端启动语义 |
| `desktop` | 协议值对象、连接档案约束、临时会话唯一性与回收规则 | token 存储、tailcat 实现、系统桌面账号密码 |
| `app` | desktop 与 remote 的跨上下文事务、持久化与失败补偿 | HTTP DTO 和 GUI 启动 |
| `api` / Web | 输入映射、服务监听展示、系统客户端启动目标 | 绕过系统 RDP/VNC 认证 |

连接档案属于 Generic CRUD：它没有独立聚合行为，只保存名称、远端引用、协议、端口和可选用户名。`DesktopSession` 是有生命周期的领域实体，由 `desktop.Manager` 管理；`Protocol` 是值对象；临时隧道通过 `Forward` 端口注入。

## 已批准事件

| Actor | Command | Domain Event | Business Rules / Invariants |
|---|---|---|---|
| 管理员 | 配置桌面服务 | `DesktopServiceConfigured` | RDP/VNC 端口必须为 1-65535 且不能相同；是否开放以 `remote.serve` 为唯一事实来源。 |
| 管理员 | 开放桌面服务 | `DesktopServiceExposed` | 修改已开放服务的端口时，必须在同一事务移除旧端口、加入新端口并调和 remote 运行态。 |
| 管理员 | 保存桌面连接 | `DesktopConnectionSaved` | 名称唯一；只引用 `remote.remotes` 名称；不保存密码或 token 副本；允许远端暂时缺失。 |
| 用户 | 启动桌面连接 | `DesktopSessionStarted` | 启动前远端必须能解析；同一档案只保留一个会话；监听固定绑定 proxyd 主机回环地址。 |
| 用户 | 再次打开客户端 | `DesktopClientLaunchRequested` | 复用现有会话；RDP 下载无密码 `.rdp`，VNC 使用 `vnc://`。 |
| 用户 | 断开桌面连接 | `DesktopSessionStopped` | 先从管理器移除，再关闭 listener、活动连接和 tailcat 客户端；关闭失败不能留下可复用的幽灵会话。 |
| 回收策略 | 清理遗忘会话 | `DesktopSessionExpired` | 从未连接超过宽限期、连接后空闲超时或达到最长寿命任一成立即回收；所有会话共用一个清扫协程。 |

## 失败与补偿

| Command | Failure / Compensation |
|---|---|
| 配置/开放桌面服务 | 校验、remote 调和或落盘失败时恢复 `desktop`、`remote` 内存快照，并重新应用旧 remote 运行态；组合返回原始与回滚错误。 |
| 保存连接档案 | 校验或落盘失败时恢复旧桌面配置，不影响运行中的会话快照。 |
| 启动桌面会话 | 远端解析、token、监听或隧道初始化失败时不登记会话；并发多建出的额外隧道立即关闭。 |
| 应用退出 | 必须先关闭 desktop 会话再关闭 remote，确保适配器仍可按依赖顺序收口。 |

## 关键决策

- 页面与“远程连接”分离，避免 SSH/通用转发和桌面服务概念混杂。
- 本机 TCP 握手只用于提示 `listening`，失败不是页面级错误；proxyd 不主动开启系统桌面服务。
- 浏览器启动的回环地址属于 proxyd 主机。API 非回环监听时 Web 必须警告浏览器可能不在同机。
- 密码始终由操作系统桌面客户端管理；用户名禁止 CR/LF，防止 `.rdp` 指令注入。
- tailcat 仍优先 NAT 穿透后的 WireGuard 点对点路径，失败时保持 DERP 中继。

## Approval

- **Approved by:** 用户
- **Date:** 2026-09-04
- **Notes:** 用户确认采用独立远程桌面页面，并要求服务端端口管理、客户端连接记录持久化与 NAT 穿透链路复用。
