package logging

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOutboundMetricsBoundsAndConcurrentScrapes(t *testing.T) {
	m, err := NewMetrics("caller")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterDependency("peer"); err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterDependency("peer"); err == nil {
		t.Fatal("duplicate accepted")
	}
	if err := m.RegisterDependency("unsafe\nname"); err == nil {
		t.Fatal("unsafe accepted")
	}
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			m.DependencyInflight("peer", "http", 1)
			m.ObserveDependency("peer", "http", "OK", time.Millisecond)
			m.DependencyInflight("peer", "http", -1)
			r := httptest.NewRecorder()
			m.ServeHTTP(r, httptest.NewRequest("GET", "/metrics", nil))
		})
	}
	wg.Wait()
	m.ObserveDependency("peer", "grpc", "arbitrary-secret", time.Millisecond)
	m.ObserveDependency("dynamic-target", "http", "OK", time.Millisecond)
	m.RejectDependency("peer", "grpc", "breaker")
	r := httptest.NewRecorder()
	m.ServeHTTP(r, httptest.NewRequest("GET", "/metrics", nil))
	for _, line := range []string{
		`gsrv_dependency_requests_total{service="caller",dependency="peer",protocol="http",error_code="OK"} 50`,
		`gsrv_dependency_duration_seconds_count{service="caller",dependency="peer",protocol="http"} 50`,
		`gsrv_dependency_duration_seconds_bucket{service="caller",dependency="peer",protocol="http",le="+Inf"} 50`,
		`gsrv_dependency_inflight{service="caller",dependency="peer",protocol="http"} 0`,
		`gsrv_dependency_requests_total{service="caller",dependency="peer",protocol="grpc",error_code="INTERNAL"} 1`,
	} {
		if !strings.Contains(r.Body.String(), line+"\n") {
			t.Fatalf("missing %s", line)
		}
	}
	for _, forbidden := range []string{"arbitrary-secret", "dynamic-target", "app_key=", "request_id=", "url="} {
		if strings.Contains(r.Body.String(), forbidden) {
			t.Fatalf("unbounded label %s", forbidden)
		}
	}
	for i := 1; i < 64; i++ {
		if err := m.RegisterDependency(fmt.Sprintf("peer-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.RegisterDependency("overflow"); err == nil {
		t.Fatal("unbounded dependencies")
	}
}
