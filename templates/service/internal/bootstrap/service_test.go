package bootstrap

import (
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gtkit/gsrv"
	json "github.com/gtkit/json/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"example.com/service/internal/platform/config"
	pb "example.com/service/pb/info/v1"
)

func TestInfoServiceProductionAssembly(t *testing.T) {
	directory, err := os.MkdirTemp("", "gsrv-info-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	cfg := &config.Config{Name: "info-test", HTTP: &gsrv.HTTPConfig{Addr: "127.0.0.1:0"}, GRPC: &gsrv.GRPCConfig{Addr: "127.0.0.1:0"},
		GatewaySocket: filepath.Join(directory, "gateway.sock"), ShutdownTimeout: time.Second, Credentials: []gsrv.Credential{testCredentials("a")}}
	srvs, err := buildServers(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan struct{})
	var runErr error
	var wg sync.WaitGroup
	wg.Go(func() { runErr = srvs.app.Run(ctx); close(finished) })
	t.Cleanup(func() {
		cancel()
		<-finished
		wg.Wait()
		if runErr != nil {
			t.Error(runErr)
		}
		if err := srvs.Close(); err != nil {
			t.Error(err)
		}
	})
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	client := &http.Client{Timeout: time.Second}
	for {
		if address := srvs.http.Address(); address != "" {
			r, err := http.NewRequestWithContext(t.Context(), "GET", "http://"+address+"/readyz", nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(r)
			if err == nil {
				_ = response.Body.Close()
				if response.StatusCode == 200 {
					break
				}
			}
		}
		select {
		case <-ticker.C:
		case <-finished:
			t.Fatalf("start failed: %v", runErr)
		case <-deadline.C:
			t.Fatal("readiness timeout")
		}
	}
	r, err := http.NewRequestWithContext(t.Context(), "GET", "http://"+srvs.http.Address()+"/v1/service", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-Request-Id", "http-info-request")
	if err := gsrv.SignHTTP(r, cfg.Name, testCredentials("a"), time.Now(), rand.Text()); err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ServiceName string `json:"serviceName"`
		RequestID   string `json:"requestId"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || result.ServiceName != cfg.Name || result.RequestID != "http-info-request" {
		t.Fatalf("HTTP info status=%d result=%+v", response.StatusCode, result)
	}
	connection, err := grpc.NewClient(srvs.grpc.Address(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := connection.Close(); err != nil {
			t.Error(err)
		}
	}()
	callCtx := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("x-app-key", "a", "authorization", "Bearer "+testCredentials("a").Token, "x-request-id", "grpc-info-request"))
	info, err := pb.NewInfoServiceClient(connection).GetInfo(callCtx, &pb.GetInfoRequest{})
	if err != nil || info.GetServiceName() != cfg.Name || info.GetRequestId() != "grpc-info-request" {
		t.Fatalf("gRPC info=%v err=%v", info, err)
	}
}
