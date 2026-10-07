package mcp

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestServeExitsOnContextCancelWhileBlockedOnRead(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- New(fakeController{}).Serve(ctx, pr, io.Discard) }()

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after context cancellation while blocked on stdin read")
	}
}

func TestServeIdleExitWithNoTraffic(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()

	s := New(fakeController{})
	s.SetIdleExit(100 * time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background(), pr, io.Discard) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrIdleExit) {
			t.Fatalf("Serve = %v, want ErrIdleExit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not idle-exit with no traffic")
	}
}

func TestServeIdleExitResetByTraffic(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()

	s := New(fakeController{})
	s.SetIdleExit(300 * time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background(), pr, io.Discard) }()

	for i := 0; i < 6; i++ {
		if _, err := pw.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")); err != nil {
			t.Fatalf("write request %d: %v", i, err)
		}
		select {
		case err := <-done:
			t.Fatalf("Serve exited during active traffic: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}

	select {
	case err := <-done:
		if !errors.Is(err, ErrIdleExit) {
			t.Fatalf("Serve = %v, want ErrIdleExit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not idle-exit after traffic stopped")
	}
}
