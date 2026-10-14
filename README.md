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
2. 修改 `go.mod` 的 module 为新服务实际路径，同步修改 `cmd`、`internal`、`gsrv.go` 中的自模块 imports（包括测试）和 proto `go_package`。架构脚本自动读取当前 module，无需手工改规则；保留既有 gtkit 依赖，不替换检查器中的旧骨架识别常量。
3. 执行 `GOWORK=off make gen` 重新生成 `pb`，再执行 `GOWORK=off go mod tidy`；禁止直接修改生成产物。
4. 在 `.example` 和真实配置同步修改服务名、监听端口、socket 路径及调用方凭据引用。改名副本须替换 `example-service`。`name` 必须与调用方 HTTP 签名目标一致；同机部署须避免端口/socket 冲突。
5. 按现有 proto → application → runtime 装配 → transport 映射 → bootstrap 注册流程开发业务，运行 `GOWORK=off make check` 和 `make build`。

记录创建服务所用的骨架提交；公共能力修复需逐个服务仓库同步并验证。公共机制不写业务规则，业务层不直接引用基础设施。

## 出站 HTTP/gRPC 调用

`clients` 缺省为空，不建下游客户端。启用方式和全部字段注释见 `config/env/env.yml.example` 中的注释块：将 `clients: {}` 替换为所需下游，配置本服务访问对方的 AppKey、凭据环境变量引用和静态目标。

每个下游共用一个 `Dependency`，HTTP/gRPC合计并发缺省100、整体预算缺省3s（含整个stream）、熔断缺省false。默认不排队、不执行业务重试或降级；长期stream需明确调整预算。gRPC策略重试与远端service config显式关闭；未发送/未处理RPC仍可能由grpc-go透明重试，沿用同一预算（[官方说明](https://pkg.go.dev/google.golang.org/grpc#WithDisableRetry)）。HTTP客户端基于gtkit/httpc，固定origin、不跟随重定向，Content-Type首尾空白按发送规则规范化后签名；不确定断连后不重放已签名请求。HTTPS/gRPC TLS默认验证服务器证书，明文只允许loopback。

配置由 `platform/config` 校验，客户端由原 `runtime/module/info` 层装配，结果保留在 `Components.HTTPClients/GRPCClients`。InfoService仍只返回服务信息；实际业务模块通过runtime adapter将客户端注入自己的application port，不在业务层导入传输实现。bootstrap在入站排空后关闭客户端，错误路径也回收资源。

根入口为 `NewDependency`、`NewServiceHTTPClient`、`NewServiceGRPCClient`。生成的gRPC客户端可直接使用 `ServiceGRPCClient`；HTTP `.Do(ctx, method, path, body, headers)` 接收已编码原始字节并返回状态、header、body。旧 `NewHTTPClient` 仍是基础gtkit/httpc入口，不自动签名或接入完整出站治理。

出站身份始终使用本服务配置的凭据，保留request ID，不直接转发入站调用方身份。日志不记录URL、body或凭据。启用metrics后新增 `gsrv_dependency_*` 请求/错误/耗时/在途/拒绝指标，名称只在装配期注册，最多64个固定依赖。

出站gRPC token须为32–4096字节。客户端流在最终成功响应后释放槽位，服务端流/双向流保持到EOF；取消、预算到期或Close即使不再Recv也会回收。上游预算耗尽保留超时错误并排除熔断样本，下游自身timeout仍计为失败。

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
GOWORK=off make check        # build、lint、vet、race、现代语法、架构、契约及复制检查
make proto-check            # 临时生成并比较完整pb集合，不修改工作区
make copy-check             # module/import/go_package和.example标识，不读取真实配置/凭据
bash scripts/check-copy.sh /本机/另一个服务源码目录 # 显式比较示例服务名、监听与socket冲突
make gen                    # 改 proto 后重新生成 pb；禁止手改产物
make build                  # Linux/amd64 二进制 bin/server，无 UPX/容器要求
```

`make check` 不访问媒体、数据库或 Redis，行为测试会绑定本机临时端口和 Unix socket。Linux systemd 使用样例保留在根目录 `GSRV_DESIGN.md`，按新服务名称和路径调整后由部署负责人安装。

契约检查复用已安装buf及本地protoc插件；首次缺少buf锁定依赖缓存时需要访问Buf源。复制冲突检查依据 `.example`，不检查实际生产监听或真实配置，部署负责人仍需核对目标机配置。
