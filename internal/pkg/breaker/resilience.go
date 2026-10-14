// Package breaker 封装依赖调用保护；失败分类和降级结果由业务服务提供。
package breaker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gtkit/httpc"
	"github.com/gtkit/logger/v2"
	"github.com/sony/gobreaker/v2"

	"github.com/gtkit/gsrv/internal/pkg/fault"
	"github.com/gtkit/gsrv/internal/pkg/logging"
	request "github.com/gtkit/gsrv/internal/pkg/requestid"
)

// Config 设置单个下游熔断策略；不得跨不相关依赖共用实例。
type Config struct {
	_                struct{}         // 强制外部使用带键字面量。
	Name             string           // 依赖名称；必填，一个实例保护一个下游依赖，不跨无关依赖共用。
	Interval         time.Duration    // 关闭状态的计数窗口；0=30s，必须为正。
	OpenTimeout      time.Duration    // 熔断开启后转半开的等待时间；0=10s，必须为正。
	MinRequests      uint32           // 至少完成的有效样本数；0=20，不含排除请求与在途请求。
	FailureRatio     float64          // 开启熔断的失败比例阈值；0=0.5，必须在 (0,1]。
	HalfOpenRequests uint32           // 半开状态的并行探测请求上限；0=1。
	IsSuccessful     func(error) bool // 下游成功分类；nil 使用 gobreaker 默认 err==nil，由业务解释领域错误。
	IsExcluded       func(error) bool // 额外排除样本分类；nil 无额外排除，取消/鉴权/参数错误始终排除。
	Metrics          *logging.Metrics // nil不导出状态；启用时Name为固定安全名称，唯一且最多64个依赖。
}

// Breaker 包装一个指定下游的熔断状态。
type Breaker[T any] struct{ breaker *gobreaker.CircuitBreaker[T] }

// New 创建熔断器；默认窗口 30s、开启 10s、至少 20 样本、失败比 0.5、半开 1 请求。
func New[T any](cfg Config) (*Breaker[T], error) {
	if cfg.Interval == 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.OpenTimeout == 0 {
		cfg.OpenTimeout = 10 * time.Second
	}
	if cfg.MinRequests == 0 {
		cfg.MinRequests = 20
	}
	if cfg.FailureRatio == 0 {
		cfg.FailureRatio = 0.5
	}
	if cfg.HalfOpenRequests == 0 {
		cfg.HalfOpenRequests = 1
	}
	if cfg.Name == "" || cfg.Interval <= 0 || cfg.OpenTimeout <= 0 || cfg.FailureRatio <= 0 || cfg.FailureRatio > 1 {
		return nil, errors.New("resilience: invalid breaker settings")
	}
	excluded := func(err error) bool {
		if errors.Is(err, context.Canceled) {
			return true
		}
		code := fault.CodeOf(err)
		if code == fault.InvalidArgument || code == fault.Unauthenticated || code == fault.PermissionDenied {
			return true
		}
		return cfg.IsExcluded != nil && cfg.IsExcluded(err)
	}
	settings := gobreaker.Settings{Name: cfg.Name, Interval: cfg.Interval, Timeout: cfg.OpenTimeout, MaxRequests: cfg.HalfOpenRequests,
		IsSuccessful: cfg.IsSuccessful, IsExcluded: excluded,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			total := counts.TotalSuccesses + counts.TotalFailures
			return total >= cfg.MinRequests && float64(counts.TotalFailures)/float64(total) >= cfg.FailureRatio
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			logger.Warnw("breaker state changed", "dependency", name, "from", from.String(), "to", to.String())
		}}
	b := &Breaker[T]{breaker: gobreaker.NewCircuitBreaker[T](settings)}
	if cfg.Metrics != nil {
		if err := cfg.Metrics.RegisterBreaker(cfg.Name, func() int { return int(b.State()) }); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// Execute 执行合作式可取消调用；不自动重试或降级。
func (b *Breaker[T]) Execute(ctx context.Context, operation func(context.Context) (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if operation == nil {
		return zero, errors.New("resilience: nil operation")
	}
	return b.breaker.Execute(func() (T, error) { return operation(ctx) })
}

// State 返回熔断状态，便于业务诊断。
func (b *Breaker[T]) State() gobreaker.State { return b.breaker.State() }

// Run 执行有预算的依赖调用；fallback 仅在 allow(err) 明确许可时执行，且使用剩余 ctx。
// timeout 必须为正；未提供 fallback 时保留原错误；取消、鉴权和参数错误不降级。
func Run[T any](ctx context.Context, timeout time.Duration, breaker *Breaker[T], operation func(context.Context) (T, error),
	allow func(error) bool, fallback func(context.Context, error) (T, error)) (T, error) {
	var zero T
	if timeout <= 0 || operation == nil {
		return zero, errors.New("resilience: positive timeout and operation required")
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	budgetCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var value T
	var err error
	if breaker == nil {
		value, err = operation(budgetCtx)
	} else {
		value, err = breaker.Execute(budgetCtx, operation)
	}
	if err == nil {
		return value, nil
	}
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	if budgetCtx.Err() != nil {
		return zero, budgetCtx.Err()
	}
	code := fault.CodeOf(err)
	if errors.Is(err, context.Canceled) || code == fault.InvalidArgument || code == fault.Unauthenticated || code == fault.PermissionDenied ||
		fallback == nil || allow == nil || !allow(err) {
		return zero, err
	}
	value, fallbackErr := fallback(budgetCtx, err)
	if fallbackErr != nil {
		return zero, fmt.Errorf("fallback failed: %w", errors.Join(err, fallbackErr))
	}
	if scope := request.FromContext(ctx); scope != nil {
		scope.MarkDegraded()
	}
	return value, nil
}

// NewHTTPClient 创建共享 httpc 连接池，必须显式设置正超时；不增加业务重试。
func NewHTTPClient(timeout time.Duration, options ...httpc.Option) (*httpc.Client, error) {
	if timeout <= 0 {
		return nil, errors.New("resilience: HTTP timeout must be positive")
	}
	options = append(options, httpc.WithTimeout(timeout))
	return httpc.New(options...), nil
}
