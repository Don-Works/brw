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

type fakeBiDi struct {
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

	missCtx, missCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer missCancel()
	if _, err := c.Await(missCtx, "browsingContext.userPromptOpened", nil); err == nil {
		t.Fatal("Await returned an event for a method that never arrived")
	}
}

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
