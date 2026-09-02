# Phase 1: Domain Events — proxyd 控制台体验与代理运行控制

## PRD Summary

参考 Clash Verge Rev 优化 proxyd 控制台的信息架构和交互密度，使用 beUI 官方 Registry 组件统一视觉，并新增端口映射总开关、订阅启停与编辑、自定义规则编辑与排序、配置导入预检，以及日志和连接诊断增强。

## Approved Domain Events Table

| Actor | Command | Domain Event | Business Rules / Invariants |
|:------|:--------|:-------------|:---------------------------|
| 管理员 | 关闭节点端口映射 | `PortMappingDisabled` | 只停止健康节点的一对一 listener；主端口、自动选优端口、策略组端口、订阅刷新和健康检查继续运行；稳定端口分配与快照必须保留。 |
| 管理员 | 开启节点端口映射 | `PortMappingEnabled` | 使用最新健康节点集合重新生成一对一 listener，并优先恢复已有稳定端口；不得破坏其他入口。 |
| 管理员 | 停用订阅 | `SubscriptionDisabled` | 停用订阅不再参与节点合并、健康检查和 listener 生成；订阅配置及已有缓存保留。 |
| 管理员 | 启用订阅 | `SubscriptionEnabled` | 启用后优先重新拉取；拉取失败时只有存在可用缓存才允许降级恢复，否则保持停用。 |
| 管理员 | 编辑订阅 | `SubscriptionUpdated` | 名称、URL、类型和启用状态必须合法且唯一；重命名必须原子更新引用该订阅的策略组。 |
| 管理员 | 手动刷新订阅 | `SubscriptionRefreshSucceeded` | 记录完成时间、节点总数、健康节点数和下一次全局刷新时间；新的健康集合用于重新生成运行配置。 |
| 订阅刷新策略（由管理员配置） | 定时刷新订阅 | `SubscriptionRefreshSucceeded` | 所有已启用订阅遵循全局刷新周期；停用订阅不得发起网络请求。 |
| 管理员 | 编辑自定义规则 | `CustomRuleUpdated` | 只允许修改本地 `custom-rules`；规则格式必须合法，远程规则内容保持只读。 |
| 管理员 | 调整自定义规则顺序 | `CustomRulesReordered` | 列表顺序就是规则匹配优先级，整个顺序变化必须一次性应用。 |
| 管理员 | 预检导入配置 | `ConfigurationImportValidated` | 预检只执行解析、业务校验与影响摘要，不得写盘或修改运行态。 |
| 管理员 | 确认导入配置 | `ConfigurationImportPersisted` | 只有通过预检且确认时内容摘要未变化的配置才能原子写盘；导入仍在重启后生效。 |

## Failure / Compensating Events

| Actor | Command | Domain Event | Triggered By |
|:------|:--------|:-------------|:-------------|
| 运行配置应用器 | 恢复端口映射旧状态 | `PortMappingChangeRolledBack` | 核心热重载或配置持久化失败；恢复旧内存配置、旧 listener 与旧持久化状态。 |
| 运行配置应用器 | 报告端口映射切换失败 | `PortMappingChangeFailed` | 切换失败且回滚完成；若回滚也失败，必须返回包含原始错误和回滚错误的组合错误。 |
| 订阅管理器 | 恢复停用前状态 | `SubscriptionDisableFailed` | 停用后的核心重载或持久化失败；恢复订阅、节点集合、策略组和 listener。 |
| 订阅管理器 | 保持订阅停用 | `SubscriptionEnableFailed` | 拉取失败且没有可用缓存，或应用新运行配置失败。 |
| 订阅管理器 | 恢复订阅与引用 | `SubscriptionUpdateFailed` | 校验、拉取、策略组引用更新、核心重载或持久化任一步失败。 |
| 订阅刷新策略（由管理员配置） | 保留上次可用节点 | `SubscriptionRefreshFailed` | 网络、解析或健康检测失败；记录失败原因并保留上次可用状态。 |
| 规则管理器 | 恢复旧规则 | `CustomRuleUpdateFailed` | 规则校验、核心重载或持久化失败。 |
| 规则管理器 | 恢复旧规则顺序 | `CustomRulesReorderFailed` | 新顺序应用或持久化失败。 |
| 配置导入器 | 拒绝导入配置 | `ConfigurationImportRejected` | YAML 解析、配置校验或影响分析失败；不产生任何写操作。 |
| 配置导入器 | 保留原配置文件 | `ConfigurationImportFailed` | 确认摘要不匹配或原子写盘失败。 |

## Key Design Decisions

| Question Raised | Decision | Rationale |
|:----------------|:---------|:----------|
| 端口映射开关控制哪些入口？ | 只控制健康节点的一对一 listener。 | 保持主端口、AUTO 和策略组端口的稳定语义。 |
| 关闭映射后是否删除端口分配？ | 不删除，继续维护稳定分配和 `mapping.json`。 | 重新开启时可恢复熟悉端口，减少调用方配置漂移。 |
| 停用订阅是否删除配置或缓存？ | 均不删除，只从活动节点流水线排除。 | 停用是可逆运营动作，不等同于删除。 |
| 订阅重命名如何处理策略组？ | 在同一个应用用例中原子更新所有订阅引用。 | 防止出现悬空策略组引用。 |
| 哪些规则允许在 UI 编辑？ | 只编辑本地 `custom-rules`，远程内容只读。 | 远程源会被刷新覆盖，UI 修改会制造错误持久化预期。 |
| 配置导入是否热应用？ | 不热应用，预检和确认后只原子写盘，重启生效。 | `api-listen`、`state-dir` 等字段无法在当前请求内安全热切换。 |

## Approval

- **Approved by:** 用户
- **Date:** 2026-09-02
- **Notes:** 用户确认采用审计报告中推荐的三项语义，并要求与原 UI 重排和端口映射计划一并实施。
