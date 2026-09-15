package mcp

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// The daemon's --idle-exit watcher lives on its HTTP server and sees only the
// HTTP mux. A request handled here has to reach it, or a daemon serving an
// agent over stdio counts that agent as silence and exits.
func TestEveryRequestIsReportedToTheActivityHook(t *testing.T) {
	server := New(fakeController{})

	var mu sync.Mutex
	var started, finished int
	server.SetActivityHook(func() func() {
		mu.Lock()
		started++
		mu.Unlock()
		return func() {
			mu.Lock()
			finished++
			mu.Unlock()
		}
	})

	input := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
	}, "\n") + "\n")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Serve(ctx, input, io.Discard); err != nil {
		t.Fatalf("serve: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if started != 3 {
		t.Fatalf("the hook saw %d requests, want 3", started)
	}
	if finished != started {
		t.Fatalf("the hook was told %d requests started and %d finished; an unbalanced count pins the daemon awake forever", started, finished)
	}
}

// A server with no hook is the ordinary case and must not reach for one.
func TestNoActivityHookIsFine(t *testing.T) {
	server := New(fakeController{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
	if err := server.Serve(ctx, input, io.Discard); err != nil {
		t.Fatalf("serve: %v", err)
	}
}

// A hook that returns nil must not panic the server: the daemon wires its own,
// but the field is exported API.
func TestAnActivityHookThatReturnsNothingIsTolerated(t *testing.T) {
	server := New(fakeController{})
	server.SetActivityHook(func() func() { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
	if err := server.Serve(ctx, input, io.Discard); err != nil {
		t.Fatalf("serve: %v", err)
	}
}
