package extensionbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/coder/websocket"
)

// call is the single chokepoint every bridge RPC funnels through. It layers
// three protections over the shared single-socket transport so many concurrent
// agents cannot wedge it:
//
//  1. per-tab serialization — ops on the same tab never interleave their frames;
//  2. backpressure — a bounded number of RPCs are on the wire at once, excess
//     queues, and a call that cannot get a slot before the deadline fails fast
//     with ErrBridgeBusy instead of hanging;
//  3. reconnect resilience — a call arriving during the MV3 reconnect gap waits
//     for the socket to return, and idempotent reads retry once on a transient
//     transport drop.
//
// Acquisition order is per-tab lock THEN in-flight slot: a same-tab call queues
// on the (cheap) tab lock before it consumes a scarce slot, so a burst on one
// tab cannot starve other tabs of slots, and the consistent ordering keeps the
// two gates deadlock-free.
func (b *Bridge) call(ctx context.Context, typ string, params map[string]any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	unlockTab, err := b.lockTab(timeoutCtx, params)
	if err != nil {
		return nil, err
	}
	if unlockTab != nil {
		defer unlockTab()
	}

	releaseSlot, err := b.acquireSlot(timeoutCtx)
	if err != nil {
		return nil, err
	}
	defer releaseSlot()

	raw, err := b.dispatch(timeoutCtx, typ, params)
	if err == nil {
		return raw, nil
	}
	// Retry once for an idempotent read whose only failure was a transient
	// transport drop: the socket fell over mid-flight but the MV3 worker
	// reconnects in ~1s, and re-issuing a side-effect-free read is safe. dispatch
	// itself waits for the reconnect inside getConn. Mutating ops never retry.
	if isIdempotentType(typ) && isTransientTransportErr(err) {
		b.retries.Add(1)
		if raw2, err2 := b.dispatch(timeoutCtx, typ, params); err2 == nil {
			return raw2, nil
		}
	}
	return raw, err
}

// dispatch performs one RPC round-trip: resolve a live connection (waiting out a
// reconnect gap), register the pending response, write the frame, and wait for
// the reply within ctx. A write failure or a disconnect-drained reply is wrapped
// as errBridgeTransport so call() can decide whether to retry.
func (b *Bridge) dispatch(ctx context.Context, typ string, params map[string]any) (json.RawMessage, error) {
	dispatched := time.Now()
	conn, err := b.getConn(ctx)
	if err != nil {
		return nil, err
	}

	id := strconv.FormatUint(b.nextID.Add(1), 10)
	ch := make(chan response, 1)
	b.mu.Lock()
	// getConn and registration are intentionally two phases because JSON
	// allocation happens afterward. Revalidate the generation and shutdown gate
	// here so Shutdown cannot drain the map and then have this call add itself
	// back, and so a replacement socket never inherits a request written to the
	// displaced connection.
	if b.shuttingDown {
		b.mu.Unlock()
		return nil, errBridgeShuttingDown
	}
	if b.conn != conn {
		b.mu.Unlock()
		return nil, fmt.Errorf("%w: extension connection changed before request dispatch", errBridgeTransport)
	}
	// A server-selected isolation pin is valid only while the CURRENT extension
	// generation still claims it. The request may have resolved tab 42 just before
	// a disconnect, then parked in getConn while tab_removed was lost and Chrome
	// reused 42. Revalidate after getConn and under the same lock that proves the
	// socket generation, so the replacement hello can revoke the stale context
	// before any frame is registered or written. Caller-supplied explicit tab_id
	// remains authoritative by design.
	if !b.followFocus && browser.TabIDRequiresCurrentOwnership(ctx) {
		if implicitTabID := browser.TabIDFromContext(ctx); implicitTabID != "" &&
			(!b.agentPinKnown || strings.TrimSpace(b.active) != implicitTabID) {
			b.mu.Unlock()
			return nil, fmt.Errorf("extension bridge isolation ownership changed before request dispatch: implicit tab %s is no longer extension-owned", implicitTabID)
		}
	}
	b.pending[id] = ch
	b.mu.Unlock()
	// Whichever terminal path wins (response, protocol failure, cancellation,
	// write failure, or disconnect), release any partial body for this request.
	defer b.clearResponseChunks(id)

	msg, err := json.Marshal(request{ID: id, Type: typ, Params: params})
	if err != nil {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, err
	}
	// Write under writeMu with an INDEPENDENT short deadline (not ctx): request
	// cancellation must never tear down the shared socket mid-write. The response
	// is still bounded by ctx in the select below.
	writeCtx, writeCancel := context.WithTimeout(context.Background(), bridgeWriteTimeout)
	b.writeMu.Lock()
	err = conn.Write(writeCtx, websocket.MessageText, msg)
	b.writeMu.Unlock()
	writeCancel()
	if err != nil {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		// A failed write means the socket is broken (closed / broken pipe); mark
		// it transient so an idempotent caller can retry after the worker returns.
		return nil, fmt.Errorf("%w: %v", errBridgeTransport, err)
	}

	select {
	case resp := <-ch:
		if !resp.OK {
			if resp.Error == "" {
				resp.Error = "extension bridge request failed"
			}
			if resp.Error == disconnectDrainReason {
				// releaseConn drained this pending call because the socket dropped:
				// transient, retryable for idempotent ops.
				return nil, fmt.Errorf("%w: %s", errBridgeTransport, resp.Error)
			}
			return nil, fmt.Errorf("extension bridge: %s", resp.Error)
		}
		return resp.Result, nil
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("extension bridge: no reply to %s within %s; the tab's renderer or the extension worker is busy or hung: %w", describeBridgeRequest(typ, params), time.Since(dispatched).Round(time.Millisecond), ctx.Err())
		}
		return nil, ctx.Err()
	}
}

// describeBridgeRequest names a bridge request for an error: the cdp
// passthrough by its method, everything else by its message type.
func describeBridgeRequest(typ string, params map[string]any) string {
	if typ == "cdp" {
		if method, _ := params["method"].(string); method != "" {
			return "cdp " + method
		}
	}
	return typ
}

// getConn returns the live socket, parking briefly for the MV3 service worker to
// reconnect if it is momentarily down rather than failing the call outright.
// MV3 workers sleep and respawn constantly, so a transient conn==nil is normal,
// not a real disconnect.
func (b *Bridge) getConn(ctx context.Context) (*websocket.Conn, error) {
	b.mu.RLock()
	conn := b.conn
	shuttingDown := b.shuttingDown
	b.mu.RUnlock()
	if shuttingDown {
		return nil, errBridgeShuttingDown
	}
	if conn != nil {
		return conn, nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, bridgeReconnectGrace)
	defer cancel()
	for {
		b.mu.RLock()
		conn := b.conn
		ready := b.connReady
		shuttingDown := b.shuttingDown
		b.mu.RUnlock()
		if shuttingDown {
			return nil, errBridgeShuttingDown
		}
		if conn != nil {
			return conn, nil
		}
		select {
		case <-ready:
			// A connection went live; re-read b.conn on the next loop iteration.
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errBridgeNotConnected
		}
	}
}

// acquireSlot takes one in-flight slot from the backpressure semaphore, blocking
// (ctx-aware) when the cap is saturated. If the deadline elapses before a slot
// frees, it returns ErrBridgeBusy so the caller backs off instead of piling onto
// a flooded socket. The returned release MUST be called. A nil sema disables the
// cap and returns a no-op release immediately.
func (b *Bridge) acquireSlot(ctx context.Context) (func(), error) {
	sema := b.sema
	if sema == nil {
		return func() {}, nil
	}
	release := func() { <-sema; b.inflight.Add(-1) }
	select {
	case sema <- struct{}{}:
		b.inflight.Add(1)
		return release, nil
	default:
	}
	// Saturated: queue for a slot, bounded by the op deadline.
	b.queued.Add(1)
	defer b.queued.Add(-1)
	select {
	case sema <- struct{}{}:
		b.inflight.Add(1)
		return release, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			b.busyDrops.Add(1)
			return nil, ErrBridgeBusy
		}
		return nil, ctx.Err()
	}
}

// ctxKeySkipTabLock marks a context whose RPC must NOT take the per-tab lock.
type ctxKeySkipTabLock struct{}

// withoutTabLock marks ctx so call() skips per-tab serialization for this RPC.
// It is for the abandonable settle fingerprint probe only: a read-only,
// fire-and-forget background read that the settle watchdog may give up on while
// it keeps running (cancelling it mid-write would drop the shared socket). If
// such an orphan held the tab lock it would block the next foreground op on that
// tab; exempting it keeps settle's "abandon and move on" semantics. The in-flight
// cap still bounds it.
func withoutTabLock(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeySkipTabLock{}, true)
}

func tabLockSkipped(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeySkipTabLock{}).(bool)
	return v
}

// lockTab acquires the per-tab serialization lock for the tab named in params
// (if any), so two RPCs targeting the same tab never overlap. Returns a nil
// unlock when the op is not tab-scoped (e.g. list_tabs, open_tab) or is marked
// skip via withoutTabLock. Ctx-aware: a call that cannot get the tab's turn
// before the deadline returns ErrBridgeBusy.
func (b *Bridge) lockTab(ctx context.Context, params map[string]any) (func(), error) {
	if tabLockSkipped(ctx) {
		return nil, nil
	}
	key := tabKeyFromParams(params)
	if key == "" {
		return nil, nil
	}
	entry := b.retainTabLock(key)
	select {
	case entry.ch <- struct{}{}:
		return func() {
			<-entry.ch
			b.releaseTabLock(key, entry)
		}, nil
	case <-ctx.Done():
		b.releaseTabLock(key, entry)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrBridgeBusy
		}
		return nil, ctx.Err()
	}
}

// retainTabLock returns the stable lock entry for key and takes one reference
// before the caller begins waiting. Counting waiters as well as the holder is
// what makes deletion safe across a tab close followed by numeric-id reuse.
func (b *Bridge) retainTabLock(key string) *tabLockEntry {
	b.tabLocksMu.Lock()
	defer b.tabLocksMu.Unlock()
	entry := b.tabLocks[key]
	if entry == nil {
		entry = &tabLockEntry{ch: make(chan struct{}, 1)}
		b.tabLocks[key] = entry
	}
	entry.refs++
	return entry
}

// releaseTabLock drops one holder/waiter reference. Delete only when the map
// still names this exact entry and no caller can retain or acquire its channel;
// otherwise numeric-id reuse could create two live locks for the same tab.
func (b *Bridge) releaseTabLock(key string, entry *tabLockEntry) {
	b.tabLocksMu.Lock()
	defer b.tabLocksMu.Unlock()
	if entry.refs > 0 {
		entry.refs--
	}
	if entry.refs == 0 && b.tabLocks[key] == entry {
		delete(b.tabLocks, key)
	}
}

// tabKeyFromParams extracts a stable string lock key from a call's tabId param,
// which is an int on the cdp/file-chooser paths and a string on focus/close.
// A zero/absent id means "not tab-scoped" (no lock).
func tabKeyFromParams(params map[string]any) string {
	if params == nil {
		return ""
	}
	v, ok := params["tabId"]
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case int:
		if t == 0 {
			return ""
		}
		return strconv.Itoa(t)
	case int64:
		if t == 0 {
			return ""
		}
		return strconv.FormatInt(t, 10)
	case float64:
		if t == 0 {
			return ""
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}
