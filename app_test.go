package gsrv_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gtkit/gsrv"
	"github.com/gtkit/gsrv/internal/pkg/health"
)

type fakeServer struct {
	failure error
	done    chan struct{}
	once    sync.Once
	closed  atomic.Int32
	state   *health.State
	t       *testing.T
}

func (s *fakeServer) Listen() error { return s.failure }
func (s *fakeServer) Serve() error  { <-s.done; return nil }
func (s *fakeServer) Shutdown(context.Context) error {
	if s.state.Ready(s.t.Context()) {
		s.t.Error("shutdown began before readiness was removed")
	}
	s.once.Do(func() { close(s.done) })
	return nil
}
func (s *fakeServer) Close() error { s.closed.Add(1); s.once.Do(func() { close(s.done) }); return nil }
func TestStartupFailureClosesListeners(t *testing.T) {
	state := health.New()
	first := &fakeServer{done: make(chan struct{}), state: state, t: t}
	failure := errors.New("port conflict")
	second := &fakeServer{failure: failure, done: make(chan struct{}), state: state, t: t}
	app, err := gsrv.New("test", gsrv.WithServers(first, second), gsrv.WithHealth(state))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Run(t.Context()); !errors.Is(err, failure) {
		t.Fatal("startup error not preserved")
	}
	if first.closed.Load() != 1 || second.closed.Load() != 1 || state.Ready(t.Context()) {
		t.Fatal("startup did not unwind")
	}
}
func TestCancellationAndHookRollback(t *testing.T) {
	state := health.New()
	server := &fakeServer{done: make(chan struct{}), state: state, t: t}
	ctx, cancel := context.WithCancel(t.Context())
	var start, stop atomic.Int32
	app, err := gsrv.New("test", gsrv.WithServers(server), gsrv.WithHealth(state),
		gsrv.WithHook(gsrv.Hook{Name: "worker", Start: func(context.Context) error { start.Add(1); cancel(); return nil },
			Stop: func(context.Context) error { stop.Add(1); return nil }}))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if start.Load() != 1 || stop.Load() != 1 || server.closed.Load() != 1 {
		t.Fatal("lifecycle not completed")
	}
	if err := app.Run(t.Context()); err == nil {
		t.Fatal("second run accepted")
	}
	broken, err := gsrv.New("test", gsrv.WithServers(&fakeServer{done: make(chan struct{}), state: state, t: t}), gsrv.WithHealth(state),
		gsrv.WithHook(gsrv.Hook{Name: "partial", Start: func(context.Context) error { return errors.New("failed") }, Stop: func(context.Context) error { stop.Add(1); return nil }}))
	if err != nil {
		t.Fatal(err)
	}
	if err := broken.Run(t.Context()); err == nil || stop.Load() != 2 {
		t.Fatal("partially started hook not stopped")
	}
}
func TestServeFailureStopsWholeApp(t *testing.T) {
	state := health.New()
	first := &fakeServer{done: make(chan struct{}), state: state, t: t}
	second := &fakeServer{done: make(chan struct{}), state: state, t: t}
	app, err := gsrv.New("test", gsrv.WithServers(first, second), gsrv.WithHealth(state),
		gsrv.WithHook(gsrv.Hook{Name: "exit", Start: func(context.Context) error { first.once.Do(func() { close(first.done) }); return nil }, Stop: func(context.Context) error { return nil }}))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Run(t.Context()); err == nil {
		t.Fatal("unexpected exit ignored")
	}
	if second.closed.Load() != 1 {
		t.Fatal("peer server left running")
	}
}
func ExampleNew() {
	server := &exampleServer{}
	app, err := gsrv.New("demo", gsrv.WithServers(server))
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = app.Run(ctx)
	// Output:
}

type exampleServer struct{}

func (*exampleServer) Listen() error                  { return nil }
func (*exampleServer) Serve() error                   { return nil }
func (*exampleServer) Shutdown(context.Context) error { return nil }
func (*exampleServer) Close() error                   { return nil }
