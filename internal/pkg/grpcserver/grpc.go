// Package grpc 提供 gRPC server、认证和请求治理适配。
package grpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gtkit/gsrv/internal/pkg/auth"
	"github.com/gtkit/gsrv/internal/pkg/health"
	"github.com/gtkit/gsrv/internal/pkg/interceptor"
	"github.com/gtkit/gsrv/internal/pkg/limiter"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/proto"
)

// 私有 gateway metadata 名称与 interceptor 保持一致。
const (
	GatewayAppKey = interceptor.GatewayAppKey
	GatewayKeyID  = interceptor.GatewayKeyID
)

// Authenticator 校验绑定 AppKey 的 token。
type Authenticator = interceptor.Authenticator

// Config 定义 gRPC 监听、消息上限和 unary 预算。
type Config struct {
	_                    struct{}      // 强制外部使用带键字面量。
	Addr                 string        `yaml:"addr"`                   // TCP 地址；空值 127.0.0.1:9090；内部 gateway 为 socket 路径。
	Network              string        `yaml:"network"`                // tcp（默认）或 unix；私有 gateway 必须 unix。
	CertFile             string        `yaml:"cert_file"`              // TLS 证书路径；非 loopback TCP 必填，与 key_file 成对。
	KeyFile              string        `yaml:"key_file"`               // TLS 私钥路径；不入库，与 cert_file 成对。
	RequestTimeout       time.Duration `yaml:"request_timeout"`        // unary 上下文预算；0=3s；不延长客户端 deadline。
	MaxRecvBytes         int           `yaml:"max_recv_bytes"`         // 入站消息上限；0=1MiB；必须为正。
	MaxSendBytes         int           `yaml:"max_send_bytes"`         // 出站消息上限；0=1MiB；必须为正。
	MaxConcurrentStreams uint32        `yaml:"max_concurrent_streams"` // 每连接并行流数；0=100；并非逐消息 QPS。
	Reflection           bool          `yaml:"reflection"`             // 是否注册反射；默认 false；开启后仍执行 stream 认证。
}

type options struct {
	authenticator Authenticator
	quota         limiter.Limiter
	state         *health.State
	validate      func(proto.Message) error
	trusted       bool
}

// Option 配置协议治理组件。
type Option func(*options)

// WithAuth 注入调用方认证器；公开 server 必填。
func WithAuth(a Authenticator) Option { return func(o *options) { o.authenticator = a } }

// WithLimiter 注入与 HTTP 共用的配额实例；nil 不启用 QPS 限流。
func WithLimiter(l limiter.Limiter) Option { return func(o *options) { o.quota = l } }

// WithHealth 注入与 HTTP/App 共用的状态。
func WithHealth(s *health.State) Option { return func(o *options) { o.state = s } }

// WithValidation 注入 protovalidate.Validate；unary 和 stream RecvMsg 均校验。
func WithValidation(v func(proto.Message) error) Option { return func(o *options) { o.validate = v } }

// Server 实现 gsrv.Server，不依赖任何业务模块。
type Server struct {
	server   *grpc.Server
	cfg      Config
	mu       sync.RWMutex
	listener net.Listener
}

// New 构造公开入口；不允许缺认证器静默开放业务方法。
func New(service string, cfg Config, opts ...Option) (*Server, error) {
	return build(service, cfg, false, opts...)
}

// NewGatewayBackend 构造仅通过权限受限 Unix socket 接收 gateway 身份的内部入口。
// 调用方须使用独立服务用户、0700 父目录，不能将本入口暴露为 TCP。
func NewGatewayBackend(service string, cfg Config, opts ...Option) (*Server, error) {
	if cfg.Network != "unix" {
		return nil, errors.New("grpc: gateway backend requires unix network")
	}
	return build(service, cfg, true, opts...)
}

func build(service string, cfg Config, trusted bool, opts ...Option) (*Server, error) {
	if cfg.Network == "" {
		cfg.Network = "tcp"
	}
	if cfg.Addr == "" && cfg.Network == "tcp" {
		cfg.Addr = "127.0.0.1:9090"
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 3 * time.Second
	}
	if cfg.MaxRecvBytes == 0 {
		cfg.MaxRecvBytes = 1 << 20
	}
	if cfg.MaxSendBytes == 0 {
		cfg.MaxSendBytes = 1 << 20
	}
	if cfg.MaxConcurrentStreams == 0 {
		cfg.MaxConcurrentStreams = 100
	}
	o := options{state: health.New(), trusted: trusted}
	for _, option := range opts {
		option(&o)
	}
	if !auth.ValidName(service) || cfg.Addr == "" || o.state == nil || cfg.RequestTimeout <= 0 || cfg.MaxRecvBytes <= 0 || cfg.MaxSendBytes <= 0 ||
		(!trusted && o.authenticator == nil) || (cfg.Network != "tcp" && cfg.Network != "unix") {
		return nil, errors.New("grpc: invalid service, auth, health or limits")
	}
	serverOpts := []grpc.ServerOption{grpc.MaxRecvMsgSize(cfg.MaxRecvBytes), grpc.MaxSendMsgSize(cfg.MaxSendBytes), grpc.MaxConcurrentStreams(cfg.MaxConcurrentStreams)}
	if (cfg.CertFile == "") != (cfg.KeyFile == "") {
		return nil, errors.New("grpc: incomplete TLS pair")
	}
	if cfg.Network == "tcp" {
		host, _, err := net.SplitHostPort(cfg.Addr)
		if err != nil {
			return nil, fmt.Errorf("grpc address: %w", err)
		}
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) && cfg.CertFile == "" {
			return nil, errors.New("grpc: remote listener requires TLS")
		}
	}
	if cfg.CertFile != "" {
		tlsCredentials, err := credentials.NewServerTLSFromFile(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("grpc TLS: %w", err)
		}
		serverOpts = append(serverOpts, grpc.Creds(tlsCredentials))
	}
	governance := interceptor.Config{RequestTimeout: cfg.RequestTimeout, Authenticator: o.authenticator, Limiter: o.quota, Validate: o.validate, Trusted: o.trusted}
	serverOpts = append(serverOpts, grpc.ChainUnaryInterceptor(interceptor.Unary(service, governance)), grpc.ChainStreamInterceptor(interceptor.Stream(service, governance)))
	server := grpc.NewServer(serverOpts...)
	healthpb.RegisterHealthServer(server, o.state.GRPC())
	if cfg.Reflection {
		reflection.Register(server)
	}
	return &Server{server: server, cfg: cfg}, nil
}

// Register 将业务生成的注册函数接到框架提供的 ServiceRegistrar；须在 Run 前完成。
func (s *Server) Register(register func(grpc.ServiceRegistrar)) { register(s.server) }

// Wrap 将存量 gRPC server 接入生命周期；保持其既有拦截器、认证与健康注册。
func Wrap(server *grpc.Server, addr string) *Server {
	return &Server{server: server, cfg: Config{Addr: addr, Network: "tcp"}}
}

// Listen 同步绑定端口；Unix socket 权限设为 0600，关闭由 listener 清理本次创建的 socket。
func (s *Server) Listen() error {
	if s.cfg.Network == "unix" {
		info, err := os.Stat(filepath.Dir(s.cfg.Addr))
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			return errors.New("grpc: unix socket parent must be private (0700)")
		}
	}
	listener, err := net.Listen(s.cfg.Network, s.cfg.Addr)
	if err != nil {
		return err
	}
	if s.cfg.Network == "unix" {
		if err := os.Chmod(s.cfg.Addr, 0o600); err != nil {
			_ = listener.Close()
			return err
		}
	}
	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()
	return nil
}

// Address 返回实际监听地址。
func (s *Server) Address() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Serve 阻塞至协议关闭。
func (s *Server) Serve() error {
	s.mu.RLock()
	listener := s.listener
	s.mu.RUnlock()
	if listener == nil {
		return errors.New("grpc: not listening")
	}
	err := s.server.Serve(listener)
	if errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}

// Shutdown 在共享预算内优雅排空，超时强制 Stop 并等待 GracefulStop 返回。
func (s *Server) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() { s.server.GracefulStop(); close(done) })
	select {
	case <-done:
		wg.Wait()
		return nil
	case <-ctx.Done():
		s.server.Stop()
		wg.Wait()
		return ctx.Err()
	}
}

// Close 强制停止 server，并关闭已绑定但尚未 Serve 的 listener。
func (s *Server) Close() error {
	s.server.Stop()
	s.mu.RLock()
	listener := s.listener
	s.mu.RUnlock()
	if listener != nil {
		err := listener.Close()
		if !errors.Is(err, net.ErrClosed) {
			return err
		}
	}
	return nil
}

// ToStatus 沿用统一错误映射。
func ToStatus(err error) error { return interceptor.ToStatus(err) }
