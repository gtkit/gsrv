package bootstrap

import (
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	pb "example.com/service/pb/info/v1"
	"github.com/gtkit/gsrv"
	json "github.com/gtkit/json/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type quotaProbe struct {
	mu   sync.Mutex
	used map[string]int
	max  int
}

func (q *quotaProbe) Allow(_ context.Context, key string) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.used[key]++
	return q.used[key] <= q.max, nil
}

type identityService struct {
	pb.UnimplementedInfoServiceServer
}

func (identityService) GetInfo(ctx context.Context, _ *pb.GetInfoRequest) (*pb.GetInfoResponse, error) {
	identity, ok := gsrv.IdentityFromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Internal, "missing identity")
	}
	return &pb.GetInfoResponse{ServiceName: identity.AppKey}, nil
}
func testCredentials(key string) gsrv.Credential {
	return gsrv.Credential{AppKey: key, KeyID: "primary", Secret: strings.Repeat(key, 32), Token: strings.Repeat(key, 32)}
}
func startPublicStack(t *testing.T, useGateway bool) (string, *grpc.ClientConn, *quotaProbe) {
	t.Helper()
	state := gsrv.NewHealth()
	verifier, err := gsrv.NewVerifier(gsrv.AuthConfig{Service: "test", Credentials: []gsrv.Credential{testCredentials("a"), testCredentials("b")}})
	if err != nil {
		t.Fatal(err)
	}
	quota := &quotaProbe{used: make(map[string]int), max: 1}
	validator, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	public, err := gsrv.NewGRPCServer("test", gsrv.GRPCConfig{Addr: "127.0.0.1:0"},
		gsrv.WithGRPCAuth(verifier), gsrv.WithGRPCLimiter(quota), gsrv.WithGRPCHealth(state), gsrv.WithGRPCValidation(func(m proto.Message) error { return validator.Validate(m) }))
	if err != nil {
		t.Fatal(err)
	}
	register := func(registrar grpc.ServiceRegistrar) { pb.RegisterInfoServiceServer(registrar, identityService{}) }
	public.Register(register)
	var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, _ := gsrv.IdentityFromContext(r.Context())
		_, _ = w.Write([]byte(identity.AppKey))
	})
	var private *gsrv.GRPCServer
	if useGateway {
		directory, dirErr := os.MkdirTemp("", "gsrv-")
		if dirErr != nil {
			t.Fatal(dirErr)
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(directory); err != nil {
				t.Error(err)
			}
		})
		socket := filepath.Join(directory, "backend.sock")
		private, err = gsrv.NewGatewayBackend("test", gsrv.GRPCConfig{Network: "unix", Addr: socket}, gsrv.WithGRPCHealth(state), gsrv.WithGRPCValidation(func(m proto.Message) error { return validator.Validate(m) }))
		if err != nil {
			t.Fatal(err)
		}
		private.Register(register)
		bridge, err := gsrv.NewGateway(socket, pb.RegisterInfoServiceHandler)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := bridge.Close(); err != nil {
				t.Error(err)
			}
		})
		handler = bridge
	}
	front, err := gsrv.NewHTTPServer("test", gsrv.HTTPConfig{Addr: "127.0.0.1:0"}, handler, verifier, quota, state)
	if err != nil {
		t.Fatal(err)
	}
	servers := []gsrv.Server{front, public}
	if private != nil {
		servers = append(servers, private)
	}
	ready := make(chan struct{})
	app, err := gsrv.New("test", gsrv.WithServers(servers...), gsrv.WithHealth(state),
		gsrv.WithHook(gsrv.Hook{Name: "test-ready", Start: func(context.Context) error { close(ready); return nil }, Stop: func(context.Context) error { return nil }}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan struct{})
	var runErr error
	var wg sync.WaitGroup
	wg.Go(func() { runErr = app.Run(ctx); close(finished) })
	t.Cleanup(func() {
		cancel()
		<-finished
		if runErr != nil {
			t.Error(runErr)
		}
		wg.Wait()
	})
	select {
	case <-ready:
	case <-finished:
		t.Fatalf("start failed: %v", runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("start timed out")
	}
	connection, err := grpc.NewClient(public.Address(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return "http://" + front.Address(), connection, quota
}
func signedHTTP(t *testing.T, url, key, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "GET", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := gsrv.SignHTTP(req, "test", testCredentials(key), time.Now(), rand.Text()); err != nil {
		t.Fatal(err)
	}
	return req
}
func TestDualProtocolSharesVerifiedAppKeyQuota(t *testing.T) {
	base, connection, quota := startPublicStack(t, false)
	response, err := http.DefaultClient.Do(signedHTTP(t, base+"/protected", "a", "{}"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 || string(body) != "a" {
		t.Fatal("HTTP identity failed")
	}
	client := pb.NewInfoServiceClient(connection)
	ctx := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("x-app-key", "a", "authorization", "Bearer "+testCredentials("a").Token))
	_, err = client.GetInfo(ctx, &pb.GetInfoRequest{})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("shared quota=%v", err)
	}
	ctx = metadata.NewOutgoingContext(t.Context(), metadata.Pairs("x-app-key", "b", "authorization", "Bearer "+testCredentials("b").Token))
	result, err := client.GetInfo(ctx, &pb.GetInfoRequest{})
	if err != nil || result.GetServiceName() != "b" {
		t.Fatalf("gRPC identity=%v err=%v", result, err)
	}
	forged := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("x-app-key", "a", "authorization", "Bearer "+testCredentials("b").Token))
	if _, err := client.GetInfo(forged, &pb.GetInfoRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatal("token accepted with forged AppKey")
	}
	quota.mu.Lock()
	used := quota.used["a"]
	quota.mu.Unlock()
	if used != 2 {
		t.Fatal("authentication failure consumed caller quota")
	}
}
func TestPrivateGatewayCannotAcceptClientIdentityHeaders(t *testing.T) {
	base, connection, quota := startPublicStack(t, true)
	req := signedHTTP(t, base+"/v1/service", "a", "")
	req.Header.Set("Grpc-Metadata-X-Gsrv-App-Key", "forged")
	req.Header.Set("X-Gsrv-App-Key", "forged")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	var result struct {
		ServiceName string `json:"serviceName"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || result.ServiceName != "a" {
		t.Fatalf("gateway status=%d body=%s", response.StatusCode, body)
	}
	quota.mu.Lock()
	used := quota.used["a"]
	quota.mu.Unlock()
	if used != 1 {
		t.Fatalf("gateway double-charged quota: %d", used)
	}
	healthClient := healthpb.NewHealthClient(connection)
	if _, err := healthClient.Check(t.Context(), &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal("health unexpectedly needs token")
	}
}
