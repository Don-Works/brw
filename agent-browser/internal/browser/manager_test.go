package browser

import (
	"errors"
	"testing"
)

func TestIsTransientNavigationError(t *testing.T) {
	for _, msg := range []string{
		"Execution context was destroyed. (-32000)",
		"Cannot find context with specified id",
		"frame was detached",
	} {
		if !isTransientNavigationError(errors.New(msg)) {
			t.Fatalf("expected transient navigation error for %q", msg)
		}
	}
	if isTransientNavigationError(errors.New("node is detached from document")) {
		t.Fatal("detached DOM nodes should not be treated as navigation retry errors")
	}
}
