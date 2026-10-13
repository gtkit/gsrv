package fault

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestMappingPreservesCauseAndHidesUnknownDetails(t *testing.T) {
	cause := errors.New("private detail")
	wrapped := fmt.Errorf("outer: %w", Wrap(Unavailable, "temporarily unavailable", cause))
	if !errors.Is(wrapped, cause) || CodeOf(wrapped) != Unavailable || Message(wrapped) != "temporarily unavailable" {
		t.Fatal("wrapped contract lost")
	}
	for _, test := range []struct {
		err     error
		code    Code
		message string
	}{
		{cause, Internal, "internal server error"},
		{fmt.Errorf("timeout: %w", context.DeadlineExceeded), DeadlineExceeded, "deadline exceeded"},
		{context.Canceled, Canceled, "request canceled"},
		{New(NotFound, "missing"), NotFound, "missing"},
	} {
		if CodeOf(test.err) != test.code || Message(test.err) != test.message {
			t.Fatalf("mapping %v", test.code)
		}
	}
	if CodeOf(nil) != OK {
		t.Fatal("nil must be OK")
	}
}
