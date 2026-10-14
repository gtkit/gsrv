# gsrv

基于 Go 1.27 与现有 gtkit 依赖的最小 HTTP/gRPC 服务骨架，当前模块路径为 `github.com/gtkit/gsrv`。创建新服务时复制整个项目源码，在新仓库内维护公共实现与业务；根包只负责通用能力的装配，业务继续采用当前项目的模块分层。

[架构、请求时序、装配、生命周期、整仓复制与实施方案](GSRV_DESIGN.md)

## 目录与边界

```text
cmd/server → internal/bootstrap → internal/runtime/module/info → internal/module/info/application
                               → internal/module/info/transport/grpc → application、pb
internal/platform/config       配置加载、默认值与本服务校验
internal/pkg                   通用实现：interceptor、requestid、limiter、logging、breaker 等
gsrv.go                        本仓库通用能力的装配入口
proto/info/v1 → pb/info/v1      唯一契约源及 buf 生成产物
```

OCPC 及其媒体 SDK、Redis、任务队列、业务配置和测试已按要求移除。`info` 模块只返回服务名和请求 ID，无业务规则，也不依赖数据库或 Redis。目录层级与依赖方向沿用原项目；未经明确授权不得调整。

## 复制项目创建新服务

1. 将项目源码复制到新目录，建立独立 Git 仓库；不要复制 `.git`、`bin`、`run`、`logs`、真实配置、凭据和证书私钥。
2. 修改 `go.mod` 的 module 为新服务实际路径，同步修改 `cmd`、`internal`、`gsrv.go` 中的自模块 imports（包括测试）、proto `go_package` 和架构检查脚本中的自模块路径。保留现有 gtkit 依赖；新服务不需要额外引用外部 gsrv 模块。
3. 执行 `GOWORK=off make gen` 重新生成 `pb`，再执行 `GOWORK=off go mod tidy`；禁止直接修改生成产物。
4. 从 `.example` 创建真实配置，修改服务名、监听端口、socket 路径及调用方凭据引用。`name` 必须与调用方 HTTP 签名的目标服务名一致；同机部署须避免端口/socket 冲突。
5. 按现有 proto → application → runtime 装配 → transport 映射 → bootstrap 注册流程开发业务，运行 `GOWORK=off make check` 和 `make build`。

记录创建服务所用的骨架提交；公共能力修复需逐个服务仓库同步并验证。公共机制不写业务规则，业务层不直接引用基础设施。

## 本地启动

1. 已安装 Go 1.27.2 或更新的 1.27 补丁版（已验证 1.27.2，包含 HTTP/2 安全修复）；生成契约还需要 buf 与 `buf.gen.yaml` 中的本地插件。
2. 将 `config/env/env.yml.example` 复制为 `config/env/env.yml`。逐项注释包含用途、默认值和约束。
3. 在进程环境中设置 `SERVICE_APP_SECRET` 和 `SERVICE_APP_TOKEN`，各至少 32 字节。配置只引用变量名称。
4. `GOWORK=off make run`。默认监听 HTTP `127.0.0.1:8080`、gRPC `127.0.0.1:9090`。

无认证探针：`GET /livez`、`GET /readyz`、标准 gRPC Health。业务接口：签名 `GET /v1/service`、认证 RPC `info.v1.InfoService/GetInfo`。gRPC metadata 为 `x-app-key` 和 `authorization: Bearer <token>`；HTTP 请使用 `gsrv.SignHTTP`，签名字段与示例见设计文档。

跨主机监听必须配置 TLS。`gateway_socket` 非空时，由私有 Unix socket 桥接 proto HTTP 接口；为空时 HTTP 只有探针和可由装配方替换的业务 handler。

## 检查与构建

App 在监听前校验共享健康状态并拒绝 typed-nil server；readiness 读取后台快照。默认 nonce 按 HTTP AppKey 预留份额，单方用满返回429而不挤占其他调用方；`grpc.max_inflight` 默认100，限制跨连接 unary/stream 合计并发，健康和私有 gateway 跳过。

`metrics.enabled` 默认关闭，启用要求 HTTP；本机直连 `GET /metrics` 可读取最小指标，不使用 AppKey、请求ID或原始URL标签。配置示例和根设计文档同步定义默认值及边界。

```sh
GOWORK=off make check        # 当前项目：build、lint、vet、race、现代语法、架构、buf lint
make gen                    # 改 proto 后重新生成 pb；禁止手改产物
make build                  # Linux/amd64 二进制 bin/server，无 UPX/容器要求
```

`make check` 不访问媒体、数据库或 Redis，行为测试会绑定本机临时端口和 Unix socket。Linux systemd 使用样例保留在根目录 `GSRV_DESIGN.md`，按新服务名称和路径调整后由部署负责人安装。
