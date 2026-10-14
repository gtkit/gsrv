package breaker

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/gtkit/gsrv/internal/pkg/fault"
	"github.com/gtkit/gsrv/internal/pkg/logging"
)

func TestDependencySharedConcurrencyAndFinish(t *testing.T) {
	m, err := logging.NewMetrics("caller")
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDependency("peer", DependencyConfig{MaxInflight: 1}, m)
	if err != nil {
		t.Fatal(err)
	}
	ctx, finish, err := d.Begin(t.Context(), "http", "service-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Begin(t.Context(), "grpc", "service-a"); fault.CodeOf(err) != fault.ResourceExhausted {
		t.Fatalf("shared capacity err=%v", err)
	}
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if err := finish(nil); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if ctx.Err() == nil {
		t.Fatal("finished budget still active")
	}
	_, end, err := d.Begin(t.Context(), "grpc", "service-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := end(nil); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	m.ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	for _, line := range []string{
		`gsrv_dependency_inflight{service="caller",dependency="peer",protocol="http"} 0`,
		`gsrv_dependency_inflight{service="caller",dependency="peer",protocol="grpc"} 0`,
		`gsrv_dependency_requests_total{service="caller",dependency="peer",protocol="http",error_code="OK"} 1`,
		`gsrv_dependency_rejections_total{service="caller",dependency="peer",protocol="grpc",reason="inflight"} 1`,
	} {
		if !strings.Contains(recorder.Body.String(), line+"\n") {
			t.Fatalf("missing metric %s", line)
		}
	}
}

func TestDependencyCancellationAndBreakerClassification(t *testing.T) {
	d, err := NewDependency("peer", DependencyConfig{MaxInflight: 1, CircuitBreaker: true, Breaker: Config{MinRequests: 2, FailureRatio: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, finish, err := d.Begin(ctx, "http", "a")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := finish(nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	for _, code := range []fault.Code{fault.InvalidArgument, fault.Unauthenticated, fault.PermissionDenied, fault.NotFound} {
		_, end, err := d.Begin(t.Context(), "grpc", "a")
		if err != nil {
			t.Fatal(err)
		}
		if err := end(fault.New(code, "safe")); fault.CodeOf(err) != code {
			t.Fatalf("finish code=%v", err)
		}
	}
	if d.breaker.State() != gobreaker.StateClosed {
		t.Fatal("caller errors opened breaker")
	}
	for range 2 {
		_, end, err := d.Begin(t.Context(), "http", "a")
		if err != nil {
			t.Fatal(err)
		}
		if err := end(fault.New(fault.Unavailable, "safe")); fault.CodeOf(err) != fault.Unavailable {
			t.Fatalf("finish failure=%v", err)
		}
	}
	if d.breaker.State() != gobreaker.StateClosed { // NotFound is a successful operational sample.
		t.Fatal("unexpected failure ratio")
	}
	d2, err := NewDependency("failing", DependencyConfig{CircuitBreaker: true, Breaker: Config{MinRequests: 2, FailureRatio: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, end, err := d2.Begin(t.Context(), "http", "a")
		if err != nil {
			t.Fatal(err)
		}
		if err := end(fault.New(fault.Unavailable, "safe")); fault.CodeOf(err) != fault.Unavailable {
			t.Fatalf("finish failure=%v", err)
		}
	}
	if _, _, err := d2.Begin(t.Context(), "grpc", "a"); fault.CodeOf(err) != fault.Unavailable {
		t.Fatalf("breaker rejection=%v", err)
	}
}

func TestDependencyBudgetCannotExtendCaller(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, err := NewDependency("peer", DependencyConfig{Timeout: time.Second, MaxInflight: 1}, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		callCtx, finish, err := d.Begin(ctx, "http", "a")
		if err != nil {
			t.Fatal(err)
		}
		parentDeadline, _ := ctx.Deadline()
		childDeadline, _ := callCtx.Deadline()
		if childDeadline.After(parentDeadline) {
			t.Fatal("caller budget extended")
		}
		synctest.Sleep(100 * time.Millisecond)
		if err := finish(nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("late success accepted=%v", err)
		}
		_, end, err := d.Begin(t.Context(), "grpc", "a")
		if err != nil {
			t.Fatal(err)
		}
		if err := end(nil); err != nil {
			t.Fatal(err)
		}
	})
}

func TestDependencyInvalidAndDeadline(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  DependencyConfig
	}{
		{"unsafe name", DependencyConfig{}}, {"peer", DependencyConfig{Timeout: -time.Second}}, {"peer", DependencyConfig{MaxInflight: -1}},
	} {
		if _, err := NewDependency(tc.name, tc.cfg, nil); err == nil {
			t.Fatalf("accepted %s %+v", tc.name, tc.cfg)
		}
	}
	d, err := NewDependency("peer", DependencyConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Timeout() != 3*time.Second {
		t.Fatal("timeout default")
	}
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if _, _, err := d.Begin(ctx, "http", "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline=%v", err)
	}
	for _, tc := range []struct {
		ctx           context.Context
		protocol, key string
	}{{nil, "http", "a"}, {t.Context(), "invalid", "a"}, {t.Context(), "http", "bad key"}} {
		if _, _, err := d.Begin(tc.ctx, tc.protocol, tc.key); fault.CodeOf(err) != fault.InvalidArgument {
			t.Fatalf("invalid=%v", err)
		}
	}
}

func TestDependencyAttributesBudgetToCallerOrDownstream(t *testing.T) {
	for _, tc := range []struct {
		name            string
		caller, timeout time.Duration
		finishDelay     time.Duration
		state           gobreaker.State
	}{{"caller", time.Second, 2 * time.Second, 0, gobreaker.StateClosed}, {"downstream", 2 * time.Second, time.Second, 0, gobreaker.StateOpen}, {"downstream-finish-after-caller", 2 * time.Second, time.Second, 2 * time.Second, gobreaker.StateOpen}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d, err := NewDependency("peer", DependencyConfig{Timeout: tc.timeout, MaxInflight: 1, CircuitBreaker: true, Breaker: Config{MinRequests: 1}}, nil)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), tc.caller)
				defer cancel()
				callCtx, finish, err := d.Begin(ctx, "grpc", "service-a")
				if err != nil {
					t.Fatal(err)
				}
				<-callCtx.Done()
				if tc.finishDelay > 0 {
					synctest.Sleep(tc.finishDelay)
				}
				if err := finish(nil); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("budget result=%v", err)
				}
				if got := d.breaker.State(); got != tc.state {
					t.Fatalf("budget attributed incorrectly: got=%s want=%s", got, tc.state)
				}
			})
		})
	}
}

func TestNestedDependenciesAttributeInheritedBudgetToParent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, err := NewDependency("parent", DependencyConfig{Timeout: time.Second, CircuitBreaker: true, Breaker: Config{MinRequests: 1}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		child, err := NewDependency("child", DependencyConfig{Timeout: 2 * time.Second, CircuitBreaker: true, Breaker: Config{MinRequests: 1}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, finishParent, err := parent.Begin(t.Context(), "http", "service-a")
		if err != nil {
			t.Fatal(err)
		}
		callCtx, finishChild, err := child.Begin(ctx, "grpc", "service-a")
		if err != nil {
			t.Fatal(err)
		}
		<-callCtx.Done()
		if err := finishChild(nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("child budget=%v", err)
		}
		if err := finishParent(nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("parent budget=%v", err)
		}
		if parent.breaker.State() != gobreaker.StateOpen || child.breaker.State() != gobreaker.StateClosed {
			t.Fatal("inherited budget opened the child breaker or failed to open the parent")
		}
	})
}
