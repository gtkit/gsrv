## Context

用户已批准总体方案并确认 module 为 github.com/gtkit/gsrv。部署为 Linux 单实例，无容器、注册中心。用户最新指令明确移除 OCPC；原分层中的只读 InfoService 作为验证模块，并提供同结构独立模板。当前 Go 1.27.2 已安装，旧 import 与 module 不匹配是实际基线失败。

## Goals / Non-Goals

Goals: 公共骨架可被独立模块导入；双协议身份/限流/日志共用；配置严格加载；生命周期错误可回收；熔断封装与降级钩子可测试；独立模板实际编译运行；根目录文档可直接用于后续开发。

Non-Goals: 业务规则、动态注册中心、数据库强制初始化、分布式配额、发布或远程部署。保持根目录 cmd、internal/bootstrap、internal/runtime/module、internal/module 分层，InfoService 替换 OCPC；公共包不依赖内部验证模块。独立模板沿用相同分层。

## Decisions

1. root package gsrv 管理 Server 接口（Listen/Serve/Shutdown/Close）、显式生命周期 hooks、signals。监听全部成功后置 ready，任一 Serve 异常退出统一关停，Run 不可重入。协议服务由 internal/pkg 内封装实现，根 gsrv 包只导出装配入口；不新设顶层能力目录。
2. health.State 使用一个生命周期就绪标志和有超时的必需依赖探测；HTTP/gRPC 共享状态。仅就绪探针探测依赖，监视器归 Run 生命周期管理。
3. config.Load 严格单文档 YAML；配置/默认值/示例/文档逐项一致。服务配置以 struct 组合；凭据支持明确的环境变量引用。
4. auth.Verifier 使用公开 encry/hmac 原语，固定 Base64 编码、版本/目标服务/AppKey/KeyID/method/escaped path/raw query/content type/timestamp/nonce/body SHA256 签名，body 有界。内存 nonce store 原子去重、容量上限、按到期时间清理，不后台起 goroutine。缺凭据 fail-closed；gRPC token 与 AppKey 绑定。远程监听需要 TLS。
5. limiter.Local 保持 golimit v1 锁定版本，键含实例 namespace+服务+已认证 AppKey；两协议同一实例配额。0 显式关闭，burst=QPS，不暴露无支持的 burst 配置。
6. request 记录对象用于外层访问日志读取内层认证身份；日志 deferred 覆盖 panic/拒绝。协议错误通过中立 fault 类型映射，不含 secret/token/body。
7. resilience 用 gobreaker/v2；泛型执行入口接受调用函数与可选 fallback。上下文取消、鉴权和参数错误不降级；fallback 有剩余预算。业务决定失败分类与降级结果。客户端超时由显式适配器限定，默认无业务重试。
8. 删除 OCPC 及其专属启动、配置、测试、生成契约和依赖。模板直接使用公开包，InfoService 仅返回服务名和 request ID，并提供 proto/gateway 接线示例；可选 gateway 使用私有 Unix socket，由 HTTP 身份校验和限流控制入口，内部链只校验/恢复。
9. 独立模板放 templates/service，go.mod 显式本地 replace 便于未发布状态验证；复制到其他仓库时替换 module 与 replace。systemd 仅交付文件，不操作系统。

## Risks / Trade-offs

- 内存 nonce 不跨重启/副本 → 文档明确，提供 NonceStore 接口而不强制 Redis。
- golimit v1 进程全局清理无 Close → 文档说明，实例 namespace 避免互相覆盖；不升级 major。
- 签名受代理 URI 改写影响 → 按最终发送 URI 签名并禁用 method override。
- 公开包不得依赖 internal 验证模块 → import sensor 检查依赖方向，根验证程序与独立模板均验证装配。
- 关闭 hook 必须遵守 ctx → 文档合同与验证，不内置任何业务 worker。

## Migration Plan

先统一公开 module 与公共包；移除 OCPC 及专属依赖；按原分层装配根 InfoService 并生成契约，构建同结构独立模板；同步文档/传感器/配置；跑门禁。仅交付源码。回滚为恢复之前源码和配置，无数据库迁移。

## Open Questions

无阻塞设计问题。真实 QPS/延迟尚未给出，方案中的容量值为压测输入假设。


## Directory constraint

必须对齐参考项目 ocpc-tool-service 的目录层级：cmd/server、internal/bootstrap、internal/platform/config、internal/runtime/module、internal/module/<m>/application 与 transport/grpc、internal/pkg。通用能力位于 internal/pkg，gRPC 拦截器与请求标识沿用 interceptor/requestid。bootstrap 沿用 app/logging/grpc/gateway/serve 文件职责。根 gsrv 包仅作独立模块公开入口；独立模板使用相同业务层级。未经用户明确授权不得调整此约束。

## Verified dependency correction

govulncheck 发现 grpc v1.83.0 与 x/net v0.57.0 存量可达漏洞。依据官方 Go 漏洞库升级为 grpc v1.83.2、x/net v0.60.0；依赖约束同步 x/sys v0.48.0、x/text v0.42.0。gtkit major 与锁定版本不改。
