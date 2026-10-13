## Why

当前 OCPC 的启动、协议治理和配置与业务装配耦合，无法供验证码、推送、文件和身份等独立服务直接复用。用户已批准将公共能力抽为独立 Go 模块，Linux 裸机部署，移除 OCPC，以简洁 InfoService 验证，并交付根目录设计文档。

## What Changes

- 公共模块路径设为用户确认的 `github.com/gtkit/gsrv`，Go 1.27；独立模板的 proto 生成实际服务契约。
- 提供生命周期、严格配置、健康检查、HTTP/gRPC 注册与请求治理公共入口。
- HTTP HMAC-SHA256 + 时间戳 + nonce；gRPC AppKey/token；两协议复用已认证身份、配额和日志。
- 复用现有 logger/v2、encry、golimit、httpc；新增 gobreaker/v2 封装和业务降级钩子。
- 提供无强制数据库/Redis的独立服务模板、Linux systemd 样例。
- 按用户最新指令移除 OCPC 业务、契约、专属配置/测试/依赖，按原有分层用根 InfoService 替换 OCPC，并保留同结构独立模板。
- 根目录 `GSRV_DESIGN.md` 包含依赖图、请求时序、装配流程、生命周期、配置与验收。

## Capabilities

### New Capabilities
- `gsrv-runtime`: 公共运行骨架、生命周期和探针。
- `gsrv-request-governance`: 身份、共享配额、协议错误映射、访问日志、熔断与降级入口。
- `gsrv-service-template`: 独立模板与 InfoService 验证。

### Modified Capabilities

无已有基线 spec；OCPC 的移除由用户明确授权。

## Impact

新增公开包，调整模块路径、业务装配和生成产物，新增模板、质量门禁和文档。现有 gtkit 依赖维持锁定版本；公共包不得依赖模板的 internal 包。不发布、不提交、不部署到外部系统。
