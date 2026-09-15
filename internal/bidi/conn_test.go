package bidi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeBiDi is a BiDi endpoint whose answers the test dictates. It exists so the
// protocol rules below are checked on a machine with no browser installed: the
// live Firefox tests skip there, and a settle machinery that loses an event or
// mismatches a response would then go unmeasured everywhere.
type fakeBiDi struct {
	// handle answers one command. Returning a nil reply sends nothing, which is
	// how the "the socket dropped mid-command" case is staged.
	handle func(conn *websocket.Conn, id uint64, method string, params json.RawMessage)
}

func (f *fakeBiDi) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var in struct {
				ID     uint64          `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(data, &in); err != nil {
				return
			}
			f.handle(conn, in.ID, in.Method, in.Params)
		}
	}))
	t.Cleanup(srv.Close)
	return "ws://" + strings.TrimPrefix(srv.URL, "http://")
}

func send(t *testing.T, conn *websocket.Conn, payload map[string]any) {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Errorf("marshal fake reply: %v", err)
		return
	}
	if err := conn.Write(context.Background(), websocket.MessageText, data); err != nil {
		t.Logf("fake write: %v", err)
	}
}

// A BiDi client sends several commands before any answer comes back, so a reply
// has to be matched by id. Answering in reverse order is what tells a matched
// response apart from one that merely happened to arrive next.
func TestCommandsAreMatchedByID(t *testing.T) {
	var held []uint64
	var conns []*websocket.Conn
	release := make(chan struct{})
	fake := &fakeBiDi{}
	fake.handle = func(conn *websocket.Conn, id uint64, method string, params json.RawMessage) {
		held = append(held, id)
		conns = append(conns, conn)
		if len(held) < 3 {
			return
		}
		// Answer last-first, each with its own id so a correct client cannot
		// mistake one for another.
		for i := len(held) - 1; i >= 0; i-- {
			send(t, conns[i], map[string]any{
				"type":   "success",
				"id":     held[i],
				"result": map[string]any{"which": held[i]},
			})
		}
		close(release)
	}
	url := fake.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Dial(ctx, url)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	type answer struct {
		Which uint64 `json:"which"`
	}
	results := make(chan answer, 3)
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			var out answer
			if err := c.Command(ctx, "probe.method", map[string]any{}, &out); err != nil {
				errs <- err
				return
			}
			results <- out
		}()
	}
	<-release
	seen := map[uint64]bool{}
	for i := 0; i < 3; i++ {
		select {
		case got := <-results:
			if seen[got.Which] {
				t.Fatalf("id %d answered two commands", got.Which)
			}
			seen[got.Which] = true
		case err := <-errs:
			t.Fatalf("command failed: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("a command never received its answer")
		}
	}
	if len(seen) != 3 {
		t.Fatalf("saw %d distinct answers, want 3", len(seen))
	}
}

// The whole capability question is answered by error codes, so an unimplemented
// command must be distinguishable from a rejected argument. A client that
// collapsed both into "error" would report every missing primitive as a bug in
// the caller.
func TestErrorCodesAreDistinguishable(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        string
		wantUnknown bool
	}{
		{name: "unimplemented command", code: "unknown command", wantUnknown: true},
		{name: "bad arguments", code: "invalid argument", wantUnknown: false},
		{name: "stale realm", code: "no such frame", wantUnknown: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeBiDi{handle: func(conn *websocket.Conn, id uint64, method string, params json.RawMessage) {
				send(t, conn, map[string]any{
					"type":    "error",
					"id":      id,
					"error":   tc.code,
					"message": "from the fake",
				})
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			c, err := Dial(ctx, fake.start(t))
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.Close()
			err = c.Command(ctx, "some.command", map[string]any{}, nil)
			if err == nil {
				t.Fatal("an error response produced no error")
			}
			if got := IsUnknownCommand(err); got != tc.wantUnknown {
				t.Fatalf("IsUnknownCommand(%v) = %v, want %v", err, got, tc.wantUnknown)
			}
			if !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("error %q does not name the BiDi error code %q", err, tc.code)
			}
		})
	}
}

// An event that arrives between the action and the wait must still satisfy the
// wait. brw resolves a wait from the event stream rather than by polling, so a
// client that only delivered events to a waiter already blocked would drop
// exactly the load event a fast navigation produces.
func TestAwaitSeesEventsThatArrivedBeforeTheCall(t *testing.T) {
	fake := &fakeBiDi{handle: func(conn *websocket.Conn, id uint64, method string, params json.RawMessage) {
		send(t, conn, map[string]any{"type": "success", "id": id, "result": map[string]any{}})
		send(t, conn, map[string]any{
			"type":   "event",
			"method": "browsingContext.load",
			"params": map[string]any{"url": "http://example.invalid/loaded"},
		})
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Dial(ctx, fake.start(t))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if err := c.Command(ctx, "browsingContext.navigate", map[string]any{}, nil); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	// Give the event time to land before anything waits for it.
	deadline := time.Now().Add(5 * time.Second)
	for len(c.Events()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(c.Events()) == 0 {
		t.Fatal("the fake never delivered its event")
	}

	waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Second)
	defer waitCancel()
	got, err := c.Await(waitCtx, "browsingContext.load", nil)
	if err != nil {
		t.Fatalf("Await lost an event that arrived before the wait: %v", err)
	}
	if !strings.Contains(string(got.Params), "/loaded") {
		t.Fatalf("awaited event params = %s", got.Params)
	}

	// A method nobody sent must still time out, or Await would be answering
	// every question with the first event it holds.
	missCtx, missCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer missCancel()
	if _, err := c.Await(missCtx, "browsingContext.userPromptOpened", nil); err == nil {
		t.Fatal("Await returned an event for a method that never arrived")
	}
}

// A browser that dies mid-command has to fail the command. Waiting out the
// context instead turns a crashed browser into a timeout, which reads as a slow
// page and gets retried.
func TestCommandFailsWhenTheSocketDrops(t *testing.T) {
	fake := &fakeBiDi{handle: func(conn *websocket.Conn, id uint64, method string, params json.RawMessage) {
		_ = conn.Close(websocket.StatusGoingAway, "browser exited")
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Dial(ctx, fake.start(t))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	done := make(chan error, 1)
	go func() { done <- c.Command(ctx, "script.callFunction", map[string]any{}, nil) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a command that lost its socket reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a command outlived its socket instead of failing")
	}
}
