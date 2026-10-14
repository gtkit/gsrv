package auth_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
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

func TestNonceReservationsRotationAndExpiration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := credentials()
		b := a
		b.AppKey = "other"
		rotated := a
		rotated.KeyID = "rotated"
		v, err := auth.New(auth.Config{Service: "test", Credentials: []auth.Credential{a, b, rotated}, NonceCapacity: 2})
		if err != nil {
			t.Fatal(err)
		}
		call := func(credential auth.Credential, nonce string) fault.Code {
			r := httptest.NewRequest("GET", "/check", nil)
			if err := auth.SignHTTP(r, "test", credential, time.Now(), nonce); err != nil {
				t.Fatal(err)
			}
			_, err := v.AuthenticateHTTP(r)
			return fault.CodeOf(err)
		}
		for _, tc := range []struct {
			credential auth.Credential
			nonce      string
			code       fault.Code
		}{
			{a, "first", fault.OK}, {rotated, "second", fault.ResourceExhausted}, {b, "first", fault.OK},
			{rotated, "first", fault.Unauthenticated}, {b, "second", fault.ResourceExhausted},
		} {
			if got := call(tc.credential, tc.nonce); got != tc.code {
				t.Fatalf("got=%s want=%s", got, tc.code)
			}
		}
		if used, cap, ok := v.NonceStats(); used != 2 || cap != 2 || !ok {
			t.Fatal("occupancy/capacity mismatch")
		}
		synctest.Sleep(32 * time.Second)
		if used, _, _ := v.NonceStats(); used != 0 {
			t.Fatal("expired occupancy retained")
		}
		if code := call(rotated, "third"); code != fault.OK {
			t.Fatal("expired reservation not freed")
		}
	})
}

func TestNonceReservationConcurrentAndHTTPOnlyCount(t *testing.T) {
	a := credentials()
	b := a
	b.AppKey = "other"
	v, err := auth.New(auth.Config{Service: "test", Credentials: []auth.Credential{a, b}, NonceCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	var success [2]atomic.Int32
	var wg sync.WaitGroup
	for i, credential := range []auth.Credential{a, b} {
		for range 50 {
			wg.Go(func() {
				r := httptest.NewRequest("GET", "/check", nil)
				if err := auth.SignHTTP(r, "test", credential, time.Now(), rand.Text()); err != nil {
					t.Error(err)
					return
				}
				_, err := v.AuthenticateHTTP(r)
				if err == nil {
					success[i].Add(1)
				} else if fault.CodeOf(err) != fault.ResourceExhausted {
					t.Error(err)
				}
			})
		}
	}
	wg.Wait()
	if success[0].Load() != 1 || success[1].Load() != 1 {
		t.Fatal("caller reservations not isolated/atomic")
	}
	b.Secret = ""
	if _, err := auth.New(auth.Config{Service: "test", Credentials: []auth.Credential{a, b}, NonceCapacity: 1}); err != nil {
		t.Fatal("Bearer-only key consumed HTTP reservation", err)
	}
}

type nonceResult struct {
	used bool
	err  error
}

func (s nonceResult) Use(context.Context, string, time.Time) (bool, error) { return s.used, s.err }

func TestNonceConfigAndCustomStores(t *testing.T) {
	a := credentials()
	b := a
	b.AppKey = "other"
	var typedNil *auth.MemoryNonceStore
	for _, store := range []auth.NonceStore{typedNil, new(auth.MemoryNonceStore)} {
		if _, err := auth.New(auth.Config{Service: "test", Credentials: []auth.Credential{a}, Nonces: store}); err == nil {
			t.Fatal("invalid memory store accepted")
		}
	}
	for _, tc := range []struct {
		name       string
		total, per int
		valid      bool
	}{
		{"default", 0, 0, true}, {"explicit", 4, 1, true}, {"too small", 1, 0, false}, {"overcommitted", 4, 3, false},
		{"negative", -1, 0, false}, {"negative caller", 4, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := auth.New(auth.Config{Service: "test", Credentials: []auth.Credential{a, b}, NonceCapacity: tc.total, NonceCapacityPerAppKey: tc.per})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	for _, tc := range []struct {
		store nonceResult
		code  fault.Code
	}{
		{nonceResult{used: true}, fault.OK}, {nonceResult{}, fault.Unauthenticated},
		{nonceResult{err: errors.New("store failed")}, fault.Unavailable},
		{nonceResult{err: context.DeadlineExceeded}, fault.DeadlineExceeded}, {nonceResult{err: context.Canceled}, fault.Canceled},
	} {
		v, err := auth.New(auth.Config{Service: "test", Credentials: []auth.Credential{a}, Nonces: tc.store})
		if err != nil {
			t.Fatal(err)
		}
		_, err = v.AuthenticateHTTP(signed(t, time.Now()))
		if fault.CodeOf(err) != tc.code {
			t.Fatalf("store code=%s want=%s", fault.CodeOf(err), tc.code)
		}
		if _, _, ok := v.NonceStats(); ok {
			t.Fatal("custom store fabricated occupancy")
		}
	}
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
