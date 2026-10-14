// Package gsrv 是公共骨架的装配入口；实现留在 internal/pkg，业务沿用原有分层。
package gsrv

import (
	"context"
	"net/http"
	"time"

	"github.com/gtkit/gsrv/internal/pkg/auth"
	"github.com/gtkit/gsrv/internal/pkg/breaker"
	"github.com/gtkit/gsrv/internal/pkg/configloader"
	"github.com/gtkit/gsrv/internal/pkg/fault"
	"github.com/gtkit/gsrv/internal/pkg/gateway"
	grpctransport "github.com/gtkit/gsrv/internal/pkg/grpcserver"
	"github.com/gtkit/gsrv/internal/pkg/health"
	httptransport "github.com/gtkit/gsrv/internal/pkg/httpserver"
	"github.com/gtkit/gsrv/internal/pkg/lifecycle"
	"github.com/gtkit/gsrv/internal/pkg/limiter"
	"github.com/gtkit/gsrv/internal/pkg/logging"
	"github.com/gtkit/gsrv/internal/pkg/requestid"
	"github.com/gtkit/httpc"
	"github.com/gtkit/logger/v2"
	"google.golang.org/protobuf/proto"
)

// 生命周期类型只提供通用装配，不依赖任何业务模块。
type (
	App    = lifecycle.App
	Server = lifecycle.Server
	Hook   = lifecycle.Hook
	Option = lifecycle.Option
)

// New 构造一次性运行的App；至少注册一个server，监听前校验共享健康状态。
// 未显式WithHealth时采用健康感知server的状态，错配/nil/typed-nil返回错误。
func New(name string, options ...Option) (*App, error) { return lifecycle.New(name, options...) }

// WithServers 按依赖顺序注册，关闭按同一顺序排空。
func WithServers(servers ...Server) Option { return lifecycle.WithServers(servers...) }

// WithHealth 注入 HTTP、gRPC 共用的健康状态。
func WithHealth(state *HealthState) Option { return lifecycle.WithHealth(state) }

// WithShutdownTimeout 设置正的总关闭预算；缺省 20s。
func WithShutdownTimeout(timeout time.Duration) Option { return lifecycle.WithShutdownTimeout(timeout) }

// WithHook 显式注册后台组件；Stop 按逆序执行。
func WithHook(hook Hook) Option { return lifecycle.WithHook(hook) }

// 身份类型不承载用户权限和业务授权规则。
type (
	Credential       = auth.Credential
	Identity         = auth.Identity
	AuthConfig       = auth.Config
	Verifier         = auth.Verifier
	NonceStore       = auth.NonceStore
	MemoryNonceStore = auth.MemoryNonceStore
)

// NewVerifier 校验凭据并创建共享认证器；空凭据报错。
func NewVerifier(cfg AuthConfig) (*Verifier, error) { return auth.New(cfg) }

// NewMemoryNonceStore 创建原始全局容量存储；显式注入时容量隔离由装配方负责。
// 默认NewVerifier会设置按AppKey预留的内存策略。
func NewMemoryNonceStore(capacity int) (*MemoryNonceStore, error) {
	return auth.NewMemoryNonceStore(capacity)
}

// SignHTTP 对原始 HTTP 请求签名；重试须使用新 nonce，body 上限 1MiB。
func SignHTTP(r *http.Request, service string, credential Credential, at time.Time, nonce string) error {
	return auth.SignHTTP(r, service, credential, at, nonce)
}

// ValidName 校验 1–64 位 ASCII 服务名、AppKey 和 KeyID。
func ValidName(name string) bool { return auth.ValidName(name) }

// WithIdentity 传递已认证身份；业务代码不得据未验证请求字段自行构造身份。
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return auth.WithIdentity(ctx, identity)
}

// IdentityFromContext 读取已认证调用方，未认证时返回 false。
func IdentityFromContext(ctx context.Context) (Identity, bool) { return auth.FromContext(ctx) }

// HealthState 在两个协议之间共享进程就绪状态和关键依赖探测。
type HealthState = health.State

// NewHealth 创建未就绪状态；由 App 在启动完成后置 ready。
func NewHealth() *HealthState { return health.New() }

// RateLimiter 接收已认证 AppKey，由 HTTP/gRPC 复用同一实例。
type RateLimiter = limiter.Limiter

// NewLocalLimiter 适配 gtkit/golimit；qps=0 关闭，负数报错，namespace 必须隔离实例。
func NewLocalLimiter(namespace string, qps int) (*limiter.Local, error) {
	return limiter.NewLocal(namespace, qps)
}

// LogConfig 配置 gtkit/logger/v2；Console 缺省 true，Path 空关闭文件输出。
type LogConfig = logging.Config

// Metrics 是显式持有的最小运行指标，标签不含请求身份或URL。
type Metrics = logging.Metrics

// NewMetrics 创建指标集，不启动后台任务或全局注册。
func NewMetrics(service string) (*Metrics, error) { return logging.NewMetrics(service) }

// NewLogger 先验证输出目录；调用方启动期 SetDefault，退出时 Sync。
func NewLogger(cfg LogConfig) (*logger.Logger, error) { return logging.New(cfg) }

// LoadConfig 严格解码单份 YAML；服务默认值与约束仍由 internal/platform/config 处理。
func LoadConfig(path string, target any) error { return configloader.Load(path, target) }

// SecretFromEnv 从指定环境变量读取凭据，缺失或空值报错，不回显值。
func SecretFromEnv(name string) (string, error) { return configloader.Secret(name) }

// ErrorCode 是协议无关的治理错误码；业务层可在 transport 中翻译领域错误。
type ErrorCode = fault.Code

// 公共错误码同时映射至 HTTP 和 gRPC。
const (
	CodeOK                = fault.OK
	CodeNotFound          = fault.NotFound
	CodePayloadTooLarge   = fault.PayloadTooLarge
	CodeInvalidArgument   = fault.InvalidArgument
	CodeUnauthenticated   = fault.Unauthenticated
	CodePermissionDenied  = fault.PermissionDenied
	CodeResourceExhausted = fault.ResourceExhausted
	CodeUnavailable       = fault.Unavailable
	CodeDeadlineExceeded  = fault.DeadlineExceeded
	CodeCanceled          = fault.Canceled
	CodeInternal          = fault.Internal
)

// NewFault 构造允许对外展示的治理错误文案。
func NewFault(code ErrorCode, message string) error { return fault.New(code, message) }

// WrapFault 保留底层原因，但对外仅返回显式的安全文案。
func WrapFault(code ErrorCode, message string, cause error) error {
	return fault.Wrap(code, message, cause)
}

// ErrorCodeOf 将未知错误归为 INTERNAL，并识别上下文取消/超时。
func ErrorCodeOf(err error) ErrorCode { return fault.CodeOf(err) }

// WriteHTTPError 输出安全 JSON 错误，并将错误码写入本请求访问记录。
func WriteHTTPError(w http.ResponseWriter, r *http.Request, err error) {
	httptransport.WriteError(w, r, err)
}

// HTTP 类型封装服务器治理，业务 handler 由服务装配方提供。
type (
	HTTPConfig        = httptransport.Config
	HTTPServer        = httptransport.Server
	HTTPAuthenticator = httptransport.Authenticator
	HTTPOption        = httptransport.Option
)

// NewHTTPServer 组合探针、认证、限流、恢复、日志和请求预算。
func NewHTTPServer(service string, cfg HTTPConfig, handler http.Handler, verifier HTTPAuthenticator, quota RateLimiter, state *HealthState, options ...HTTPOption) (*HTTPServer, error) {
	return httptransport.New(service, cfg, handler, verifier, quota, state, options...)
}

// WithHTTPMetrics 注入指标并开启仅直连loopback可读的GET /metrics。
func WithHTTPMetrics(metrics *Metrics) HTTPOption { return httptransport.WithMetrics(metrics) }

// gRPC 类型只封装协议治理；业务由生成的注册函数接入。
type (
	GRPCConfig        = grpctransport.Config
	GRPCServer        = grpctransport.Server
	GRPCOption        = grpctransport.Option
	GRPCAuthenticator = grpctransport.Authenticator
)

// NewGRPCServer 创建公开 gRPC 入口；认证器必填。
func NewGRPCServer(service string, cfg GRPCConfig, options ...GRPCOption) (*GRPCServer, error) {
	return grpctransport.New(service, cfg, options...)
}

// NewGatewayBackend 创建权限受限 Unix socket 的内部 gRPC 入口；禁止暴露为 TCP。
func NewGatewayBackend(service string, cfg GRPCConfig, options ...GRPCOption) (*GRPCServer, error) {
	return grpctransport.NewGatewayBackend(service, cfg, options...)
}

// WithGRPCAuth 注入公开 gRPC 认证器。
func WithGRPCAuth(verifier GRPCAuthenticator) GRPCOption { return grpctransport.WithAuth(verifier) }

// WithGRPCLimiter 注入与 HTTP 共用的配额；nil 关闭 QPS 限流。
func WithGRPCLimiter(quota RateLimiter) GRPCOption { return grpctransport.WithLimiter(quota) }

// WithGRPCMetrics 注入外部业务请求指标，健康和私有gateway跳过。
func WithGRPCMetrics(metrics *Metrics) GRPCOption { return grpctransport.WithMetrics(metrics) }

// WithGRPCHealth 注入与 App/HTTP 共用的健康状态。
func WithGRPCHealth(state *HealthState) GRPCOption { return grpctransport.WithHealth(state) }

// WithGRPCValidation 注入 protovalidate；每次 unary 请求和 stream RecvMsg 均执行。
func WithGRPCValidation(validate func(proto.Message) error) GRPCOption {
	return grpctransport.WithValidation(validate)
}

// ToGRPCStatus 将框架或上下文错误翻译为安全 gRPC status。
func ToGRPCStatus(err error) error { return grpctransport.ToStatus(err) }

// Gateway 类型接入 proto 生成的 Handler 注册函数。
type (
	Gateway         = gateway.Gateway
	GatewayRegister = gateway.Register
)

// NewGateway 只连接权限受限的本进程 Unix socket；须位于 HTTP 治理链之后。
func NewGateway(socket string, register GatewayRegister) (*Gateway, error) {
	return gateway.New(socket, register)
}

// RequestScope 保存 request ID、已认证 AppKey 和降级标记。
type RequestScope = requestid.Scope

// RequestFromContext 返回当前请求记录；后台路径可能为 nil。
func RequestFromContext(ctx context.Context) *RequestScope { return requestid.FromContext(ctx) }

// 熔断类型按单个依赖持有，不包含业务降级结果。
type (
	BreakerConfig  = breaker.Config
	Breaker[T any] = breaker.Breaker[T]
)

// NewBreaker 创建指定依赖的熔断器。
func NewBreaker[T any](cfg BreakerConfig) (*Breaker[T], error) { return breaker.New[T](cfg) }

// RunDependency 共享调用总预算，仅按业务 allow/fallback 执行降级。
func RunDependency[T any](ctx context.Context, timeout time.Duration, guard *Breaker[T], operation func(context.Context) (T, error), allow func(error) bool, fallback func(context.Context, error) (T, error)) (T, error) {
	return breaker.Run(ctx, timeout, guard, operation, allow, fallback)
}

// NewHTTPClient 创建带正超时的 gtkit/httpc 客户端，不自动重试业务请求。
func NewHTTPClient(timeout time.Duration, options ...httpc.Option) (*httpc.Client, error) {
	return breaker.NewHTTPClient(timeout, options...)
}
