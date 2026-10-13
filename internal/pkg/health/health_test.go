package health_test

import (
	"context"
	"errors"
	"github.com/gtkit/gsrv/internal/pkg/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"net/http/httptest"
	"sync"
	"testing"
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
	state.SetReady(true)
	entered := make(chan struct{})
	release := make(chan struct{})
	if err := state.AddCheck("paused", func(context.Context) error { close(entered); <-release; return nil }); err != nil {
		t.Fatal(err)
	}
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
