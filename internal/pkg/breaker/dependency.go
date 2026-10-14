package breaker

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/gtkit/gsrv/internal/pkg/auth"
	"github.com/gtkit/gsrv/internal/pkg/fault"
	"github.com/gtkit/gsrv/internal/pkg/limiter"
	"github.com/gtkit/gsrv/internal/pkg/logging"
	request "github.com/gtkit/gsrv/internal/pkg/requestid"
)

// DependencyConfig 配置一个下游的整体调用预算与共享准入，不包含业务重试或降级。
type DependencyConfig struct {
	Timeout        time.Duration `yaml:"timeout"`         // 整体调用（含stream）预算；0/缺省=3s，必须>0，不延长caller deadline。
	MaxInflight    int           `yaml:"max_inflight"`    // HTTP/gRPC合计在途调用；0/缺省=100，必须>0，满时立即拒绝。
	CircuitBreaker bool          `yaml:"circuit_breaker"` // 是否统计并拒绝故障下游调用；缺省false，启用沿用现有breaker默认值。
	Breaker        Config        `yaml:"-"`               // 代码装配时可提供失败分类与阈值；不从YAML加载，名称/指标由Dependency统一持有。
}

// Normalize 返回已校验默认值；长期stream需显式配置更长预算。
func (cfg DependencyConfig) Normalize() (DependencyConfig, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = 3 * time.Second
	}
	if cfg.MaxInflight == 0 {
		cfg.MaxInflight = 100
	}
	if cfg.Timeout <= 0 || cfg.MaxInflight <= 0 {
		return cfg, errors.New("dependency: timeout and max_inflight must be positive")
	}
	return cfg, nil
}

// Dependency 由runtime按固定下游名称创建；两协议共享实例，调用结束时才归还容量。
type Dependency struct {
	name    string
	timeout time.Duration
	gate    *limiter.Inflight
	breaker *gobreaker.TwoStepCircuitBreaker[struct{}]
	metrics *logging.Metrics
}

// NewDependency 不发请求；熔断缺省关闭，最多注册64个指标依赖名称。
func NewDependency(name string, cfg DependencyConfig, metrics *logging.Metrics) (*Dependency, error) {
	cfg, err := cfg.Normalize()
	if err != nil || !auth.ValidName(name) {
		return nil, errors.New("dependency: invalid name or protection settings")
	}
	gate, err := limiter.NewInflight(cfg.MaxInflight)
	if err != nil {
		return nil, err
	}
	d := &Dependency{name: name, timeout: cfg.Timeout, gate: gate, metrics: metrics}
	if cfg.CircuitBreaker {
		settings := cfg.Breaker
		settings.Name = name
		if settings.IsSuccessful == nil {
			settings.IsSuccessful = func(err error) bool {
				switch fault.CodeOf(err) {
				case fault.Unavailable, fault.DeadlineExceeded, fault.ResourceExhausted, fault.Internal:
					return false
				default:
					return true
				}
			}
		}
		st, err := breakerSettings(settings)
		if err != nil {
			return nil, err
		}
		d.breaker = gobreaker.NewTwoStepCircuitBreaker[struct{}](st)
	}
	if metrics != nil {
		if err := metrics.RegisterDependency(name); err != nil {
			return nil, err
		}
		if d.breaker != nil {
			if err := metrics.RegisterBreaker(name, func() int { return int(d.breaker.State()) }); err != nil {
				return nil, err
			}
		}
	}
	return d, nil
}

// Name 返回构造期确定的下游名称，零值返回空串。
func (d *Dependency) Name() string { return d.name }

// Timeout 返回整体预算；HTTP client超时与此保持一致。
func (d *Dependency) Timeout() time.Duration { return d.timeout }

// Begin 返回调用ctx与必须执行的finish；finish可重复调用，但只计量和释放一次。
// 返回错误时没有占用槽位。不排队、不重试、不执行降级；操作必须遵守ctx。
func (d *Dependency) Begin(ctx context.Context, protocol, appKey string) (context.Context, func(error) error, error) {
	if d == nil || d.gate == nil || ctx == nil || !auth.ValidName(appKey) || (protocol != "http" && protocol != "grpc") {
		return nil, nil, fault.New(fault.InvalidArgument, "invalid dependency call")
	}
	if request.FromContext(ctx) == nil {
		ctx = request.WithScope(ctx, request.New(""))
	}
	started := time.Now()
	callCtx, cancel := context.WithTimeout(ctx, d.timeout)
	reject := func(err error, reason string) (context.Context, func(error) error, error) {
		cancel()
		d.metrics.RejectDependency(d.name, protocol, reason)
		d.observe(ctx, protocol, appKey, err, time.Since(started))
		return nil, nil, err
	}
	if err := callCtx.Err(); err != nil {
		return reject(err, "")
	}
	if !d.gate.Acquire() {
		return reject(fault.New(fault.ResourceExhausted, "downstream concurrency exhausted"), "inflight")
	}
	var report func(error)
	if d.breaker != nil {
		var err error
		report, err = d.breaker.Allow()
		if err != nil {
			d.gate.Release()
			return reject(fault.Wrap(fault.Unavailable, "downstream circuit open", err), "breaker")
		}
	}
	d.metrics.DependencyInflight(d.name, protocol, 1)
	var once sync.Once
	var result error
	finish := func(err error) error {
		once.Do(func() {
			defer cancel()
			defer d.gate.Release()
			defer d.metrics.DependencyInflight(d.name, protocol, -1)
			result = err
			if callCtx.Err() != nil {
				result = callCtx.Err()
			}
			if report != nil {
				report(result)
			}
			d.observe(callCtx, protocol, appKey, result, time.Since(started))
		})
		return result
	}
	return callCtx, finish, nil
}

func (d *Dependency) observe(ctx context.Context, protocol, appKey string, err error, duration time.Duration) {
	code := fault.CodeOf(err)
	d.metrics.ObserveDependency(d.name, protocol, string(code), duration)
	logging.Outbound(ctx, d.name, protocol, appKey, string(code), duration)
}
