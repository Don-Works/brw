package browser

import (
	"context"
	"errors"
	"testing"
	"time"
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

func TestRetryAssertAfterNavigation(t *testing.T) {
	attempts := 0
	err := retryAssertAfterNavigation(context.Background(), time.Second, func(remaining time.Duration) error {
		attempts++
		if remaining <= 0 {
			t.Fatal("retry received an expired deadline")
		}
		if attempts == 1 {
			return errors.New("Execution context was destroyed. (-32000)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retry transient navigation: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}

	want := errors.New("permanent assertion failure")
	attempts = 0
	err = retryAssertAfterNavigation(context.Background(), time.Second, func(time.Duration) error {
		attempts++
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("non-transient error = %v, want %v", err, want)
	}
	if attempts != 1 {
		t.Fatalf("non-transient attempts = %d, want 1", attempts)
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	attempts = 0
	err = retryAssertAfterNavigation(cancelCtx, time.Second, func(time.Duration) error {
		attempts++
		cancel()
		return errors.New("frame was detached")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled retry error = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("cancelled attempts = %d, want 1", attempts)
	}
}
