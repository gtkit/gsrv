# 独立服务模板

独立模块 `example.com/service` 使用 `github.com/gtkit/gsrv` 根包公开入口。服务目录和依赖方向与根项目一致：

```text
cmd/server → internal/bootstrap → internal/runtime/module/info → internal/module/info/application
                               → internal/module/info/transport/grpc → application、pb
internal/platform/config       严格配置加载和服务默认值
proto/info/v1 → pb/info/v1      buf 契约与生成产物
```

`bootstrap` 保留 app/logging/grpc/gateway/serve 文件职责。公共实现由 gsrv 持有，不复制到新服务，也不从独立仓库导入 gsrv 的 internal 包。

## 在本仓库验证

构建/运行使用 Go 1.27.2 或更新的 1.27 补丁版；1.27.2 是本轮实际验证版本。

`go.mod` 使用临时 `v0.0.0` 与 `replace github.com/gtkit/gsrv => ../..`，仅用于当前尚未发布的源码联调，不代表已发布版本。

1. 复制 `config/env/env.yml.example` 为 `config/env/env.yml`。
2. 在进程环境中设置 `SERVICE_APP_SECRET` 和 `SERVICE_APP_TOKEN`，值均至少 32 字节。
3. `GOWORK=off make run`。

签名 HTTP `GET /v1/service` 和 gRPC `info.v1.InfoService/GetInfo` 返回真实配置的服务名及 request ID。无数据库或 Redis。

## 复制到独立仓库

复制本目录后，用 `go mod edit -replace=github.com/gtkit/gsrv=/本机/gsrv/绝对路径` 指向源码即可独立编译。正式发布 gsrv 后，选择实际发布版本并删除本地 replace；不要把 `v0.0.0` 当作远端可下载版本。

使用真实服务 module 时，同步修改 `go.mod`、手写 Go 文件中的 `example.com/service` import 及 `proto/info/v1/info.proto` 的 `go_package`，再执行 `make gen` 和 `go mod tidy`。不要修改 `pb` 生成文件。将 `info` 替换为验证码、推送、文件或身份模块时，继续采用当前层级，不移动 application/transport/runtime 的位置。

```sh
GOWORK=off make check
make build                  # bin/server，Linux/amd64
```

部署样例见 `deploy/service.service.example`。生产配置将 `gateway_socket` 设为 `/run/gsrv-service/gateway.sock`，使用独立服务用户；systemd 的 `RuntimeDirectoryMode=0700` 与 socket `0600` 保护内部身份通道。`TimeoutStopSec=30s` 大于应用总关闭预算 `20s`。远程 HTTP/gRPC 监听必须同时配置各自的 TLS 证书/私钥。日志默认显式输出控制台，由 journald 收集；所有配置字段的默认值、关联项和边界见 `.example` 注释及根目录 `GSRV_DESIGN.md`。
