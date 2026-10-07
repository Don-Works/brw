package extensionbridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type concExtension struct {
	mu        sync.Mutex
	cur       int
	maxCur    int
	handled   int
	perTabCur map[string]int
	perTabMax map[string]int

	writeMu sync.Mutex

	gate chan struct{}

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

	exited := false
	exitOnce := func() {
		if !exited {
			exited = true
			e.exit(tab)
		}
	}
	defer exitOnce()

	e.mu.Lock()
	failNow := e.failFirst[typ] && !e.failedOnce[typ]
	if failNow {
		e.failedOnce[typ] = true
	}
	e.mu.Unlock()
	if failNow {

		exitOnce()
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
	exitOnce()
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

func TestBridgeBusyWhenSaturated(t *testing.T) {

	b := New("", 5*time.Second, "")
	b.SetMaxInflight(1)
	e := newConcExtension()
	e.gate = make(chan struct{})
	connect, cleanup := startConcExtension(t, b, e)
	connect()
	defer cleanup()

	go func() { _, _ = b.call(context.Background(), "hold", nil) }()
	waitUntil(t, func() bool { cur, _, _ := e.snapshot(); return cur == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := b.call(ctx, "work", nil)
	if !errors.Is(err, ErrBridgeBusy) {
		t.Fatalf("saturated call err = %v, want ErrBridgeBusy", err)
	}
	if b.busyDrops.Load() == 0 {
		t.Fatal("busy_drops should have incremented")
	}
	drain(t, e, 1)
}

func TestBridgeSerializesSameTab(t *testing.T) {
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
		go func() {
			defer wg.Done()
			_, _ = b.call(context.Background(), "work", map[string]any{"tabId": 7})
		}()
	}

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

	waitUntil(t, func() bool { cur, _, _ := e.snapshot(); return cur == n })
	drain(t, e, n)
	wg.Wait()
}

func TestBridgeWithoutTabLockBypassesSerialization(t *testing.T) {
	b := New("", 5*time.Second, "")
	b.SetMaxInflight(8)
	e := newConcExtension()
	e.gate = make(chan struct{})
	connect, cleanup := startConcExtension(t, b, e)
	connect()
	defer cleanup()

	go func() { _, _ = b.call(context.Background(), "work", map[string]any{"tabId": 5}) }()
	waitUntil(t, func() bool { cur, _, _ := e.snapshot(); return cur == 1 })

	go func() {
		_, _ = b.call(withoutTabLock(context.Background()), "work", map[string]any{"tabId": 5})
	}()
	waitUntil(t, func() bool { cur, _, _ := e.snapshot(); return cur == 2 })

	drain(t, e, 2)
}

func TestTabLockLifecycleSerializesRemovalAndNumericIDReuse(t *testing.T) {
	b := New("", 5*time.Second, "")
	params := map[string]any{"tabId": 42}
	holderUnlock, err := b.lockTab(context.Background(), params)
	if err != nil {
		t.Fatalf("acquire holder: %v", err)
	}

	const oldWaiters = 24
	start := make(chan struct{})
	releaseAcquired := make(chan struct{})
	acquired := make(chan struct{}, oldWaiters+1)
	errs := make(chan error, oldWaiters+1)
	var wg sync.WaitGroup
	var currentMu sync.Mutex
	current, peak := 0, 0
	spawnWaiter := func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			unlock, lockErr := b.lockTab(context.Background(), params)
			if lockErr != nil {
				errs <- lockErr
				return
			}
			currentMu.Lock()
			current++
			if current > peak {
				peak = current
			}
			currentMu.Unlock()
			acquired <- struct{}{}
			<-releaseAcquired
			currentMu.Lock()
			current--
			currentMu.Unlock()
			unlock()
		}()
	}
	for i := 0; i < oldWaiters; i++ {
		spawnWaiter()
	}
	close(start)
	waitUntil(t, func() bool {
		b.tabLocksMu.Lock()
		defer b.tabLocksMu.Unlock()
		entry := b.tabLocks["42"]
		return entry != nil && entry.refs == oldWaiters+1
	})

	live := &websocket.Conn{}
	b.mu.Lock()
	b.conn = live
	b.active = "42"
	b.mu.Unlock()
	b.handleDecodedFrame(live, response{Type: "tab_removed", TabID: 42})
	reuseStart := make(chan struct{})
	start = reuseStart
	spawnWaiter()
	close(reuseStart)
	waitUntil(t, func() bool {
		b.tabLocksMu.Lock()
		defer b.tabLocksMu.Unlock()
		entry := b.tabLocks["42"]
		return entry != nil && entry.refs == oldWaiters+2
	})
	select {
	case <-acquired:
		t.Fatal("a waiter acquired while the removed tab's holder still owned the lock")
	default:
	}

	holderUnlock()
	for i := 0; i < oldWaiters+1; i++ {
		select {
		case <-acquired:
			releaseAcquired <- struct{}{}
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d never acquired", i)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("waiter failed: %v", err)
	}
	currentMu.Lock()
	gotPeak, gotCurrent := peak, current
	currentMu.Unlock()
	if gotPeak != 1 || gotCurrent != 0 {
		t.Fatalf("same numeric id was not serialized across removal/reuse: peak=%d current=%d", gotPeak, gotCurrent)
	}
	b.tabLocksMu.Lock()
	remaining := len(b.tabLocks)
	b.tabLocksMu.Unlock()
	if remaining != 0 {
		t.Fatalf("tab-lock table retained %d entries after all holders/waiters exited", remaining)
	}
}

func TestTabLockCancelledWaitersReleaseReferences(t *testing.T) {
	b := New("", time.Second, "")
	params := map[string]any{"tabId": 77}
	holderUnlock, err := b.lockTab(context.Background(), params)
	if err != nil {
		t.Fatalf("acquire holder: %v", err)
	}

	const waiters = 32
	var wg sync.WaitGroup
	errs := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			if _, lockErr := b.lockTab(ctx, params); !errors.Is(lockErr, ErrBridgeBusy) {
				errs <- lockErr
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("cancelled waiter error = %v, want ErrBridgeBusy", err)
	}
	b.tabLocksMu.Lock()
	entry := b.tabLocks["77"]
	refs := 0
	if entry != nil {
		refs = entry.refs
	}
	b.tabLocksMu.Unlock()
	if refs != 1 {
		t.Fatalf("cancelled waiters leaked references: refs=%d want holder-only 1", refs)
	}
	holderUnlock()
	b.tabLocksMu.Lock()
	remaining := len(b.tabLocks)
	b.tabLocksMu.Unlock()
	if remaining != 0 {
		t.Fatalf("tab-lock table retained %d entries after holder release", remaining)
	}
}

func TestTabLockTableReturnsToBaselineAfterHighCardinalityChurn(t *testing.T) {
	b := New("", time.Second, "")
	const workers = 8
	const perWorker = 500
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				key := strconv.Itoa(worker*perWorker + i + 1)
				unlock, err := b.lockTab(context.Background(), map[string]any{"tabId": key})
				if err != nil {
					errs <- err
					return
				}
				unlock()
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("churn lock failed: %v", err)
	}
	b.tabLocksMu.Lock()
	remaining := len(b.tabLocks)
	b.tabLocksMu.Unlock()
	if remaining != 0 {
		t.Fatalf("tab-lock table retained %d entries after %d completed tab ids", remaining, workers*perWorker)
	}
}

func TestBridgeWaitsForReconnectInsteadOfFailing(t *testing.T) {
	b := New("", 3*time.Second, "")
	e := newConcExtension()
	connect, cleanup := startConcExtension(t, b, e)
	defer cleanup()

	done := make(chan error, 1)
	go func() {
		_, err := b.call(context.Background(), "list_tabs", nil)
		done <- err
	}()

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

	waitUntil(t, func() bool { cur, _, _ := e.snapshot(); return cur == n })
	drain(t, e, n)
	wg.Wait()
}
