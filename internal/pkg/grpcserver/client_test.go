package grpcserver

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		if len(md.Get("x-app-key")) != 1 || md.Get("x-app-key")[0] != "service-a" || len(md.Get("authorization")) != 1 || md.Get("authorization")[0] != "Bearer "+strings.Repeat("t", 32) || len(md.Get("x-request-id")) != 1 || len(md.Get("x-gsrv-app-key")) != 0 {
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
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Error(err)
		}
	})
	t.Cleanup(func() { server.Stop(); wg.Wait() })
	return listener.Addr().String()
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
		return &pb.GetInfoResponse{ServiceName: md.Get("x-app-key")[0], RequestId: md.Get("x-request-id")[0]}, nil
	})
	client := outboundClient(t, target, grpcDependency(t, nil))
	scope := request.New("shared-id")
	scope.Authenticate("service-b")
	ctx := metadata.NewOutgoingContext(request.WithScope(t.Context(), scope), metadata.Pairs("authorization", "Bearer inbound-private", "x-app-key", "service-b", "x-request-id", "forged", "x-gsrv-app-key", "service-b"))
	for range 2 {
		result, err := pb.NewInfoServiceClient(client).GetInfo(ctx, &pb.GetInfoRequest{})
		if err != nil || result.ServiceName != "service-a" || result.RequestId != "shared-id" {
			t.Fatalf("result=%v err=%v", result, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("attempts=%d", calls.Load())
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
