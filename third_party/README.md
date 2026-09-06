# 第三方依赖补丁

`mihomo/` 保留[上游 v1.19.30](https://github.com/MetaCubeX/mihomo/tree/v1.19.30) 源码和许可证。运行逻辑补丁共三处：`log/log.go` 使用 `atomic.Int32` 同步日志级别的全部读写，覆盖后台输出、配置热更新与控制器 API。`adapter/inbound/auth.go` 与 `adapter/inbound/ipfilter.go` 使用读写锁和快照副本，修复模块启停与新连接认证之间的前缀策略竞态。不能用 proxyd 外围互斥锁替代此补丁，因为上游后台日志不持有该锁。

升级时以完整新版本替换此目录，检查上游是否已修复；已修复则删除本地 replace，否则重新应用同步补丁。通过根模块 `go test -race ./internal/proxy/core ./internal/app` 验证，不编辑 Go module cache。第三方原始注释保持上游格式，自有补丁使用中文说明。
