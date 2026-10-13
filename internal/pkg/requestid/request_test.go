package requestid

import (
	"strings"
	"sync"
	"testing"
)

func TestSafeIDAndConcurrentRecord(t *testing.T) {
	for _, id := range []string{"", "a\nb", "id space", "中文", strings.Repeat("x", 129)} {
		if ValidID(id) || !ValidID(New(id).ID()) {
			t.Fatalf("unsafe ID accepted: %q", id)
		}
	}
	scope := New("caller-request")
	ctx := WithScope(t.Context(), scope)
	if FromContext(ctx) != scope || scope.ID() != "caller-request" || FromContext(t.Context()) != nil {
		t.Fatal("scope propagation")
	}
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			scope.Authenticate("caller")
			scope.MarkDegraded()
			_ = scope.AppKey()
			_ = scope.Degraded()
			_ = LogFields(ctx)
		})
	}
	wg.Wait()
	if scope.AppKey() != "caller" || !scope.Degraded() {
		t.Fatal("record lost")
	}
}
