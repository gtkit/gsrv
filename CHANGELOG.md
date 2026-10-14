# Changelog

## [Unreleased]

### Added

- 补齐共享健康状态校验/缓存、nonce AppKey 容量预留、gRPC 跨连接总并发和本机最小指标；保持目录与锁定依赖。
- 复审覆盖 typed-nil、重复就绪、旧探测竞争、探测 panic、认证/校验拒绝及真实流取消的并发槽位回收；同步独立模板与配置。

- 提供可复用的 HTTP/gRPC 公共骨架和独立服务模板，支持信号关闭、共享探针、严格配置与显式服务注册。
- 提供 HTTP HMAC 防重放、gRPC 调用方认证、共享 AppKey 限流、统一访问日志和业务降级钩子。
- 提供 Linux systemd 样例和根目录架构文档。

### Changed

- 模块路径确认为 `github.com/gtkit/gsrv`，采用 Go 1.27；保留原项目目录分层，通用实现放在 `internal/pkg`。

### Removed

- 按用户要求移除 OCPC 业务、媒体协议、队列、Redis 依赖及专属配置、测试和部署脚本，以简单 `info` 模块验证骨架。

### Security

- 将 gRPC 和 HTTP/2 相关依赖更新至漏洞扫描要求的修复版本，gtkit 依赖版本保持不变。
