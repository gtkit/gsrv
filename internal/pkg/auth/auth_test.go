package auth_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gtkit/gsrv/internal/pkg/auth"
	"github.com/gtkit/gsrv/internal/pkg/fault"
)

func credentials() auth.Credential {
	return auth.Credential{AppKey: "caller", KeyID: "primary", Secret: strings.Repeat("s", 32), Token: strings.Repeat("t", 32)}
}
func verifier(t *testing.T) *auth.Verifier {
	t.Helper()
	v, err := auth.New(auth.Config{Service: "test", Credentials: []auth.Credential{credentials()}})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func signed(t *testing.T, at time.Time) *http.Request {
	t.Helper()
	r := httptest.NewRequest("POST", "http://example.com/v1/test?b=2&a=1", strings.NewReader("{\"name\":\"demo\"}"))
	r.Header.Set("Content-Type", "application/json")
	if err := auth.SignHTTP(r, "test", credentials(), at, rand.Text()); err != nil {
		t.Fatal(err)
	}
	return r
}
func TestHTTPAuthentication(t *testing.T) {
	cases := []struct {
		name   string
		change func(*http.Request)
		code   fault.Code
	}{
		{"success", func(*http.Request) {}, fault.OK},
		{"body", func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader("{}")) }, fault.Unauthenticated},
		{"method", func(r *http.Request) { r.Method = "PUT" }, fault.Unauthenticated},
		{"path", func(r *http.Request) { r.URL.Path = "/other" }, fault.Unauthenticated},
		{"query", func(r *http.Request) { r.URL.RawQuery = "a=1&b=2" }, fault.Unauthenticated},
		{"content-type", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, fault.Unauthenticated},
		{"app-key", func(r *http.Request) { r.Header.Set(auth.AppKeyHeader, "another") }, fault.Unauthenticated},
		{"duplicate-header", func(r *http.Request) { r.Header.Add(auth.AppKeyHeader, "caller") }, fault.Unauthenticated},
		{"malformed-signature", func(r *http.Request) { r.Header.Set(auth.SignatureHeader, "invalid") }, fault.Unauthenticated},
		{"method-override", func(r *http.Request) { r.Header.Set("X-HTTP-Method-Override", "GET") }, fault.Unauthenticated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := verifier(t)
			r := signed(t, time.Now())
			tc.change(r)
			identity, err := v.AuthenticateHTTP(r)
			if fault.CodeOf(err) != tc.code {
				t.Fatalf("code=%s want=%s", fault.CodeOf(err), tc.code)
			}
			if err == nil {
				if identity.AppKey != "caller" {
					t.Fatal("unverified identity")
				}
				body, _ := io.ReadAll(r.Body)
				if !bytes.Equal(body, []byte("{\"name\":\"demo\"}")) {
					t.Fatal("body not restored")
				}
			}
		})
	}
}
func TestReplayTimestampAndBodyLimit(t *testing.T) {
	v := verifier(t)
	r := signed(t, time.Now())
	if _, err := v.AuthenticateHTTP(r); err != nil {
		t.Fatal(err)
	}
	if _, err := v.AuthenticateHTTP(r); fault.CodeOf(err) != fault.Unauthenticated {
		t.Fatal("replay accepted")
	}
	for _, offset := range []time.Duration{-31 * time.Second, 31 * time.Second} {
		if _, err := v.AuthenticateHTTP(signed(t, time.Now().Add(offset))); fault.CodeOf(err) != fault.Unauthenticated {
			t.Fatal("expired or future timestamp accepted")
		}
	}
	v, err := auth.New(auth.Config{Service: "test", Credentials: []auth.Credential{credentials()}, MaxBodyBytes: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.AuthenticateHTTP(signed(t, time.Now())); fault.CodeOf(err) != fault.PayloadTooLarge {
		t.Fatal("unbounded body accepted")
	}
}
func TestTokenBindingAndInvalidConfiguration(t *testing.T) {
	v := verifier(t)
	for _, tc := range []struct {
		key, token string
		ok         bool
	}{{"caller", credentials().Token, true}, {"another", credentials().Token, false}, {"caller", "", false}} {
		_, err := v.AuthenticateToken(t.Context(), tc.key, tc.token)
		if (err == nil) != tc.ok {
			t.Fatalf("key=%s ok=%v", tc.key, tc.ok)
		}
	}
	for _, cfg := range []auth.Config{{}, {Service: "test"}, {Service: "test", Credentials: []auth.Credential{{AppKey: "a", KeyID: "b", Secret: "short"}}}} {
		if _, err := auth.New(cfg); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
}
func TestMemoryNonceAtomicCapacityAndExpiry(t *testing.T) {
	store, err := auth.NewMemoryNonceStore(1)
	if err != nil {
		t.Fatal(err)
	}
	var success atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			ok, err := store.Use(t.Context(), "shared", time.Now().Add(time.Minute))
			if err != nil {
				t.Error(err)
			}
			if ok {
				success.Add(1)
			}
		})
	}
	wg.Wait()
	if success.Load() != 1 {
		t.Fatalf("concurrent nonce successes=%d", success.Load())
	}
	if _, err := store.Use(t.Context(), "second", time.Now().Add(time.Minute)); err == nil {
		t.Fatal("capacity exceeded")
	}
	synctest.Test(t, func(t *testing.T) {
		store, _ := auth.NewMemoryNonceStore(1)
		if ok, err := store.Use(t.Context(), "first", time.Now().Add(time.Second)); !ok || err != nil {
			t.Fatal(err)
		}
		synctest.Sleep(2 * time.Second)
		if ok, err := store.Use(t.Context(), "second", time.Now().Add(time.Second)); !ok || err != nil {
			t.Fatal("expired capacity not reclaimed")
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.Use(ctx, "canceled", time.Now().Add(time.Minute)); err == nil {
		t.Fatal("cancellation ignored")
	}
}
