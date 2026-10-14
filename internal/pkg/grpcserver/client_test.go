package grpcserver

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpchealth "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/gtkit/gsrv/internal/pkg/auth"
	"github.com/gtkit/gsrv/internal/pkg/breaker"
	"github.com/gtkit/gsrv/internal/pkg/logging"
	request "github.com/gtkit/gsrv/internal/pkg/requestid"
	pb "github.com/gtkit/gsrv/pb/info/v1"
)

type outboundInfo struct {
	pb.UnimplementedInfoServiceServer
	call func(context.Context) (*pb.GetInfoResponse, error)
}

func (s *outboundInfo) GetInfo(ctx context.Context, _ *pb.GetInfoRequest) (*pb.GetInfoResponse, error) {
	return s.call(ctx)
}

func startClientPeer(t *testing.T, call func(context.Context) (*pb.GetInfoResponse, error), options ...grpc.ServerOption) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	verify := func(ctx context.Context) error {
		md, _ := metadata.FromIncomingContext(ctx)
		if len(md.Get("x-app-key")) != 1 || md.Get("x-app-key")[0] != "service-a" || len(md.Get("authorization")) != 1 || md.Get("authorization")[0] != "Bearer "+strings.Repeat("t", 32) || len(md.Get("x-request-id")) != 1 || len(md.Get("x-gsrv-app-key")) != 0 || len(md.Get("cookie")) != 0 || len(md.Get("proxy-authorization")) != 0 || len(md.Get("x-signature")) != 0 {
			return status.Error(codes.Unauthenticated, "wrong identity")
		}
		return nil
	}
	options = append(options, grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		if err := verify(ctx); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}), grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, next grpc.StreamHandler) error {
		if err := verify(stream.Context()); err != nil {
			return err
		}
		return next(srv, stream)
	}))
	server := grpc.NewServer(options...)
	pb.RegisterInfoServiceServer(server, &outboundInfo{call: call})
	health := grpchealth.NewServer()
	health.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(server, health)
	// 仅测试使用的流服务，复用现有消息产物，不修改业务proto。
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Outbound",
		HandlerType: (*pb.InfoServiceServer)(nil),
		Streams: []grpc.StreamDesc{
			{StreamName: "Collect", ClientStreams: true, Handler: outboundStreamHandler(false)},
			{StreamName: "Echo", ClientStreams: true, ServerStreams: true, Handler: outboundStreamHandler(true)},
		},
	}, &outboundInfo{call: call})
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Error(err)
		}
	})
	t.Cleanup(func() { server.Stop(); wg.Wait() })
	return listener.Addr().String()
}

func outboundStreamHandler(serverStreams bool) grpc.StreamHandler {
	return func(srv any, stream grpc.ServerStream) error {
		for {
			err := stream.RecvMsg(new(pb.GetInfoRequest))
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if errors.Is(err, io.EOF) && serverStreams {
				return nil
			}
			if err == nil && !serverStreams {
				continue
			}
			response, err := srv.(*outboundInfo).call(stream.Context())
			if err != nil {
				return err
			}
			if err := stream.SendMsg(response); err != nil {
				return err
			}
			if !serverStreams {
				return nil
			}
		}
	}
}

func outboundClient(t *testing.T, target string, d *breaker.Dependency) *Client {
	t.Helper()
	client, err := NewClient(ClientConfig{Target: target, Plaintext: true}, auth.Credential{AppKey: "service-a", Token: strings.Repeat("t", 32)}, d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func grpcDependency(t *testing.T, m *logging.Metrics) *breaker.Dependency {
	t.Helper()
	d, err := breaker.NewDependency("peer", breaker.DependencyConfig{MaxInflight: 1}, m)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestGRPCClientOwnMetadataGeneratedClientAndNoRetry(t *testing.T) {
	var calls atomic.Int32
	target := startClientPeer(t, func(ctx context.Context) (*pb.GetInfoResponse, error) {
		calls.Add(1)
		md, _ := metadata.FromIncomingContext(ctx)
		if len(md.Get("traceparent")) != 1 || md.Get("traceparent")[0] != "preserved-trace" {
			return nil, status.Error(codes.Internal, "trace propagation failed")
		}
		return &pb.GetInfoResponse{ServiceName: md.Get("x-app-key")[0], RequestId: md.Get("x-request-id")[0]}, nil
	})
	client := outboundClient(t, target, grpcDependency(t, nil))
	scope := request.New("shared-id")
	scope.Authenticate("service-b")
	outgoing := metadata.MD{"authorization": {"Bearer inbound-private"}, "aUtHoRiZaTiOn": {"Bearer forged"}, "X-App-Key": {"service-b"}, "X-Request-ID": {"forged"}, "x-gsrv-app-key": {"service-b"}, "Cookie": {"session=inbound"}, "Proxy-Authorization": {"Basic inbound-private"}, "X-Signature": {"inbound-signature"}, "traceparent": {"preserved-trace"}}
	before := outgoing.Copy()
	ctx := metadata.NewOutgoingContext(request.WithScope(t.Context(), scope), outgoing)
	for range 2 {
		result, err := pb.NewInfoServiceClient(client).GetInfo(ctx, &pb.GetInfoRequest{})
		if err != nil || result.ServiceName != "service-a" || result.RequestId != "shared-id" {
			t.Fatalf("result=%v err=%v", result, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("attempts=%d", calls.Load())
	}
	if !maps.EqualFunc(outgoing, before, slices.Equal) {
		t.Fatal("caller metadata changed")
	}
	var failed atomic.Int32
	badTarget := startClientPeer(t, func(context.Context) (*pb.GetInfoResponse, error) {
		failed.Add(1)
		return nil, status.Error(codes.Unavailable, "private downstream detail")
	})
	badClient := outboundClient(t, badTarget, grpcDependency(t, nil))
	_, err := pb.NewInfoServiceClient(badClient).GetInfo(t.Context(), &pb.GetInfoRequest{})
	if status.Code(err) != codes.Unavailable || failed.Load() != 1 || status.Convert(err).Message() != "downstream RPC failed" {
		t.Fatalf("status=%v attempts=%d", status.Code(err), failed.Load())
	}
}

func TestGRPCClientStreamSlotCancellationAndClose(t *testing.T) {
	target := startClientPeer(t, func(context.Context) (*pb.GetInfoResponse, error) { return &pb.GetInfoResponse{ServiceName: "ok"}, nil })
	d := grpcDependency(t, nil)
	client := outboundClient(t, target, d)
	for range 10 {
		ctx, cancel := context.WithCancel(t.Context())
		stream, err := healthpb.NewHealthClient(client).Watch(ctx, &healthpb.HealthCheckRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(); err != nil {
			t.Fatal(err)
		}
		if _, err := pb.NewInfoServiceClient(client).GetInfo(t.Context(), &pb.GetInfoRequest{}); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("stream did not hold slot: %v", err)
		}
		cancel()
		if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
			t.Fatalf("cancel=%v", err)
		}
		if _, err := pb.NewInfoServiceClient(client).GetInfo(t.Context(), &pb.GetInfoRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	stream, err := healthpb.NewHealthClient(client).Watch(t.Context(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.Canceled && status.Code(err) != codes.Unavailable {
		t.Fatalf("close=%v", err)
	}
	_, finish, err := d.Begin(t.Context(), "http", "service-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pb.NewInfoServiceClient(client).GetInfo(t.Context(), &pb.GetInfoRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("after close=%v", err)
	}
}

func TestGRPCClientTLSVerification(t *testing.T) {
	certServer := httptest.NewUnstartedServer(http.NotFoundHandler())
	certServer.StartTLS()
	t.Cleanup(certServer.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certServer.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	target := startClientPeer(t, func(context.Context) (*pb.GetInfoResponse, error) { return &pb.GetInfoResponse{ServiceName: "ok"}, nil }, grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: certServer.TLS.Certificates})))
	credential := auth.Credential{AppKey: "service-a", Token: strings.Repeat("t", 32)}
	client, err := NewClient(ClientConfig{Target: target, CAFile: ca}, credential, grpcDependency(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := pb.NewInfoServiceClient(client).GetInfo(t.Context(), &pb.GetInfoRequest{}); err != nil {
		t.Fatal(err)
	}
	wrong, err := NewClient(ClientConfig{Target: target, CAFile: ca, ServerName: "wrong.invalid"}, credential, grpcDependency(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wrong.Close() })
	if _, err := pb.NewInfoServiceClient(wrong).GetInfo(t.Context(), &pb.GetInfoRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("certificate name validation skipped=%v", err)
	}
}

func TestGRPCClientMessageLimitsCannotBeRelaxedByCallOptions(t *testing.T) {
	target := startClientPeer(t, func(context.Context) (*pb.GetInfoResponse, error) {
		return &pb.GetInfoResponse{ServiceName: strings.Repeat("x", 128)}, nil
	})
	credential := auth.Credential{AppKey: "service-a", Token: strings.Repeat("t", 32)}
	client, err := NewClient(ClientConfig{Target: target, Plaintext: true, MaxRecvBytes: 8}, credential, grpcDependency(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := pb.NewInfoServiceClient(client).GetInfo(t.Context(), &pb.GetInfoRequest{}, grpc.MaxCallRecvMsgSize(1<<20)); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("unary limit relaxed=%v", err)
	}
	stream, err := client.NewStream(t.Context(), &grpc.StreamDesc{ServerStreams: true}, "/info.v1.InfoService/GetInfo", grpc.MaxCallRecvMsgSize(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SendMsg(&pb.GetInfoRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if err := stream.RecvMsg(new(pb.GetInfoResponse)); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("stream limit relaxed=%v", err)
	}
}

func TestGRPCClientStreamSuccessAndUnaryDeadline(t *testing.T) {
	target := startClientPeer(t, func(context.Context) (*pb.GetInfoResponse, error) { return &pb.GetInfoResponse{ServiceName: "ok"}, nil })
	client := outboundClient(t, target, grpcDependency(t, nil))
	stream, err := client.NewStream(t.Context(), &grpc.StreamDesc{ServerStreams: true}, "/info.v1.InfoService/GetInfo")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SendMsg(&pb.GetInfoRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if _, err := pb.NewInfoServiceClient(client).GetInfo(t.Context(), &pb.GetInfoRequest{}); status.Code(err) != codes.ResourceExhausted {
		t.Fatal("CloseSend freed live slot")
	}
	if err := stream.RecvMsg(new(pb.GetInfoResponse)); err != nil {
		t.Fatal(err)
	}
	if err := stream.RecvMsg(new(pb.GetInfoResponse)); !errors.Is(err, io.EOF) {
		t.Fatalf("end=%v", err)
	}
	if _, err := pb.NewInfoServiceClient(client).GetInfo(t.Context(), &pb.GetInfoRequest{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := pb.NewInfoServiceClient(client).GetInfo(ctx, &pb.GetInfoRequest{}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("deadline=%v", err)
	}
	for _, cfg := range []ClientConfig{{Target: "example.com:9090", Plaintext: true}, {Target: "127.0.0.1:0", Plaintext: true}, {Target: "dns:///example.com:9090"}, {Target: "127.0.0.1:9090", Plaintext: true, CAFile: "unexpected"}, {Target: "127.0.0.1:9090", MaxRecvBytes: -1}} {
		if _, err := cfg.Normalize(); err == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
}

func TestGRPCClientStreamingFinalResponseReleasesSlot(t *testing.T) {
	target := startClientPeer(t, func(context.Context) (*pb.GetInfoResponse, error) { return &pb.GetInfoResponse{ServiceName: "ok"}, nil })
	client := outboundClient(t, target, grpcDependency(t, nil))
	stream, err := client.NewStream(t.Context(), &grpc.StreamDesc{ClientStreams: true}, "/test.Outbound/Collect")
	if err != nil {
		t.Fatal(err)
	}
	generated := &grpc.GenericClientStream[pb.GetInfoRequest, pb.GetInfoResponse]{ClientStream: stream}
	for range 3 {
		if err := generated.Send(&pb.GetInfoRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	response, err := generated.CloseAndRecv()
	if err != nil || response.ServiceName != "ok" {
		t.Fatalf("final response=%v error=%v", response, err)
	}
	// 生成客户端只Recv一次；不能要求用户额外Recv EOF才能归还容量。
	if _, err := pb.NewInfoServiceClient(client).GetInfo(t.Context(), &pb.GetInfoRequest{}); err != nil {
		t.Fatal(err)
	}
}

func TestGRPCClientBidiKeepsSlotUntilEOF(t *testing.T) {
	target := startClientPeer(t, func(context.Context) (*pb.GetInfoResponse, error) { return &pb.GetInfoResponse{ServiceName: "ok"}, nil })
	client := outboundClient(t, target, grpcDependency(t, nil))
	stream, err := client.NewStream(t.Context(), &grpc.StreamDesc{ClientStreams: true, ServerStreams: true}, "/test.Outbound/Echo")
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := stream.SendMsg(&pb.GetInfoRequest{}); err != nil {
			t.Fatal(err)
		}
		if err := stream.RecvMsg(new(pb.GetInfoResponse)); err != nil {
			t.Fatal(err)
		}
		if _, err := pb.NewInfoServiceClient(client).GetInfo(t.Context(), &pb.GetInfoRequest{}); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("bidi response freed active slot: %v", err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if err := stream.RecvMsg(new(pb.GetInfoResponse)); !errors.Is(err, io.EOF) {
		t.Fatalf("final status=%v", err)
	}
	if _, err := pb.NewInfoServiceClient(client).GetInfo(t.Context(), &pb.GetInfoRequest{}); err != nil {
		t.Fatal(err)
	}
}

func TestGRPCClientAbandonedStreamCancellationBudgetAndConcurrentClose(t *testing.T) {
	for _, mode := range []string{"cancel", "caller-deadline", "dependency-deadline", "concurrent-close"} {
		t.Run(mode, func(t *testing.T) {
			target := startClientPeer(t, func(context.Context) (*pb.GetInfoResponse, error) { return &pb.GetInfoResponse{}, nil })
			metrics, err := logging.NewMetrics("caller")
			if err != nil {
				t.Fatal(err)
			}
			timeout := 5 * time.Second
			if mode == "dependency-deadline" {
				timeout = 300 * time.Millisecond
			}
			d, err := breaker.NewDependency("peer", breaker.DependencyConfig{Timeout: timeout, MaxInflight: 1}, metrics)
			if err != nil {
				t.Fatal(err)
			}
			client := outboundClient(t, target, d)
			if _, err := healthpb.NewHealthClient(client).Check(t.Context(), &healthpb.HealthCheckRequest{}); err != nil {
				t.Fatal(err)
			}
			callerTimeout := 5 * time.Second
			if mode == "caller-deadline" {
				callerTimeout = 300 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), callerTimeout)
			defer cancel()
			stream, err := client.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/grpc.health.v1.Health/Watch")
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.SendMsg(&healthpb.HealthCheckRequest{}); err != nil {
				t.Fatal(err)
			}
			if err := stream.CloseSend(); err != nil {
				t.Fatal(err)
			}
			if err := stream.RecvMsg(new(healthpb.HealthCheckResponse)); err != nil {
				t.Fatal(err)
			}
			// 不再Recv：回收不能依赖应用继续读取。
			wantCode := "CANCELED"
			switch mode {
			case "cancel":
				cancel()
			case "concurrent-close":
				var wg sync.WaitGroup
				for range 16 {
					wg.Go(func() {
						if err := client.Close(); err != nil {
							t.Error(err)
						}
					})
				}
				wg.Go(cancel)
				wg.Wait()
			default:
				wantCode = "DEADLINE_EXCEEDED"
			}
			select {
			case <-stream.(*clientStream).done:
			case <-time.After(2 * time.Second):
				t.Fatal("abandoned stream did not release capacity")
			}
			_, finish, err := d.Begin(t.Context(), "http", "service-a")
			if err != nil {
				t.Fatal(err)
			}
			if err := finish(nil); err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			metrics.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			for _, line := range []string{
				`gsrv_dependency_inflight{service="caller",dependency="peer",protocol="grpc"} 0`,
				`gsrv_dependency_requests_total{service="caller",dependency="peer",protocol="grpc",error_code="` + wantCode + `"} 1`,
			} {
				if !strings.Contains(recorder.Body.String(), line+"\n") {
					t.Fatalf("missing final metric %s", line)
				}
			}
		})
	}
}

func TestGRPCClientUnaryBudgetAndTokenBounds(t *testing.T) {
	for _, tc := range []struct {
		name            string
		caller, timeout time.Duration
	}{{"caller", 200 * time.Millisecond, 5 * time.Second}, {"dependency", 5 * time.Second, 200 * time.Millisecond}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			target := startClientPeer(t, func(ctx context.Context) (*pb.GetInfoResponse, error) {
				calls.Add(1)
				<-ctx.Done()
				return nil, status.FromContextError(ctx.Err()).Err()
			})
			d, err := breaker.NewDependency("peer", breaker.DependencyConfig{Timeout: tc.timeout, MaxInflight: 1}, nil)
			if err != nil {
				t.Fatal(err)
			}
			client := outboundClient(t, target, d)
			if _, err := healthpb.NewHealthClient(client).Check(t.Context(), &healthpb.HealthCheckRequest{}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), tc.caller)
			defer cancel()
			start := time.Now()
			_, err = pb.NewInfoServiceClient(client).GetInfo(ctx, &pb.GetInfoRequest{})
			if status.Code(err) != codes.DeadlineExceeded || calls.Load() != 1 || time.Since(start) > 2*time.Second {
				t.Fatalf("budget code=%s attempts=%d duration=%s", status.Code(err), calls.Load(), time.Since(start))
			}
			_, finish, err := d.Begin(t.Context(), "http", "service-a")
			if err != nil {
				t.Fatal(err)
			}
			if err := finish(nil); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, size := range []int{0, 31, 32, 4096, 4097} {
		client, err := NewClient(ClientConfig{Target: "127.0.0.1:9090", Plaintext: true}, auth.Credential{AppKey: "service-a", Token: strings.Repeat("t", size)}, grpcDependency(t, nil))
		valid := size >= 32 && size <= 4096
		if (err == nil) != valid {
			t.Fatalf("token size=%d valid=%t error=%v", size, valid, err)
		}
		if client != nil {
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}
