## 1. 基线

- [x] 1.1 module 对齐 Go 1.27，清理 OCPC 专属依赖；verify: go build、go mod tidy。

## 2. 公共骨架

- [x] 2.1 生命周期、健康、严格配置；verify: 启动回收、取消排空、探针与配置测试。
- [x] 2.2 身份与 HMAC、防重放、golimit 适配、request/fault；verify: 篡改、过期、并发重复、键隔离测试。
- [x] 2.3 HTTP/gRPC server 与请求治理；verify: 真实协议请求、日志与跨协议配额测试。
- [x] 2.4 熔断/降级与 httpc 调用入口；verify: 状态转换、失败分类、预算与取消测试。

## 3. 验证服务和模板

- [x] 3.1 移除 OCPC 业务、配置、协议生成产物、专属脚本与依赖；verify: 文件和依赖无 OCPC 引用，根目录原有分层保留。
- [x] 3.2 独立服务模板及可选 gateway；verify: 独立目录编译、签名 HTTP/gRPC、私有身份边界。

## 4. 交付

- [x] 4.1 根目录设计文档、配置示例、README、systemd、传感器同步；verify: 配置逐项核对、模板与文档一致。
- [x] 4.2 全量门禁、Linux 编译、行为验证、现代写法与自审；verify: make check、template check、govulncheck。
- [x] 4.3 OpenSpec strict 验证并 archive；verify: 归档目录和合并 spec 实际存在。
