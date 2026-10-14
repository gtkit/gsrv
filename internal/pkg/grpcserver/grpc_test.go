package grpcserver

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/gtkit/gsrv/internal/pkg/auth"
	"github.com/gtkit/gsrv/internal/pkg/health"
	"github.com/gtkit/gsrv/internal/pkg/lifecycle"
)

type memoryServer struct {
	*Server
	buffer    *bufconn.Listener
	listening chan struct{}
}

func (s *memoryServer) Listen() error {
	s.mu.Lock()
	s.listener = s.buffer
	s.mu.Unlock()
	close(s.listening)
	return nil
}

func TestDefaultHealthCrossConnectionAndRealStreamCancellation(t *testing.T) {
	credential := auth.Credential{AppKey: "caller", KeyID: "key", Token: strings.Repeat("t", 32)}
	v, err := auth.New(auth.Config{Service: "test", Credentials: []auth.Credential{credential}})
	if err != nil {
		t.Fatal(err)
	}
	validate := func(m proto.Message) error {
		if _, ok := m.(*healthpb.HealthCheckRequest); ok {
			return errors.New("business validation must not run for Health")
		}
		return nil
	}
	server, err := New("test", Config{MaxInflight: 1}, WithAuth(v), WithValidation(validate))
	if err != nil {
		t.Fatal(err)
	}
	other, err := New("test", Config{}, WithAuth(v))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.New("test", lifecycle.WithServers(server, other)); err == nil {
		t.Fatal("mismatched default states accepted")
	}
	if server.Address() != "" || other.Address() != "" {
		t.Fatal("constructor bound listeners")
	}
	for _, cfg := range []Config{{MaxInflight: -1}, {MaxInflight: 1}} {
		opts := []Option{WithAuth(v)}
		if cfg.MaxInflight == 1 {
			opts = append(opts, WithHealth(new(health.State)))
		}
		if _, err := New("test", cfg, opts...); err == nil {
			t.Fatal("negative cap/zero health accepted")
		}
	}
	entered, release, streamEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	server.Register(func(registrar grpc.ServiceRegistrar) {
		registrar.RegisterService(&grpc.ServiceDesc{ServiceName: "test.Protection", HandlerType: (*any)(nil), Methods: []grpc.MethodDesc{{MethodName: "Call", Handler: func(srv any, ctx context.Context, decode func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			req := new(emptypb.Empty)
			if err := decode(req); err != nil {
				return nil, err
			}
			handler := func(ctx context.Context, _ any) (any, error) {
				once.Do(func() { close(entered) })
				select {
				case <-release:
					return new(emptypb.Empty), nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return interceptor(ctx, req, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/test.Protection/Call"}, handler)
		}}}, Streams: []grpc.StreamDesc{{StreamName: "Stream", ServerStreams: true, Handler: func(_ any, s grpc.ServerStream) error {
			if err := s.RecvMsg(new(emptypb.Empty)); err != nil {
				return err
			}
			close(streamEntered)
			<-s.Context().Done()
			return s.Context().Err()
		}}}}, new(struct{}))
	})
	buffered := &memoryServer{Server: server, buffer: bufconn.Listen(1 << 20), listening: make(chan struct{})}
	app, err := lifecycle.New("test", lifecycle.WithServers(buffered))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := app.Run(ctx); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(func() { cancel(); wg.Wait() })
	<-buffered.listening
	connections := make([]*grpc.ClientConn, 0, 2)
	for range 2 {
		conn, err := grpc.NewClient("passthrough:///memory", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return buffered.buffer.DialContext(ctx) }))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		connections = append(connections, conn)
	}
	probeCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	watch, err := healthpb.NewHealthClient(connections[0]).Watch(probeCtx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for {
		r, err := watch.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if r.GetStatus() == healthpb.HealthCheckResponse_SERVING {
			break
		}
	}
	stop()
	callCtx := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("x-app-key", credential.AppKey, "authorization", "Bearer "+credential.Token))
	call := func(conn *grpc.ClientConn, ctx context.Context) error {
		return conn.Invoke(ctx, "/test.Protection/Call", new(emptypb.Empty), new(emptypb.Empty))
	}
	firstDone := make(chan error, 1)
	wg.Go(func() { firstDone <- call(connections[0], callCtx) })
	<-entered
	if err := call(connections[1], callCtx); status.Code(err) != codes.ResourceExhausted {
		t.Error("second connection bypassed cap", err)
	}
	r, err := healthpb.NewHealthClient(connections[1]).Check(t.Context(), &healthpb.HealthCheckRequest{})
	if err != nil || r.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Error("Health blocked or not shared", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := call(connections[1], callCtx); err != nil {
		t.Fatal("unary slot leaked", err)
	}
	streamCtx, cancelStream := context.WithCancel(callCtx)
	defer cancelStream()
	stream, err := connections[0].NewStream(streamCtx, &grpc.StreamDesc{ServerStreams: true}, "/test.Protection/Stream")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SendMsg(new(emptypb.Empty)); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	<-streamEntered
	if err := call(connections[1], callCtx); status.Code(err) != codes.ResourceExhausted {
		t.Error("stream and unary did not share cap", err)
	}
	cancelStream()
	if err := stream.RecvMsg(new(emptypb.Empty)); status.Code(err) != codes.Canceled {
		t.Fatal("client stream cancellation not observed", err)
	}
	// 客户端取消可能先于服务端退出；等服务端真正释放槽位，不能把客户端返回当清理完成。
	retryCtx, endRetry := context.WithTimeout(callCtx, 2*time.Second)
	defer endRetry()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		err := call(connections[1], retryCtx)
		if err == nil {
			break
		}
		if status.Code(err) != codes.ResourceExhausted {
			t.Fatal("canceled stream slot not released", err)
		}
		select {
		case <-ticker.C:
		case <-retryCtx.Done():
			t.Fatal("slot cleanup timeout")
		}
	}
}
