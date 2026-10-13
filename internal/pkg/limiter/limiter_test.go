package limiter_test

import (
	"crypto/rand"
	"github.com/gtkit/gsrv/internal/pkg/limiter"
	"testing"
)

func TestIsolationAndExplicitDisable(t *testing.T) {
	limit, err := limiter.NewLocal(rand.Text(), 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key  string
		want bool
	}{{"one", true}, {"one", false}, {"two", true}} {
		got, err := limit.Allow(t.Context(), tc.key)
		if err != nil || got != tc.want {
			t.Fatalf("key=%s got=%v err=%v", tc.key, got, err)
		}
	}
	if _, err := limit.Allow(t.Context(), ""); err == nil {
		t.Fatal("untrusted key accepted")
	}
	unlimited, _ := limiter.NewLocal(rand.Text(), 0)
	for range 100 {
		if ok, err := unlimited.Allow(t.Context(), "caller"); !ok || err != nil {
			t.Fatal("explicit disable failed")
		}
	}
	if _, err := limiter.NewLocal("bad", -1); err == nil {
		t.Fatal("negative accepted")
	}
}
