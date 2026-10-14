package logging

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

var latencyBuckets = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type protocolMetrics struct {
	requests, errors uint64
	inflight         int64
	rejected         [2]uint64
	buckets          [len(latencyBuckets)]uint64
	seconds          float64
}

// Metrics 是服务显式持有的最小指标集，无全局注册或动态请求标签。
type Metrics struct {
	mu           sync.Mutex
	service      string
	protocols    [2]protocolMetrics
	nonceStats   func() (int, int, bool)
	breakers     map[string]func() int
	dependencies map[string][2]outboundCounters
}

// NewMetrics 构造指标，service 为1–64位安全ASCII名称；不启动监听或后台任务。
func NewMetrics(service string) (*Metrics, error) {
	if !metricName(service) {
		return nil, errors.New("metrics: invalid service name")
	}
	return &Metrics{service: service, breakers: make(map[string]func() int), dependencies: make(map[string][2]outboundCounters)}, nil
}

func metricName(name string) bool {
	return len(name) > 0 && len(name) <= 64 && strings.IndexFunc(name, func(c rune) bool {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			return false
		default:
			return true
		}
	}) < 0
}

func protocolIndex(protocol string) int {
	switch protocol {
	case "http":
		return 0
	case "grpc":
		return 1
	default:
		return -1
	}
}

// Observe 记录进入治理链的请求最终结果与耗时；protocol仅接受http/grpc，nil接收者关闭记录。
func (m *Metrics) Observe(protocol string, failed bool, duration time.Duration) {
	i := protocolIndex(protocol)
	if m == nil || i < 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p := &m.protocols[i]
	p.requests++
	if failed {
		p.errors++
	}
	seconds := max(0, duration.Seconds())
	p.seconds += seconds
	for i, bound := range latencyBuckets {
		if seconds <= bound {
			p.buckets[i]++
		}
	}
}

// Inflight 由admission/release记录已获槽位的增减，nil接收者为空操作。
func (m *Metrics) Inflight(protocol string, delta int64) {
	i := protocolIndex(protocol)
	if m == nil || i < 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.protocols[i].inflight += delta
}

// Reject 仅记录qps/inflight保护拒绝，nil接收者为空操作。
func (m *Metrics) Reject(protocol, reason string) {
	i := protocolIndex(protocol)
	if m == nil || i < 0 {
		return
	}
	index := 0
	switch reason {
	case "qps":
	case "inflight":
		index = 1
	default:
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.protocols[i].rejected[index]++
}

// BindNonceStats 绑定快速的内存占用来源，available=false时不导出该指标，不执行外部I/O。
func (m *Metrics) BindNonceStats(stats func() (int, int, bool)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nonceStats = stats
}

// RegisterBreaker 在启动装配时注册固定安全依赖名；唯一且最多64项。
// state须快速返回0=closed、1=half-open、2=open，不执行外部I/O。
func (m *Metrics) RegisterBreaker(name string, state func() int) error {
	if m == nil || !metricName(name) || state == nil {
		return errors.New("metrics: invalid breaker registration")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.breakers[name]; exists {
		return errors.New("metrics: duplicate breaker")
	}
	if len(m.breakers) >= 64 {
		return errors.New("metrics: breaker limit exceeded")
	}
	m.breakers[name] = state
	return nil
}

// ServeHTTP 导出Prometheus 0.0.4；访问控制由HTTP server的本机管理路由负责。
func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	protocols, stats, breakers := m.protocols, m.nonceStats, maps.Clone(m.breakers)
	m.mu.Unlock()
	var body strings.Builder
	service := "service=" + strconv.Quote(m.service)
	for _, metric := range []struct{ name, kind string }{
		{"gsrv_requests_total", "counter"}, {"gsrv_errors_total", "counter"}, {"gsrv_inflight", "gauge"},
		{"gsrv_limiter_rejections_total", "counter"}, {"gsrv_request_duration_seconds", "histogram"},
	} {
		fmt.Fprintf(&body, "# TYPE %s %s\n", metric.name, metric.kind)
		for i, protocol := range [...]string{"http", "grpc"} {
			p := protocols[i]
			labels := service + ",protocol=" + strconv.Quote(protocol)
			switch metric.name {
			case "gsrv_requests_total":
				fmt.Fprintf(&body, "%s{%s} %d\n", metric.name, labels, p.requests)
			case "gsrv_errors_total":
				fmt.Fprintf(&body, "%s{%s} %d\n", metric.name, labels, p.errors)
			case "gsrv_inflight":
				fmt.Fprintf(&body, "%s{%s} %d\n", metric.name, labels, p.inflight)
			case "gsrv_limiter_rejections_total":
				for j, reason := range [...]string{"qps", "inflight"} {
					fmt.Fprintf(&body, "%s{%s,reason=%q} %d\n", metric.name, labels, reason, p.rejected[j])
				}
			case "gsrv_request_duration_seconds":
				for j, bound := range latencyBuckets {
					fmt.Fprintf(&body, "%s_bucket{%s,le=%q} %d\n", metric.name, labels, strconv.FormatFloat(bound, 'g', -1, 64), p.buckets[j])
				}
				fmt.Fprintf(&body, "%s_bucket{%s,le=%q} %d\n", metric.name, labels, "+Inf", p.requests)
				fmt.Fprintf(&body, "%s_sum{%s} %g\n%s_count{%s} %d\n", metric.name, labels, p.seconds, metric.name, labels, p.requests)
			}
		}
	}
	if stats != nil {
		if used, capacity, ok := stats(); ok {
			fmt.Fprintf(&body, "# TYPE gsrv_nonce_entries gauge\ngsrv_nonce_entries{%s} %d\n# TYPE gsrv_nonce_capacity gauge\ngsrv_nonce_capacity{%s} %d\n", service, used, service, capacity)
		}
	}
	fmt.Fprintln(&body, "# TYPE gsrv_breaker_state gauge")
	for _, name := range slices.Sorted(maps.Keys(breakers)) {
		fmt.Fprintf(&body, "gsrv_breaker_state{%s,dependency=%q} %d\n", service, name, breakers[name]())
	}
	m.writeDependencies(&body, service)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(body.String()))
}
