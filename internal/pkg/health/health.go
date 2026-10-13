// Package health 提供 HTTP/gRPC 共用的生命周期和就绪探测。
package health

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// Check 在 ctx 截止前完成必需依赖探测。
type Check func(context.Context) error

// State 的生命周期标志和 gRPC 健康状态可并发访问；检查函数自行保证并发安全。
type State struct {
	desired  atomic.Bool
	statusMu sync.Mutex
	mu       sync.RWMutex
	checks   map[string]Check
	grpc     *health.Server
}

// New 构造未就绪状态；检查超时固定为每项 1s，监视间隔 1s。
func New() *State { return FromGRPC(health.NewServer()) }

// FromGRPC 将已有 gRPC Health server 纳入生命周期；构造时置 NOT_SERVING。
func FromGRPC(server *health.Server) *State {
	s := &State{checks: make(map[string]Check), grpc: server}
	s.SetReady(false)
	return s
}

// GRPC 返回应注册到协议服务的标准 Health server。
func (s *State) GRPC() *health.Server { return s.grpc }

// AddCheck 注册唯一名称的关键依赖。调用方在启动前完成注册。
func (s *State) AddCheck(name string, check Check) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" || check == nil {
		return errors.New("health: name and check required")
	}
	if _, ok := s.checks[name]; ok {
		return errors.New("health: duplicate check")
	}
	s.checks[name] = check
	return nil
}

// SetReady 设置生命周期就绪标志；关闭状态立即传播至标准 gRPC Health。
func (s *State) SetReady(ready bool) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.desired.Store(ready)
	if !ready {
		s.grpc.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	}
}

// Ready 根据生命周期和全部必需依赖返回就绪状态，不暴露底层错误。
func (s *State) Ready(ctx context.Context) bool {
	if !s.desired.Load() || ctx.Err() != nil {
		return false
	}
	s.mu.RLock()
	checks := make([]Check, 0, len(s.checks))
	for _, check := range s.checks {
		checks = append(checks, check)
	}
	s.mu.RUnlock()
	for _, check := range checks {
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		err := check(probeCtx)
		contextErr := probeCtx.Err()
		cancel()
		if err != nil || contextErr != nil {
			return false
		}
	}
	return s.desired.Load() && ctx.Err() == nil
}

// Monitor 将必需依赖检查结果定期同步到 gRPC Health；由 App 启动并等待退出。
func (s *State) Monitor(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		s.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Refresh 完成一次依赖探测并同步 gRPC Health，关闭状态优先于探测结果。
func (s *State) Refresh(ctx context.Context) {
	ready := s.Ready(ctx)
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	status := healthpb.HealthCheckResponse_NOT_SERVING
	if ready && s.desired.Load() {
		status = healthpb.HealthCheckResponse_SERVING
	}
	s.grpc.SetServingStatus("", status)
}

// Handler 提供 GET /livez 和 GET /readyz；不包含依赖地址或错误详情。
func (s *State) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.Ready(r.Context()) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
