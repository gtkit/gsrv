package breaker_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	resilience "github.com/gtkit/gsrv/internal/pkg/breaker"
	"github.com/gtkit/gsrv/internal/pkg/fault"
	request "github.com/gtkit/gsrv/internal/pkg/requestid"
	"github.com/sony/gobreaker/v2"
)

func TestBreakerOpensAndRecovers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		breaker, err := resilience.New[string](resilience.Config{Name: "upstream", MinRequests: 2, OpenTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		failed := errors.New("down")
		operation := func(context.Context) (string, error) { return "", failed }
		for range 2 {
			if _, err := breaker.Execute(t.Context(), operation); !errors.Is(err, failed) {
				t.Fatal(err)
			}
		}
		if _, err := breaker.Execute(t.Context(), operation); !errors.Is(err, gobreaker.ErrOpenState) {
			t.Fatal("breaker did not open")
		}
		synctest.Sleep(2 * time.Second)
		if got, err := breaker.Execute(t.Context(), func(context.Context) (string, error) { return "ok", nil }); err != nil || got != "ok" {
			t.Fatal("half-open probe failed")
		}
		if breaker.State() != gobreaker.StateClosed {
			t.Fatal("recovery failed")
		}
	})
}
func TestBusinessFallbackAndNoAutomaticFallback(t *testing.T) {
	original := errors.New("down")
	primary := func(context.Context) (string, error) { return "", original }
	scope := request.New("test")
	ctx := request.WithScope(t.Context(), scope)
	value, err := resilience.Run(ctx, time.Second, nil, primary, func(error) bool { return true }, func(context.Context, error) (string, error) { return "cached", nil })
	if err != nil || value != "cached" || !scope.Degraded() {
		t.Fatal("business fallback not marked")
	}
	if _, err := resilience.Run(t.Context(), time.Second, nil, primary, nil, nil); !errors.Is(err, original) {
		t.Fatal("original lost")
	}
	called := false
	fallback := func(context.Context, error) (string, error) { called = true; return "wrong", nil }
	for _, failure := range []error{context.Canceled, fault.New(fault.InvalidArgument, "bad"), fault.New(fault.Unauthenticated, "bad")} {
		_, _ = resilience.Run(t.Context(), time.Second, nil, func(context.Context) (string, error) { return "", failure }, func(error) bool { return true }, fallback)
		if called {
			t.Fatal("unauthorized fallback")
		}
	}
}
func TestSharedBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		called := false
		_, err := resilience.Run(t.Context(), time.Second, nil, func(ctx context.Context) (string, error) { <-ctx.Done(); return "", ctx.Err() },
			func(error) bool { return true }, func(context.Context, error) (string, error) { called = true; return "", nil })
		if !errors.Is(err, context.DeadlineExceeded) || called {
			t.Fatal("fallback extended exhausted budget")
		}
	})
}
