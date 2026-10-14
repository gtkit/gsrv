package httpserver

import (
	"context"
	"encoding/pem"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gtkit/gsrv/internal/pkg/auth"
	"github.com/gtkit/gsrv/internal/pkg/breaker"
	"github.com/gtkit/gsrv/internal/pkg/fault"
	request "github.com/gtkit/gsrv/internal/pkg/requestid"
)

func clientCredential() auth.Credential {
	return auth.Credential{AppKey: "service-a", KeyID: "primary", Secret: strings.Repeat("s", 32), Token: strings.Repeat("t", 32)}
}

func httpDependency(t *testing.T) *breaker.Dependency {
	t.Helper()
	d, err := breaker.NewDependency("peer", breaker.DependencyConfig{MaxInflight: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestHTTPClientExactSignatureOwnIdentityAndReuse(t *testing.T) {
	credential := clientCredential()
	verifier, err := auth.New(auth.Config{Service: "peer-service", Credentials: []auth.Credential{credential}})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var nonces, connections []string
	body := []byte("{\n  \"value\": \"原始字节\"\n}\n")
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, err := verifier.AuthenticateHTTP(r)
		if err != nil {
			t.Error(err)
			w.WriteHeader(401)
			return
		}
		actual, err := io.ReadAll(r.Body)
		if err != nil || string(actual) != string(body) || identity.AppKey != "service-a" || r.URL.RequestURI() != "/v1/check%2Fraw?b=2&a=1" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-Request-ID") != "shared-id" || r.Header.Get("Authorization") != "" || r.Header.Get("Grpc-Metadata-Authorization") != "" {
			t.Error("identity/signature/request bytes differ")
		}
		mu.Lock()
		nonces = append(nonces, r.Header.Get(auth.NonceHeader))
		connections = append(connections, r.RemoteAddr)
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}))
	server.Start()
	t.Cleanup(server.Close)
	client, err := NewClient(ClientConfig{Service: "peer-service", BaseURL: server.URL}, credential, httpDependency(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	scope := request.New("shared-id")
	scope.Authenticate("service-b")
	ctx := auth.WithIdentity(request.WithScope(t.Context(), scope), auth.Identity{AppKey: "service-b", KeyID: "inbound"})
	headers := http.Header{"content-type": {"application/json"}, "x-app-key": {"service-b"}, "x-key-id": {"other"}, "x-request-id": {"forged"}, "Authorization": {"Bearer inbound-private"}, "Grpc-Metadata-Authorization": {"Bearer inbound-private"}}
	original := headers.Clone()
	for range 2 {
		result, err := client.Do(ctx, "POST", "/v1/check%2Fraw?b=2&a=1", body, headers)
		if err != nil || result.StatusCode != 200 || string(result.Body) != "ok" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(nonces) != 2 || nonces[0] == nonces[1] || connections[0] != connections[1] {
		t.Fatalf("nonce/reuse differ: count=%d", len(nonces))
	}
	if !maps.EqualFunc(headers, original, slices.Equal) {
		t.Fatal("input headers changed")
	}
}

func TestHTTPClientFailuresDoNotRetryOrRedirect(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/failed":
			w.WriteHeader(503)
		case "/redirect":
			w.Header().Set("Location", "/target")
			w.WriteHeader(302)
		case "/large":
			_, _ = io.WriteString(w, strings.Repeat("x", 9))
		default:
			_, _ = io.WriteString(w, "ok")
		}
	}))
	server.Start()
	t.Cleanup(server.Close)
	client, err := NewClient(ClientConfig{Service: "peer", BaseURL: server.URL, MaxResponseBytes: 8, MaxRequestBytes: 8}, clientCredential(), httpDependency(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	for _, tc := range []struct {
		path   string
		status int
		code   fault.Code
	}{{"/failed", 503, fault.Unavailable}, {"/redirect", 302, fault.Unavailable}, {"/large", 200, fault.PayloadTooLarge}} {
		t.Run(tc.path, func(t *testing.T) {
			before := calls.Load()
			result, err := client.Do(t.Context(), "GET", tc.path, nil, nil)
			if result == nil || result.StatusCode != tc.status || fault.CodeOf(err) != tc.code || calls.Load() != before+1 {
				t.Fatalf("status/result=%+v err=%v attempts=%d", result, err, calls.Load()-before)
			}
			if tc.path == "/large" && len(result.Body) != 0 {
				t.Fatal("oversized body returned")
			}
		})
	}
	before := calls.Load()
	for _, tc := range []struct {
		path string
		body []byte
		code fault.Code
	}{{"http://127.0.0.1/foreign", nil, fault.InvalidArgument}, {"//127.0.0.1/foreign", nil, fault.InvalidArgument}, {"/local#fragment", nil, fault.InvalidArgument}, {"/local", []byte("too-large"), fault.PayloadTooLarge}} {
		if _, err := client.Do(t.Context(), "POST", tc.path, tc.body, nil); fault.CodeOf(err) != tc.code {
			t.Fatalf("input boundary=%v", err)
		}
	}
	if calls.Load() != before {
		t.Fatal("invalid input reached downstream")
	}
}

func TestHTTPClientCloseCancelsAndReleasesSharedGate(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }))
	server.Start()
	t.Cleanup(server.Close)
	d := httpDependency(t)
	client, err := NewClient(ClientConfig{Service: "peer", BaseURL: server.URL}, clientCredential(), d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	var wg sync.WaitGroup
	var result error
	wg.Go(func() { _, result = client.Do(t.Context(), "GET", "/blocked", nil, nil) })
	<-entered
	if _, _, err := d.Begin(t.Context(), "grpc", "service-a"); fault.CodeOf(err) != fault.ResourceExhausted {
		t.Fatalf("shared gate=%v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if fault.CodeOf(result) != fault.Canceled {
		t.Fatalf("close=%v", result)
	}
	_, finish, err := d.Begin(t.Context(), "grpc", "service-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(context.WithoutCancel(t.Context()), "GET", "/blocked", nil, nil); fault.CodeOf(err) != fault.Unavailable {
		t.Fatalf("after close=%v", err)
	}
}

func TestHTTPClientTLSAndConfiguration(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	server.StartTLS()
	t.Cleanup(server.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{Service: "peer", BaseURL: server.URL, CAFile: ca}, clientCredential(), httpDependency(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := client.Do(t.Context(), "GET", "/", nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []ClientConfig{{Service: "peer", BaseURL: "http://example.com"}, {Service: "peer", BaseURL: "https://user@example.com"}, {Service: "peer", BaseURL: "https://example.com/path"}, {Service: "peer", BaseURL: "https://example.com?q=1"}, {Service: "peer", BaseURL: "https://example.com", MaxRequestBytes: 1<<20 + 1}, {Service: "peer", BaseURL: "https://example.com", MaxResponseBytes: -1}} {
		if _, err := cfg.Normalize(); err == nil {
			t.Fatalf("accepted invalid config %+v", cfg)
		}
	}
}
