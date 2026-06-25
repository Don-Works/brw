package extensionbridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// concExtension is a controllable stand-in for the Chrome extension service
// worker used to exercise the bridge's concurrency protections. Unlike
// fakeExtension it processes each RPC in its own goroutine (modelling the real
// worker's async chrome.debugger calls) and records how many requests — overall
// and per tab — are in flight at once, so a test can assert the bridge's
// in-flight cap and per-tab serialization actually hold.
type concExtension struct {
	mu        sync.Mutex
	cur       int            // requests currently being processed
	maxCur    int            // peak concurrent requests observed
	handled   int            // total requests fully processed
	perTabCur map[string]int // concurrent requests per tab id
	perTabMax map[string]int // peak concurrent per tab id

	writeMu sync.Mutex // coder/websocket writes are not concurrency-safe

	// gate, when non-nil, blocks every handler until the test sends it a token,
	// so a test can pin requests "in flight" and observe the steady state.
	gate chan struct{}

	// failFirst names RPC types whose FIRST occurrence replies as if the socket
	// dropped ("extension disconnected"), to drive the transient-drop retry path.
	failFirst  map[string]bool
	failedOnce map[string]bool
}

func newConcExtension() *concExtension {
	return &concExtension{
		perTabCur:  map[string]int{},
		perTabMax:  map[string]int{},
		failedOnce: map[string]bool{},
	}
}

func (e *concExtension) enter(tab string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cur++
	if e.cur > e.maxCur {
		e.maxCur = e.cur
	}
	if tab != "" {
		e.perTabCur[tab]++
		if e.perTabCur[tab] > e.perTabMax[tab] {
			e.perTabMax[tab] = e.perTabCur[tab]
		}
	}
}

func (e *concExtension) exit(tab string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cur--
	e.handled++
	if tab != "" {
		e.perTabCur[tab]--
	}
}

func (e *concExtension) snapshot() (cur, peak, handled int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cur, e.maxCur, e.handled
}

func (e *concExtension) perTabPeak(tab string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.perTabMax[tab]
}

func (e *concExtension) serve(ctx context.Context, conn *websocket.Conn) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg struct {
			ID     string         `json:"id"`
			Type   string         `json:"type"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		go e.handle(ctx, conn, msg.ID, msg.Type, msg.Params)
	}
}

func (e *concExtension) handle(ctx context.Context, conn *websocket.Conn, id, typ string, params map[string]any) {
	tab := tabKeyFromParams(params)
	e.enter(tab)
	defer e.exit(tab)

	e.mu.Lock()
	failNow := e.failFirst[typ] && !e.failedOnce[typ]
	if failNow {
		e.failedOnce[typ] = true
	}
	e.mu.Unlock()
	if failNow {
		// Model a socket drop: reply with the disconnect-drain marker the bridge
		// treats as a transient transport failure.
		e.reply(ctx, conn, id, false, disconnectDrainReason, nil)
		return
	}

	if e.gate != nil {
		select {
		case <-e.gate:
		case <-ctx.Done():
			return
		}
	}
	e.reply(ctx, conn, id, true, "", map[string]any{})
}

func (e *concExtension) reply(ctx context.Context, conn *websocket.Conn, id string, ok bool, errMsg string, result any) {
	payload := map[string]any{"id": id, "ok": ok}
	if errMsg != "" {
		payload["error"] = errMsg
	}
	if result != nil {
		payload["result"] = result
	}
	data, _ := json.Marshal(payload)
	e.writeMu.Lock()
	_ = conn.Write(ctx, websocket.MessageText, data)
	e.writeMu.Unlock()
}

// startConcExtension stands up the bridge's /extension server and returns a
// connect func that dials the extension and begins serving (split out so a test
// can issue calls BEFORE the socket exists, exercising the reconnect-wait path).
func startConcExtension(t *testing.T, b *Bridge, e *concExtension) (connect func(), cleanup func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	serveCtx, serveCancel := context.WithCancel(context.Background())
	var conn *websocket.Conn
	connect = func() {
		dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dialCancel()
		c, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
			HTTPHeader: http.Header{"Origin": []string{testDefaultOrigin}},
		})
		if err != nil {
			srv.Close()
			t.Fatalf("dial bridge: %v", err)
		}
		conn = c
		waitUntil(t, func() bool {
			b.mu.RLock()
			defer b.mu.RUnlock()
			return b.conn != nil
		})
		go e.serve(serveCtx, conn)
	}
	cleanup = func() {
		serveCancel()
		if conn != nil {
			_ = conn.Close(websocket.StatusNormalClosure, "test done")
		}
		srv.Close()
	}
	return connect, cleanup
}

// drain releases n gated handlers, one token at a time.
func drain(t *testing.T, e *concExtension, n int) {
	t.Helper()
	go func() {
		for i := 0; i < n; i++ {
			select {
			case e.gate <- struct{}{}:
			case <-time.After(2 * time.Second):
				return
			}
		}
	}()
}

// TestBridgeBoundsConcurrentInflight proves the backpressure valve: with the cap
// at 2, no more than 2 RPCs ever reach the extension at once however many agents
// fire together — the rest queue and are served as slots free, none dropped.
func TestBridgeBoundsConcurrentInflight(t *testing.T) {
	b := New("", 5*time.Second, "")
	b.SetMaxInflight(2)
	e := newConcExtension()
	e.gate = make(chan struct{})
	connect, cleanup := startConcExtension(t, b, e)
	connect()
	defer cleanup()

	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = b.call(context.Background(), "work", nil)
		}(i)
	}

	// Exactly cap=2 requests reach the extension; the other 4 queue at the sema.
	waitUntil(t, func() bool { cur, _, _ := e.snapshot(); return cur == 2 })
	waitUntil(t, func() bool { return b.queued.Load() >= int64(n-2) })
	time.Sleep(100 * time.Millisecond)
	if _, peak, _ := e.snapshot(); peak != 2 {
		t.Fatalf("peak concurrent in-flight = %d, want 2 (the cap)", peak)
	}

	drain(t, e, n)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d failed: %v", i, err)
		}
	}
	if _, peak, handled := e.snapshot(); peak != 2 || handled != n {
		t.Fatalf("peak=%d handled=%d, want peak=2 handled=%d", peak, handled, n)
	}
	if drops := b.busyDrops.Load(); drops != 0 {
		t.Fatalf("busy_drops = %d, want 0 (all calls should have queued, not dropped)", drops)
	}
}

// TestBridgeBusyWhenSaturated proves a call that cannot get a slot before its
// deadline fails fast with ErrBridgeBusy (a backpressure signal) rather than
// hanging or returning an opaque timeout.
func TestBridgeBusyWhenSaturated(t *testing.T) {
	// Generous bridge timeout so the slot-holder keeps the only slot; the waiter
	// supplies its own short deadline to force the busy path deterministically.
	b := New("", 5*time.Second, "")
	b.SetMaxInflight(1)
	e := newConcExtension()
	e.gate = make(chan struct{})
	connect, cleanup := startConcExtension(t, b, e)
	connect()
	defer cleanup()

	// Occupy the single slot with a handler parked on the gate.
	go func() { _, _ = b.call(context.Background(), "hold", nil) }()
	waitUntil(t, func() bool { cur, _, _ := e.snapshot(); return cur == 1 })

	// A second call with a short deadline can never get a slot -> ErrBridgeBusy.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := b.call(ctx, "work", nil)
	if !errors.Is(err, ErrBridgeBusy) {
		t.Fatalf("saturated call err = %v, want ErrBridgeBusy", err)
	}
	if b.busyDrops.Load() == 0 {
		t.Fatal("busy_drops should have incremented")
	}
	drain(t, e, 1) // release the holder so cleanup is clean
}

// TestBridgeSerializesSameTab proves two operations on the SAME tab never run
// concurrently (the interleaving that corrupts in-page ref state and yields
// stale refs), even when the in-flight cap would otherwise allow it.
func TestBridgeSerializesSameTab(t *testing.T) {
	b := New("", 5*time.Second, "")
	b.SetMaxInflight(8) // ensure the sema is not the limiter
	e := newConcExtension()
	e.gate = make(chan struct{})
	connect, cleanup := startConcExtension(t, b, e)
	connect()
	defer cleanup()

	const n = 4
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = b.call(context.Background(), "work", map[string]any{"tabId": 7})
		}()
	}

	// Only one op on tab 7 is ever in flight; the rest wait on the tab lock.
	waitUntil(t, func() bool { cur, _, _ := e.snapshot(); return cur == 1 })
	time.Sleep(100 * time.Millisecond)
	if peak := e.perTabPeak("7"); peak != 1 {
		t.Fatalf("peak concurrent ops on tab 7 = %d, want 1 (serialized)", peak)
	}

	drain(t, e, n)
	wg.Wait()
	if peak := e.perTabPeak("7"); peak != 1 {
		t.Fatalf("after drain, peak on tab 7 = %d, want 1", peak)
	}
	if _, _, handled := e.snapshot(); handled != n {
		t.Fatalf("handled = %d, want %d", handled, n)
	}
}

// TestBridgeRunsDistinctTabsInParallel is the counterpart: per-tab serialization
// must NOT serialize across different tabs — distinct tabs run concurrently up to
// the cap. If it wrongly serialized everything, cur would never reach n and the
// waitUntil would time out.
func TestBridgeRunsDistinctTabsInParallel(t *testing.T) {
	b := New("", 5*time.Second, "")
	b.SetMaxInflight(8)
	e := newConcExtension()
	e.gate = make(chan struct{})
	connect, cleanup := startConcExtension(t, b, e)
	connect()
	defer cleanup()

	const n = 4
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = b.call(context.Background(), "work", map[string]any{"tabId": 100 + i})
		}(i)
	}

	// Distinct tabs each hold their own lock -> all n overlap.
	waitUntil(t, func() bool { cur, _, _ := e.snapshot(); return cur == n })
	drain(t, e, n)
	wg.Wait()
}

// TestBridgeWithoutTabLockBypassesSerialization proves the settle-probe exemption:
// a withoutTabLock call runs even while another op holds the same tab's lock, so
// an abandoned fingerprint probe can never stall the next foreground action.
func TestBridgeWithoutTabLockBypassesSerialization(t *testing.T) {
	b := New("", 5*time.Second, "")
	b.SetMaxInflight(8)
	e := newConcExtension()
	e.gate = make(chan struct{})
	connect, cleanup := startConcExtension(t, b, e)
	connect()
	defer cleanup()

	// Foreground op holds tab 5's lock, parked on the gate.
	go func() { _, _ = b.call(context.Background(), "work", map[string]any{"tabId": 5}) }()
	waitUntil(t, func() bool { cur, _, _ := e.snapshot(); return cur == 1 })

	// An exempt probe on the SAME tab must not wait on the lock -> cur reaches 2.
	go func() {
		_, _ = b.call(withoutTabLock(context.Background()), "work", map[string]any{"tabId": 5})
	}()
	waitUntil(t, func() bool { cur, _, _ := e.snapshot(); return cur == 2 })

	drain(t, e, 2)
}

// TestBridgeWaitsForReconnectInsteadOfFailing proves a call arriving while the
// MV3 worker is momentarily disconnected parks for the reconnect instead of
// failing fast with "not connected" — the spurious-disconnect class of errors.
func TestBridgeWaitsForReconnectInsteadOfFailing(t *testing.T) {
	b := New("", 3*time.Second, "")
	e := newConcExtension()
	connect, cleanup := startConcExtension(t, b, e)
	defer cleanup()
	// Deliberately do NOT connect yet: the socket is down when the call is made.

	done := make(chan error, 1)
	go func() {
		_, err := b.call(context.Background(), "list_tabs", nil)
		done <- err
	}()

	// The call must still be parked (not failed) while disconnected.
	time.Sleep(150 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("call returned %v before reconnect; it should have waited", err)
	default:
	}

	connect()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("call should have ridden out the reconnect gap: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("call did not complete after reconnect")
	}
}

// TestBridgeRetriesIdempotentReadAfterTransientDrop proves a read whose first
// attempt hit a transient transport drop is retried once and succeeds.
func TestBridgeRetriesIdempotentReadAfterTransientDrop(t *testing.T) {
	b := New("", 3*time.Second, "")
	e := newConcExtension()
	e.failFirst = map[string]bool{"list_tabs": true}
	connect, cleanup := startConcExtension(t, b, e)
	connect()
	defer cleanup()

	if _, err := b.call(context.Background(), "list_tabs", nil); err != nil {
		t.Fatalf("idempotent read should have retried past the transient drop: %v", err)
	}
	if got := b.retries.Load(); got != 1 {
		t.Fatalf("retries = %d, want 1", got)
	}
}

// TestBridgeDoesNotRetryMutatingOp proves a mutating op is NOT auto-retried after
// a transient drop — re-issuing it could double-apply the action.
func TestBridgeDoesNotRetryMutatingOp(t *testing.T) {
	b := New("", 3*time.Second, "")
	e := newConcExtension()
	e.failFirst = map[string]bool{"open_tab": true}
	connect, cleanup := startConcExtension(t, b, e)
	connect()
	defer cleanup()

	if _, err := b.call(context.Background(), "open_tab", map[string]any{"url": "https://example.test"}); err == nil {
		t.Fatal("mutating op must surface the transient error, not silently retry")
	}
	if got := b.retries.Load(); got != 0 {
		t.Fatalf("retries = %d, want 0 (mutating ops never auto-retry)", got)
	}
}

// TestBridgeStatusReportsBackpressureMetrics proves the contention signal is
// surfaced over /status for operators.
func TestBridgeStatusReportsBackpressureMetrics(t *testing.T) {
	b := New("", time.Second, "fake")
	b.SetMaxInflight(4)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	b.handleStatus(rec, req)

	var status map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("parse status: %v", err)
	}
	if got, ok := status["max_inflight"].(float64); !ok || got != 4 {
		t.Fatalf("max_inflight = %v, want 4", status["max_inflight"])
	}
	for _, k := range []string{"inflight", "queued", "busy_drops", "retries"} {
		if _, ok := status[k]; !ok {
			t.Fatalf("status missing backpressure metric %q", k)
		}
	}
}

// TestBridgeUnboundedWhenCapDisabled proves SetMaxInflight(0) removes the cap:
// all requests run concurrently with no semaphore gating.
func TestBridgeUnboundedWhenCapDisabled(t *testing.T) {
	b := New("", 5*time.Second, "")
	b.SetMaxInflight(0)
	e := newConcExtension()
	e.gate = make(chan struct{})
	connect, cleanup := startConcExtension(t, b, e)
	connect()
	defer cleanup()

	const n = 5
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = b.call(context.Background(), "work", nil)
		}()
	}
	// With no cap, all n reach the extension at once.
	waitUntil(t, func() bool { cur, _, _ := e.snapshot(); return cur == n })
	drain(t, e, n)
	wg.Wait()
}
