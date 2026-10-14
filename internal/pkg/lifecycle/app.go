// Package lifecycle 提供显式装配的 HTTP/gRPC 进程生命周期，不包含业务规则。
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gtkit/gsrv/internal/pkg/health"
)

// Server 由协议适配器实现。Listen 同步绑定；Serve 阻塞至关闭；Shutdown 必须遵守 ctx。
// Close 是强制停止兜底，并须允许 Listen 失败后调用。
type Server interface {
	Listen() error
	Serve() error
	Shutdown(context.Context) error
	Close() error
}

// Hook 是显式注册的后台组件；Stop 必须遵守 ctx，即使 Start 部分失败也须释放资源。
type Hook struct {
	_     struct{} // 强制外部使用带键字面量。
	Name  string
	Start func(context.Context) error
	Stop  func(context.Context) error
}

// Option 配置进程生命周期。
type Option func(*App)

// App 只运行一次；构造阶段不启动 goroutine。
type App struct {
	name             string
	servers          []Server
	hooks            []Hook
	state            *health.State
	healthConfigured bool
	timeout          time.Duration
	running          atomic.Bool
}

// WithServers 按依赖顺序注册服务器；关闭顺序与参数顺序一致（如 HTTP → gRPC）。
func WithServers(servers ...Server) Option {
	return func(a *App) { a.servers = append(a.servers, servers...) }
}

// WithHealth 使用同一份 HTTP/gRPC 就绪状态。
func WithHealth(state *health.State) Option {
	return func(a *App) { a.state, a.healthConfigured = state, true }
}

// WithShutdownTimeout 设置全局排空预算，默认 20s；必须为正值。
func WithShutdownTimeout(timeout time.Duration) Option { return func(a *App) { a.timeout = timeout } }

// WithHook 注册后台组件；服务器排空后按逆序停止组件。
func WithHook(hook Hook) Option { return func(a *App) { a.hooks = append(a.hooks, hook) } }

// New 创建进程，不绑定端口。name 必填，至少注册一个 Server。
func New(name string, options ...Option) (*App, error) {
	a := &App{name: name, timeout: 20 * time.Second}
	for _, option := range options {
		option(a)
	}
	if name == "" || len(a.servers) == 0 || a.healthConfigured && (a.state == nil || a.state.GRPC() == nil) || a.timeout <= 0 {
		return nil, errors.New("gsrv: name, servers, health and positive shutdown timeout are required")
	}
	for _, server := range a.servers {
		if nilServer(server) {
			return nil, errors.New("gsrv: nil server")
		}
		if provider, ok := server.(interface{ HealthState() *health.State }); ok {
			state := provider.HealthState()
			if state == nil {
				continue
			} // 旧 Wrap 的既有健康注册由调用方负责。
			if state.GRPC() == nil {
				return nil, errors.New("gsrv: uninitialized server health state")
			}
			if a.state == nil {
				a.state = state
			} else if a.state != state {
				return nil, errors.New("gsrv: servers and App must share one health state")
			}
		}
	}
	if a.state == nil {
		a.state = health.New()
	}
	for _, hook := range a.hooks {
		if hook.Name == "" || hook.Start == nil || hook.Stop == nil {
			return nil, errors.New("gsrv: hook requires name, Start and Stop")
		}
	}
	return a, nil
}

// 仅在构造时检查 interface 中的 typed-nil，不引入请求热路径反射。
func nilServer(server Server) bool {
	if server == nil {
		return true
	}
	value := reflect.ValueOf(server)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// RunSignals 在调用方 ctx 基础上接收 SIGINT/SIGTERM；信号由本次运行独占管理。
func (a *App) RunSignals(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return a.Run(ctx)
}

// Run 绑定全部监听、启动 hooks，再进入服务；取消或任一监听退出均执行统一回收。
// 同一个 App 的后续或并发 Run 返回错误。
func (a *App) Run(ctx context.Context) (result error) {
	if !a.running.CompareAndSwap(false, true) {
		return errors.New("gsrv: App already ran")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	active := make([]Server, 0, len(a.servers))
	started := make([]Hook, 0, len(a.hooks))
	workerCtx, stopWorkers := context.WithCancel(context.WithoutCancel(ctx))
	stopStartup := context.AfterFunc(ctx, stopWorkers)
	defer stopStartup()
	monitorCtx, stopMonitor := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	defer func() {
		a.state.SetReady(false)
		stopMonitor()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.timeout)
		defer cancel()
		for _, server := range active {
			result = errors.Join(result, server.Shutdown(shutdownCtx))
			result = errors.Join(result, server.Close())
		}
		stopWorkers()
		for _, s := range slices.Backward(started) {
			if err := s.Stop(shutdownCtx); err != nil {
				result = errors.Join(result, fmt.Errorf("stop %s: %w", s.Name, err))
			}
		}
		wg.Wait()
	}()
	for _, server := range a.servers {
		active = append(active, server)
		if err := server.Listen(); err != nil {
			return fmt.Errorf("listen: %w", err)
		}
	}
	for _, hook := range a.hooks {
		started = append(started, hook)
		if err := hook.Start(workerCtx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("start %s: %w", hook.Name, err)
		}
	}
	stopStartup()
	if err := ctx.Err(); err != nil {
		return nil
	}
	exited := make(chan error, len(active))
	for _, server := range active {
		wg.Go(func() { exited <- server.Serve() })
	}
	a.state.SetReady(true)
	a.state.Refresh(ctx)
	wg.Go(func() { a.state.Monitor(monitorCtx) })
	select {
	case <-ctx.Done():
		return nil
	case err := <-exited:
		if err == nil {
			err = errors.New("server exited without cancellation")
		}
		return fmt.Errorf("%s serve: %w", a.name, err)
	}
}
