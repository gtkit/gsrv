package health_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/gtkit/gsrv/internal/pkg/health"
)

func TestReadinessAndLiveness(t *testing.T) {
	state := health.New()
	if state.Ready(t.Context()) {
		t.Fatal("ready before startup")
	}
	if err := state.AddCheck("db", func(context.Context) error { return errors.New("private address") }); err != nil {
		t.Fatal(err)
	}
	state.SetReady(true)
	for _, tc := range []struct {
		path   string
		status int
	}{{"/livez", 200}, {"/readyz", 503}} {
		rec := httptest.NewRecorder()
		state.Handler().ServeHTTP(rec, httptest.NewRequest("GET", tc.path, nil))
		if rec.Code != tc.status || rec.Body.Len() != 0 {
			t.Fatal("probe response exposes detail or wrong status")
		}
	}
	state.Refresh(t.Context())
	response, err := state.GRPC().Check(t.Context(), &healthpb.HealthCheckRequest{})
	if err != nil || response.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatal("grpc disagrees")
	}
	if err := state.AddCheck("db", func(context.Context) error { return nil }); err == nil {
		t.Fatal("duplicate check accepted")
	}
}
func TestConcurrentShutdownWinsOverHealthRefresh(t *testing.T) {
	state := health.New()
	entered := make(chan struct{})
	release := make(chan struct{})
	if err := state.AddCheck("paused", func(context.Context) error { close(entered); <-release; return nil }); err != nil {
		t.Fatal(err)
	}
	state.SetReady(true)
	var wg sync.WaitGroup
	wg.Go(func() { state.Refresh(t.Context()) })
	<-entered
	state.SetReady(false)
	close(release)
	wg.Wait()
	response, err := state.GRPC().Check(t.Context(), &healthpb.HealthCheckRequest{})
	if err != nil || response.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatal("refresh resurrected shutting-down service")
	}
}

func TestCachedReadinessRepeatedActivationAndPanic(t *testing.T) {
	state := health.New()
	var probes atomic.Int32
	var panicProbe atomic.Bool
	if err := state.AddCheck("required", func(context.Context) error {
		probes.Add(1)
		if panicProbe.Load() {
			panic("private probe detail")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	state.SetReady(true)
	if state.Ready(t.Context()) {
		t.Fatal("unprobed dependency was ready")
	}
	state.Refresh(t.Context())
	state.SetReady(true)
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			rec := httptest.NewRecorder()
			state.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
			res, err := state.GRPC().Check(t.Context(), &healthpb.HealthCheckRequest{})
			if rec.Code != 200 || err != nil || res.GetStatus() != healthpb.HealthCheckResponse_SERVING {
				t.Error("shared cache not healthy")
			}
		})
	}
	wg.Wait()
	if probes.Load() != 1 {
		t.Fatal("read path reprobed dependencies")
	}
	if err := state.AddCheck("late", func(context.Context) error { return nil }); err == nil {
		t.Fatal("late registration accepted")
	}
	panicProbe.Store(true)
	state.Refresh(t.Context())
	res, err := state.GRPC().Check(t.Context(), &healthpb.HealthCheckRequest{})
	if state.Ready(t.Context()) || err != nil || res.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatal("panic did not fail closed")
	}
	panicProbe.Store(false)
	state.Refresh(t.Context())
	if !state.Ready(t.Context()) {
		t.Fatal("probe gate leaked after panic")
	}
}

func TestOldLifecycleProbeCannotPublishAfterReactivation(t *testing.T) {
	state := health.New()
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	if err := state.AddCheck("required", func(context.Context) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return nil
		}
		return errors.New("down")
	}); err != nil {
		t.Fatal(err)
	}
	state.SetReady(true)
	var wg sync.WaitGroup
	wg.Go(func() { state.Refresh(t.Context()) })
	<-entered
	state.SetReady(false)
	state.SetReady(true)
	close(release)
	wg.Wait()
	if state.Ready(t.Context()) {
		t.Fatal("previous lifecycle resurrected cache")
	}
	state.Refresh(t.Context())
	if state.Ready(t.Context()) {
		t.Fatal("new failing probe was ignored")
	}
}

type waitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *waitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestQueuedRefreshCanBeCanceled(t *testing.T) {
	state := health.New()
	entered, release := make(chan struct{}), make(chan struct{})
	if err := state.AddCheck("required", func(context.Context) error { close(entered); <-release; return nil }); err != nil {
		t.Fatal(err)
	}
	state.SetReady(true)
	var wg sync.WaitGroup
	wg.Go(func() { state.Refresh(t.Context()) })
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	waiting := &waitContext{Context: ctx, entered: make(chan struct{})}
	done := make(chan struct{})
	wg.Go(func() { state.Refresh(waiting); close(done) })
	<-waiting.entered
	cancel()
	<-done // 必须在被阻塞的第一项探测释放前返回。
	close(release)
	wg.Wait()
}
