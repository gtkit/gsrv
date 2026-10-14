package logging

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gtkit/logger/v2"
	"go.uber.org/zap"

	"github.com/gtkit/gsrv/internal/pkg/fault"
)

var outboundCodes = [...]string{string(fault.OK), string(fault.NotFound), string(fault.PayloadTooLarge), string(fault.InvalidArgument), string(fault.Unauthenticated), string(fault.PermissionDenied), string(fault.ResourceExhausted), string(fault.Unavailable), string(fault.DeadlineExceeded), string(fault.Canceled), string(fault.Internal)}

type outboundCounters struct {
	protocolMetrics
	codes [len(outboundCodes)]uint64
}

// Outbound 只记录安全的固定目标与结果；AppKey来自本服务出站凭据，不记录URL/body/错误文案。
func Outbound(ctx context.Context, dependency, protocol, appKey, code string, duration time.Duration) {
	fields := []zap.Field{zap.String("direction", "outbound"), zap.String("dependency", dependency), zap.String("protocol", protocol), zap.String("app_key", appKey), zap.String("error_code", code), zap.Float64("duration_ms", float64(duration.Microseconds())/1000)}
	if code == string(fault.OK) {
		logger.InfoCtx(ctx, "dependency call completed", fields...)
	} else {
		logger.WarnCtx(ctx, "dependency call failed", fields...)
	}
}

// RegisterDependency 构造期注册固定安全名称，重复或超过64项报错。
func (m *Metrics) RegisterDependency(name string) error {
	if m == nil || !metricName(m.service) || !metricName(name) {
		return errors.New("metrics: invalid dependency registration")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.dependencies[name]; exists || len(m.dependencies) >= 64 {
		return errors.New("metrics: duplicate dependency or capacity exceeded")
	}
	m.dependencies[name] = [2]outboundCounters{}
	return nil
}

// ObserveDependency 仅统计已注册目标；未知错误码收敛INTERNAL，不接受动态标签。
func (m *Metrics) ObserveDependency(name, protocol, code string, duration time.Duration) {
	if m == nil || protocolIndex(protocol) < 0 {
		return
	}
	index := slices.Index(outboundCodes[:], code)
	if index < 0 {
		index = len(outboundCodes) - 1
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	pair, exists := m.dependencies[name]
	if !exists {
		return
	}
	p := &pair[protocolIndex(protocol)]
	p.requests++
	p.codes[index]++
	if index != 0 {
		p.errors++
	}
	seconds := max(0, duration.Seconds())
	p.seconds += seconds
	for i, bound := range latencyBuckets {
		if seconds <= bound {
			p.buckets[i]++
		}
	}
	m.dependencies[name] = pair
}

// DependencyInflight 统计共享准入后的协议在途数，不创建动态目标。
func (m *Metrics) DependencyInflight(name, protocol string, delta int64) {
	if m == nil || protocolIndex(protocol) < 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if pair, exists := m.dependencies[name]; exists {
		pair[protocolIndex(protocol)].inflight += delta
		m.dependencies[name] = pair
	}
}

// RejectDependency 的reason仅接受inflight/breaker，其余不产生标签。
func (m *Metrics) RejectDependency(name, protocol, reason string) {
	if m == nil || protocolIndex(protocol) < 0 || (reason != "inflight" && reason != "breaker") {
		return
	}
	index := 0
	if reason == "breaker" {
		index = 1
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if pair, exists := m.dependencies[name]; exists {
		pair[protocolIndex(protocol)].rejected[index]++
		m.dependencies[name] = pair
	}
}

func (m *Metrics) writeDependencies(body *strings.Builder, service string) {
	m.mu.Lock()
	dependencies := maps.Clone(m.dependencies)
	m.mu.Unlock()
	for _, metric := range []struct{ name, kind string }{
		{"gsrv_dependency_requests_total", "counter"}, {"gsrv_dependency_errors_total", "counter"}, {"gsrv_dependency_inflight", "gauge"}, {"gsrv_dependency_rejections_total", "counter"}, {"gsrv_dependency_duration_seconds", "histogram"},
	} {
		fmt.Fprintf(body, "# TYPE %s %s\n", metric.name, metric.kind)
		for _, name := range slices.Sorted(maps.Keys(dependencies)) {
			for i, protocol := range [...]string{"http", "grpc"} {
				p := dependencies[name][i]
				labels := service + ",dependency=" + strconv.Quote(name) + ",protocol=" + strconv.Quote(protocol)
				switch metric.name {
				case "gsrv_dependency_requests_total":
					for j, code := range outboundCodes {
						fmt.Fprintf(body, "%s{%s,error_code=%q} %d\n", metric.name, labels, code, p.codes[j])
					}
				case "gsrv_dependency_errors_total":
					fmt.Fprintf(body, "%s{%s} %d\n", metric.name, labels, p.errors)
				case "gsrv_dependency_inflight":
					fmt.Fprintf(body, "%s{%s} %d\n", metric.name, labels, p.inflight)
				case "gsrv_dependency_rejections_total":
					for j, reason := range [...]string{"inflight", "breaker"} {
						fmt.Fprintf(body, "%s{%s,reason=%q} %d\n", metric.name, labels, reason, p.rejected[j])
					}
				case "gsrv_dependency_duration_seconds":
					for j, bound := range latencyBuckets {
						fmt.Fprintf(body, "%s_bucket{%s,le=%q} %d\n", metric.name, labels, strconv.FormatFloat(bound, 'g', -1, 64), p.buckets[j])
					}
					fmt.Fprintf(body, "%s_bucket{%s,le=%q} %d\n%s_sum{%s} %g\n%s_count{%s} %d\n", metric.name, labels, "+Inf", p.requests, metric.name, labels, p.seconds, metric.name, labels, p.requests)
				}
			}
		}
	}
}
