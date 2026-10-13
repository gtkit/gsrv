// Package http 以 net/http 提供服务监听和请求治理。
package http

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/gtkit/gsrv/internal/pkg/auth"
	"github.com/gtkit/gsrv/internal/pkg/fault"
	"github.com/gtkit/gsrv/internal/pkg/health"
	"github.com/gtkit/gsrv/internal/pkg/limiter"
	"github.com/gtkit/gsrv/internal/pkg/logging"
	request "github.com/gtkit/gsrv/internal/pkg/requestid"
	json "github.com/gtkit/json/v2"
	"github.com/gtkit/logger/v2"
	"go.uber.org/zap"
)

// Authenticator 在 HTTP body 解码前返回调用方身份。
type Authenticator interface {
	AuthenticateHTTP(*http.Request) (auth.Identity, error)
}

// Config 的零值采用 loopback 地址和有界超时；非 loopback 监听必须配置 TLS。
type Config struct {
	_                 struct{}      // 强制外部使用带键字面量。
	Addr              string        `yaml:"addr"`                // 监听地址；空值 127.0.0.1:8080；跨主机需 TLS。
	CertFile          string        `yaml:"cert_file"`           // TLS 证书路径；与 key_file 成对；loopback 可留空。
	KeyFile           string        `yaml:"key_file"`            // TLS 私钥路径；不入库；与 cert_file 成对。
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"` // 读取请求头上限；0=5s；不得为负。
	ReadTimeout       time.Duration `yaml:"read_timeout"`        // 读取完整请求上限；0=10s；不得为负。
	WriteTimeout      time.Duration `yaml:"write_timeout"`       // 响应写入上限；0=15s；须覆盖 request_timeout。
	IdleTimeout       time.Duration `yaml:"idle_timeout"`        // keep-alive 空闲时间；0=60s；不得为负。
	RequestTimeout    time.Duration `yaml:"request_timeout"`     // 处理上下文预算；0=3s；handler 必须遵守 ctx。
	MaxBodyBytes      int64         `yaml:"max_body_bytes"`      // 请求体字节上限；0=1MiB；大文件应走专用上传策略。
	MaxInflight       int           `yaml:"max_inflight"`        // 同时处理请求上限；0=100；超限立即拒绝。
}

// Server 实现 gsrv.Server，包装阶段不绑定端口。
type Server struct {
	server   *http.Server
	cfg      Config
	mu       sync.RWMutex
	listener net.Listener
}

// New 组合探针和受保护 handler；业务 handler/authenticator 均须非 nil。
func New(service string, cfg Config, handler http.Handler, authenticator Authenticator, quota limiter.Limiter, state *health.State) (*Server, error) {
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:8080"
	}
	if cfg.ReadHeaderTimeout == 0 {
		cfg.ReadHeaderTimeout = 5 * time.Second
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 10 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 15 * time.Second
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 60 * time.Second
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 3 * time.Second
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = 1 << 20
	}
	if cfg.MaxInflight == 0 {
		cfg.MaxInflight = 100
	}
	if !auth.ValidName(service) || handler == nil || authenticator == nil || state == nil || cfg.ReadHeaderTimeout <= 0 ||
		cfg.ReadTimeout <= 0 || cfg.WriteTimeout < cfg.RequestTimeout || cfg.IdleTimeout <= 0 || cfg.RequestTimeout <= 0 || cfg.MaxBodyBytes <= 0 || cfg.MaxInflight <= 0 {
		return nil, errors.New("http: invalid service, handlers or limits")
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("http address: %w", err)
	}
	ip := net.ParseIP(host)
	local := host == "localhost" || (ip != nil && ip.IsLoopback())
	if (cfg.CertFile == "") != (cfg.KeyFile == "") || !local && cfg.CertFile == "" {
		return nil, errors.New("http: TLS pair required for non-loopback listener")
	}
	slots := make(chan struct{}, cfg.MaxInflight)
	business := http.MaxBytesHandler(handler, cfg.MaxBodyBytes)
	governed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scope := request.New(r.Header.Get("X-Request-Id"))
		ctx := request.WithScope(r.Context(), scope)
		r = r.WithContext(ctx)
		w.Header().Set("X-Request-Id", scope.ID())
		record := &writer{ResponseWriter: w, status: 200, code: string(fault.OK)}
		start := time.Now()
		defer func() {
			operation := r.Pattern
			if operation == "" {
				operation = r.Method + " " + r.URL.EscapedPath()
			}
			logging.Access(ctx, service, "http", operation, record.code, record.status, time.Since(start))
		}()
		defer func() {
			if recover() != nil {
				logger.ErrorCtx(ctx, "http panic recovered", zap.ByteString("stack", debug.Stack()))
				if !record.wrote {
					WriteError(record, r, fault.New(fault.Internal, "internal server error"))
				} else {
					record.code = string(fault.Internal)
				}
			}
		}()
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			WriteError(record, r, fault.New(fault.ResourceExhausted, "too many in-flight requests"))
			return
		}
		ctx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		if r.Body != nil {
			r.Body = http.MaxBytesReader(record, r.Body, cfg.MaxBodyBytes)
		}
		identity, err := authenticator.AuthenticateHTTP(r)

		if err != nil {
			WriteError(record, r, err)
			return
		}
		scope.Authenticate(identity.AppKey)
		r = r.WithContext(auth.WithIdentity(ctx, identity))
		if quota != nil {
			allowed, err := quota.Allow(ctx, identity.AppKey)
			if err != nil {
				WriteError(record, r, fault.Wrap(fault.Unavailable, "limiter unavailable", err))
				return
			}
			if !allowed {
				record.Header().Set("Retry-After", "1")
				WriteError(record, r, fault.New(fault.ResourceExhausted, "rate limit exceeded"))
				return
			}
		}
		business.ServeHTTP(record, r)
		if record.status >= 400 && record.code == string(fault.OK) {
			record.code = fmt.Sprintf("HTTP_%d", record.status)
		}
	})
	probes := state.Handler()
	router := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && (r.URL.Path == "/livez" || r.URL.Path == "/readyz") {
			probes.ServeHTTP(w, r)
			return
		}
		governed.ServeHTTP(w, r)
	})
	server := &http.Server{Addr: cfg.Addr, Handler: router, ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout: cfg.ReadTimeout, WriteTimeout: cfg.WriteTimeout, IdleTimeout: cfg.IdleTimeout, MaxHeaderBytes: 1 << 20}
	if cfg.CertFile != "" {
		certificate, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("http TLS: %w", err)
		}
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	}
	return &Server{server: server, cfg: cfg}, nil
}

// Wrap 将已有 HTTP server 接入公共生命周期；鉴权和限制由已有装配方负责。
func Wrap(server *http.Server) *Server { return &Server{server: server} }

// ServeHTTP 通过同一治理链处理请求，可用于组合 handler 或无需监听的协议测试。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.server.Handler.ServeHTTP(w, r)
}

// Listen 同步绑定地址；绑定后 Address 可用于测试或静态发现。
func (s *Server) Listen() error {
	listener, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()
	return nil
}

// Address 返回实际监听地址，监听前为空。
func (s *Server) Address() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Serve 阻塞服务，正常关闭返回 nil。
func (s *Server) Serve() error {
	s.mu.RLock()
	listener := s.listener
	s.mu.RUnlock()
	if listener == nil {
		return errors.New("http: not listening")
	}
	var err error
	if s.cfg.CertFile != "" {
		err = s.server.ServeTLS(listener, "", "")
	} else {
		err = s.server.Serve(listener)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown 等待在途请求；超时后强制关闭连接。
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.server.Shutdown(ctx)
	if err != nil {
		return errors.Join(err, s.Close())
	}
	return nil
}

// Close 强制关闭监听和活动连接。
func (s *Server) Close() error {
	err := s.server.Close()
	s.mu.RLock()
	listener := s.listener
	s.mu.RUnlock()
	if listener != nil {
		closeErr := listener.Close()
		if !errors.Is(closeErr, net.ErrClosed) {
			err = errors.Join(err, closeErr)
		}
	}
	return err
}

// WriteError 输出公共安全错误；业务 handler 可先将领域 sentinel 翻译为 fault。
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	code := fault.CodeOf(err)
	status := http.StatusInternalServerError
	switch code {
	case fault.NotFound:
		status = 404
	case fault.PayloadTooLarge:
		status = 413
	case fault.InvalidArgument:
		status = 400
	case fault.Unauthenticated:
		status = 401
	case fault.PermissionDenied:
		status = 403
	case fault.ResourceExhausted:
		status = 429
	case fault.Unavailable:
		status = 503
	case fault.DeadlineExceeded:
		status = 504
	case fault.Canceled:
		status = 408
	}
	if record, ok := w.(*writer); ok {
		record.code = string(code)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(struct {
		Code    fault.Code `json:"code"`
		Message string     `json:"message"`
	}{code, fault.Message(err)})
	_, _ = w.Write(body) // 连接已关闭时无法补发；访问日志仍记录最终处理结果。
}

type writer struct {
	http.ResponseWriter
	status int
	code   string
	wrote  bool
}

func (w *writer) WriteHeader(status int) {
	if w.wrote {
		return
	}
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.status = status
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}
func (w *writer) Write(body []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(body)
}
func (w *writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *writer) Flush() {
	if !w.wrote {
		w.WriteHeader(200)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
