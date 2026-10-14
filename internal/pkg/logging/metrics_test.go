package logging_test

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gtkit/gsrv/internal/pkg/logging"
)

func TestMetricsConcurrentAndBounded(t *testing.T) {
	m, err := logging.NewMetrics("test")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			m.Inflight("http", 1)
			m.Observe("http", false, 20*time.Millisecond)
			m.Observe("grpc", true, 2*time.Second)
			m.Reject("grpc", "inflight")
			m.Inflight("http", -1)
		})
	}
	wg.Wait()
	m.Observe("raw-client-url", true, time.Second)
	m.Reject("grpc", "raw-caller-id")
	m.BindNonceStats(func() (int, int, bool) { return 7, 100, true })
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`gsrv_requests_total{service="test",protocol="http"} 100`,
		`gsrv_errors_total{service="test",protocol="http"} 0`,
		`gsrv_errors_total{service="test",protocol="grpc"} 100`,
		`gsrv_inflight{service="test",protocol="http"} 0`,
		`gsrv_limiter_rejections_total{service="test",protocol="grpc",reason="inflight"} 100`,
		`gsrv_request_duration_seconds_bucket{service="test",protocol="http",le="0.01"} 0`,
		`gsrv_request_duration_seconds_bucket{service="test",protocol="http",le="0.025"} 100`,
		`gsrv_request_duration_seconds_bucket{service="test",protocol="grpc",le="+Inf"} 100`,
		`gsrv_request_duration_seconds_count{service="test",protocol="grpc"} 100`,
		`gsrv_request_duration_seconds_sum{service="test",protocol="grpc"} 200`,
		`gsrv_nonce_entries{service="test"} 7`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing sample %s", want)
		}
	}
	if strings.Contains(body, "raw-") || !strings.HasSuffix(body, "\n") || rec.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatal("invalid exposition or unbounded labels")
	}
}

func TestMetricsRegistrationAndDisabled(t *testing.T) {
	var disabled *logging.Metrics
	disabled.Observe("http", false, time.Second)
	disabled.Inflight("http", 1)
	disabled.Reject("http", "qps")
	disabled.BindNonceStats(nil)
	for _, name := range []string{"", "raw\nname", strings.Repeat("a", 65)} {
		if _, err := logging.NewMetrics(name); err == nil {
			t.Fatal("unsafe service accepted")
		}
	}
	m, _ := logging.NewMetrics("test")
	if err := m.RegisterBreaker("unsafe\nname", func() int { return 0 }); err == nil {
		t.Fatal("unsafe breaker accepted")
	}
	for i := range 64 {
		if err := m.RegisterBreaker(fmt.Sprintf("dep-%d", i), func() int { return 0 }); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"dep-0", "extra"} {
		if err := m.RegisterBreaker(name, func() int { return 0 }); err == nil {
			t.Fatal("duplicate/overflow accepted")
		}
	}
	m.BindNonceStats(func() (int, int, bool) { return 0, 0, false })
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(rec.Body.String(), "gsrv_nonce_entries") {
		t.Fatal("unknown occupancy exported")
	}
}
