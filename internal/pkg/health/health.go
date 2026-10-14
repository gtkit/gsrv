// Package health 提供 HTTP/gRPC 共用的生命周期和就绪快照。
package health

import (
	"context"
	"errors"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gtkit/logger/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// Check 必须在 ctx 截止前完成关键依赖探测。
type Check func(context.Context) error

// State 提供可并发访问的共享快照；Check 在未就绪的装配阶段注册。
type State struct {
	desired    atomic.Bool
	ready      atomic.Bool
	statusMu   sync.Mutex
	generation uint64
	mu         sync.RWMutex
	checks     map[string]Check
	probeSlot  chan struct{}
	grpc       *health.Server
}

// New 构造未就绪状态；每项检查 ctx 上限 1s，刷新间隔 1s。
func New() *State { return FromGRPC(health.NewServer()) }

// FromGRPC 接入已有标准 Health server；nil 使用新实例，初始置 NOT_SERVING。
func FromGRPC(server *health.Server) *State {
	if server == nil {
		server = health.NewServer()
	}
	s := &State{checks: make(map[string]Check), probeSlot: make(chan struct{}, 1), grpc: server}
	s.SetReady(false)
	return s
}

// GRPC 返回应注册的标准 Health server；零值 State 返回 nil，协议构造方须拒绝。
func (s *State) GRPC() *health.Server { return s.grpc }

// AddCheck 注册唯一名称的关键依赖；生命周期已激活时拒绝修改检查集合。
func (s *State) AddCheck(name string, check Check) error {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if s.desired.Load() {
		return errors.New("health: register checks before readiness starts")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" || check == nil {
		return errors.New("health: name and check required")
	}
	if _, exists := s.checks[name]; exists {
		return errors.New("health: duplicate check")
	}
	s.checks[name] = check
	return nil
}

// SetReady 激活生命周期或立即清空快照；重复 true 不抹掉已完成的探测。
// 有关键检查时，激活后需 Refresh 才能就绪。
func (s *State) SetReady(ready bool) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if ready && s.desired.Load() {
		return
	}
	if s.desired.Load() != ready {
		s.generation++
	}
	s.desired.Store(ready)
	s.mu.RLock()
	noChecks := len(s.checks) == 0
	s.mu.RUnlock()
	s.publish(ready && noChecks)
}

// Ready 只读取快照，不执行依赖检查；调用上下文取消时返回 false。
func (s *State) Ready(ctx context.Context) bool { return ctx.Err() == nil && s.ready.Load() }

func (s *State) probe(ctx context.Context) (ready bool) {
	defer func() {
		if recover() != nil {
			logger.ErrorCtx(ctx, "health probe panic recovered", zap.ByteString("stack", debug.Stack()))
			ready = false
		}
	}()
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
		// 即使 Check panic 也释放本次计时器。
		ok := func() bool {
			defer cancel()
			return check(probeCtx) == nil && probeCtx.Err() == nil
		}()
		if !ok {
			return false
		}
	}
	return ctx.Err() == nil
}

// Refresh 可取消地串行探测并发布快照；旧生命周期的结果不得覆盖新生命周期。
func (s *State) Refresh(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	select {
	case s.probeSlot <- struct{}{}:
		defer func() { <-s.probeSlot }()
	case <-ctx.Done():
		return
	}
	s.statusMu.Lock()
	generation := s.generation
	s.statusMu.Unlock()
	ready := s.probe(ctx)
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if generation != s.generation {
		return
	}
	s.publish(ready && s.desired.Load())
}

// publish 由持有 statusMu 的调用方执行。
func (s *State) publish(ready bool) {
	s.ready.Store(ready)
	status := healthpb.HealthCheckResponse_NOT_SERVING
	if ready {
		status = healthpb.HealthCheckResponse_SERVING
	}
	s.grpc.SetServingStatus("", status)
}

// Monitor 由 App 在首次 Refresh 后启动，每秒更新缓存并在 ctx 取消后退出。
func (s *State) Monitor(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Refresh(ctx)
		}
	}
}

// Handler 提供 GET /livez 与只读快照的 GET /readyz，不暴露依赖详情。
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
