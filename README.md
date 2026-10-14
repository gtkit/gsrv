# gsrv

基于 Go 1.27 与现有 gtkit 依赖的最小 HTTP/gRPC 服务骨架，模块路径为 `github.com/gtkit/gsrv`。公共入口只负责通用能力的装配，业务继续采用当前项目的模块分层。

[架构、请求时序、装配、生命周期与实施方案](GSRV_DESIGN.md) · [独立服务模板](templates/service/README.md)

## 目录与边界

```text
cmd/server → internal/bootstrap → internal/runtime/module/info → internal/module/info/application
                               → internal/module/info/transport/grpc → application、pb
internal/platform/config       配置加载、默认值与本服务校验
internal/pkg                   通用实现：interceptor、requestid、limiter、logging、breaker 等
gsrv.go                        独立模块公开入口，供各服务引用
proto/info/v1 → pb/info/v1      唯一契约源及 buf 生成产物
templates/service              独立 Go 模块，沿用相同服务分层
```

OCPC 及其媒体 SDK、Redis、任务队列、业务配置和测试已按要求移除。`info` 模块只返回服务名和请求 ID，无业务规则，也不依赖数据库或 Redis。目录层级与依赖方向沿用原项目；未经明确授权不得调整。

## 本地启动

1. 已安装 Go 1.27.2 或更新的 1.27 补丁版（已验证 1.27.2，包含 HTTP/2 安全修复）；生成契约还需要 buf 与 `buf.gen.yaml` 中的本地插件。
2. 将 `config/env/env.yml.example` 复制为 `config/env/env.yml`。逐项注释包含用途、默认值和约束。
3. 在进程环境中设置 `SERVICE_APP_SECRET` 和 `SERVICE_APP_TOKEN`，各至少 32 字节。配置只引用变量名称。
4. `GOWORK=off make run`。默认监听 HTTP `127.0.0.1:8080`、gRPC `127.0.0.1:9090`。

无认证探针：`GET /livez`、`GET /readyz`、标准 gRPC Health。业务接口：签名 `GET /v1/service`、认证 RPC `info.v1.InfoService/GetInfo`。gRPC metadata 为 `x-app-key` 和 `authorization: Bearer <token>`；HTTP 请使用 `gsrv.SignHTTP`，签名字段与示例见设计文档。

跨主机监听必须配置 TLS。`gateway_socket` 非空时，由私有 Unix socket 桥接 proto HTTP 接口；为空时 HTTP 只有探针和可由装配方替换的业务 handler。

## 检查与构建

```sh
GOWORK=off make check        # 根服务与独立模板：build、lint、vet、race、现代语法、架构、buf lint
make gen                    # 改 proto 后重新生成 pb；禁止手改产物
make build                  # Linux/amd64 二进制 bin/server，无 UPX/容器要求
make -C templates/service build
```

`make check` 不访问媒体、数据库或 Redis，行为测试会绑定本机临时端口和 Unix socket。Linux systemd 样例位于 `templates/service/deploy/service.service.example`；当前仅交付源码与样例，不执行安装或部署。
