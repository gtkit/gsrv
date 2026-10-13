package http

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gtkit/gsrv/internal/pkg/auth"
	"github.com/gtkit/gsrv/internal/pkg/health"
	"github.com/gtkit/gsrv/internal/pkg/logging"
	"github.com/gtkit/logger/v2"
)

type quotaFunc func(context.Context, string) (bool, error)

func (f quotaFunc) Allow(ctx context.Context, key string) (bool, error) { return f(ctx, key) }

func TestFinalAccessLogAndRejections(t *testing.T) {
	directory := t.TempDir()
	log, err := logging.New(logging.Config{JSON: true, Path: filepath.Join(directory, "access"), Console: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	previous := logger.Default()
	logger.SetDefault(log)
	t.Cleanup(func() { log.Sync(); logger.SetDefault(previous) })
	credential := auth.Credential{AppKey: "caller", KeyID: "primary", Secret: strings.Repeat("s", 32)}
	for _, test := range []struct {
		name, code string
		status     int
		signed     bool
		quota      quotaFunc
		handler    http.HandlerFunc
	}{
		{"success", "OK", 200, true, nil, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "done") }},
		{"auth", "UNAUTHENTICATED", 401, false, nil, func(http.ResponseWriter, *http.Request) { t.Error("unauthenticated handler ran") }},
		{"limited", "RESOURCE_EXHAUSTED", 429, true, func(context.Context, string) (bool, error) { return false, nil }, nil},
		{"store", "UNAVAILABLE", 503, true, func(context.Context, string) (bool, error) { return false, errors.New("private-store-detail") }, nil},
		{"panic", "INTERNAL", 500, true, nil, func(http.ResponseWriter, *http.Request) { panic("private-panic-detail") }},
		{"deadline", "DEADLINE_EXCEEDED", 504, true, nil, func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
			WriteError(w, r, r.Context().Err())
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			verifier, err := auth.New(auth.Config{Service: "test", Credentials: []auth.Credential{credential}})
			if err != nil {
				t.Fatal(err)
			}
			handler := test.handler
			if handler == nil {
				handler = func(http.ResponseWriter, *http.Request) { t.Error("rejected handler ran") }
			}
			var quota interface {
				Allow(context.Context, string) (bool, error)
			}
			if test.quota != nil {
				quota = test.quota
			}
			server, err := New("test", Config{RequestTimeout: 50 * time.Millisecond}, handler, verifier, quota, health.New())
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("GET", "/"+test.name, nil)
			r.Header.Set("X-Request-Id", "request-"+test.name)
			if test.signed {
				if err := auth.SignHTTP(r, "test", credential, time.Now(), test.name); err != nil {
					t.Fatal(err)
				}
			}
			w := httptest.NewRecorder()
			server.ServeHTTP(w, r)
			if w.Code != test.status || w.Header().Get("X-Request-Id") != "request-"+test.name {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if test.status >= 400 && !strings.Contains(w.Body.String(), test.code) {
				t.Fatal("missing common error code")
			}
		})
	}
	log.Sync()
	var contents strings.Builder
	if err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		contents.Write(body)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"duration_ms":`, `"app_key":"caller"`, `"app_key":""`, `"error_code":"INTERNAL"`, `"error_code":"UNAUTHENTICATED"`, `"request_id":"request-panic"`} {
		if !strings.Contains(contents.String(), field) {
			t.Fatalf("missing access field %s", field)
		}
	}
	for _, secret := range []string{credential.Secret, "private-store-detail", "private-panic-detail"} {
		if strings.Contains(contents.String(), secret) {
			t.Fatal("sensitive request detail entered access log")
		}
	}
}

func TestInflightBoundAndHealthBypass(t *testing.T) {
	credential := auth.Credential{AppKey: "caller", KeyID: "key", Secret: strings.Repeat("s", 32)}
	verifier, err := auth.New(auth.Config{Service: "test", Credentials: []auth.Credential{credential}})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	state := health.New()
	server, err := New("test", Config{MaxInflight: 1}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(entered); <-release }), verifier, nil, state)
	if err != nil {
		t.Fatal(err)
	}
	first := httptest.NewRequest("GET", "/protected", nil)
	if err := auth.SignHTTP(first, "test", credential, time.Now(), "first"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() { server.ServeHTTP(httptest.NewRecorder(), first) })
	<-entered
	second := httptest.NewRecorder()
	server.ServeHTTP(second, httptest.NewRequest("GET", "/protected", nil))
	if second.Code != 429 {
		t.Errorf("inflight bound=%d", second.Code)
	}
	for path, expected := range map[string]int{"/livez": 200, "/readyz": 503} {
		w := httptest.NewRecorder()
		server.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != expected {
			t.Errorf("probe %s=%d", path, w.Code)
		}
	}
	close(release)
	wg.Wait()
}

func TestRemoteListenerRequiresTLSAndBodyBound(t *testing.T) {
	credential := auth.Credential{AppKey: "caller", KeyID: "key", Secret: strings.Repeat("s", 32)}
	verifier, err := auth.New(auth.Config{Service: "test", MaxBodyBytes: 8, Credentials: []auth.Credential{credential}})
	if err != nil {
		t.Fatal(err)
	}
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("oversized handler ran") })
	if _, err := New("test", Config{Addr: "0.0.0.0:8080"}, handler, verifier, nil, health.New()); err == nil {
		t.Fatal("remote plaintext accepted")
	}
	server, err := New("test", Config{MaxBodyBytes: 8}, handler, verifier, nil, health.New())
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/upload", strings.NewReader("123456789"))
	if err := auth.SignHTTP(r, "test", credential, time.Now(), "oversize"); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatalf("body bound=%d", w.Code)
	}
}
