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

// call is the chokepoint for every bridge RPC: per-tab serialization,
// backpressure (ErrBridgeBusy past the deadline), and reconnect resilience
// (wait out the MV3 gap; idempotent reads retry once). Order is tab lock THEN
// in-flight slot, so a burst on one tab cannot starve other tabs of slots.
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
	// Retry an idempotent read once after a transient drop; getConn waits out the
	// reconnect. Mutating ops never retry.
	if isIdempotentType(typ) && isTransientTransportErr(err) {
		b.retries.Add(1)
		if raw2, err2 := b.dispatch(timeoutCtx, typ, params); err2 == nil {
			return raw2, nil
		}
	}
	return raw, err
}

// dispatch performs one RPC round-trip. Write failures and disconnect-drained
// replies are wrapped as errBridgeTransport so call can retry.
func (b *Bridge) dispatch(ctx context.Context, typ string, params map[string]any) (json.RawMessage, error) {
	dispatched := time.Now()
	conn, err := b.getConn(ctx)
	if err != nil {
		return nil, err
	}

	id := strconv.FormatUint(b.nextID.Add(1), 10)
	ch := make(chan response, 1)
	b.mu.Lock()
	// Revalidate generation and shutdown after getConn: Shutdown must not drain the
	// map before this call adds itself back, and a replacement socket must not
	// inherit a request written to the displaced one.
	if b.shuttingDown {
		b.mu.Unlock()
		return nil, errBridgeShuttingDown
	}
	if b.conn != conn {
		b.mu.Unlock()
		return nil, fmt.Errorf("%w: extension connection changed before request dispatch", errBridgeTransport)
	}
	// An implicit isolation pin is valid only while the current generation still
	// claims it: tab 42 may have closed and been reused during a reconnect.
	// Checked under the lock that proves the generation. An explicit tab_id wins.
	if !b.followFocus && browser.TabIDRequiresCurrentOwnership(ctx) {
		if implicitTabID := browser.TabIDFromContext(ctx); implicitTabID != "" &&
			(!b.agentPinKnown || strings.TrimSpace(b.active) != implicitTabID) {
			b.mu.Unlock()
			return nil, fmt.Errorf("extension bridge isolation ownership changed before request dispatch: implicit tab %s is no longer extension-owned", implicitTabID)
		}
	}
	b.pending[id] = ch
	b.mu.Unlock()
	defer b.clearResponseChunks(id)

	msg, err := json.Marshal(request{ID: id, Type: typ, Params: params})
	if err != nil {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, err
	}
	// Independent write deadline, not ctx: cancellation mid-write tears down the
	// shared socket.
	writeCtx, writeCancel := context.WithTimeout(context.Background(), bridgeWriteTimeout)
	b.writeMu.Lock()
	err = conn.Write(writeCtx, websocket.MessageText, msg)
	b.writeMu.Unlock()
	writeCancel()
	if err != nil {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, fmt.Errorf("%w: %v", errBridgeTransport, err)
	}

	select {
	case resp := <-ch:
		if !resp.OK {
			if resp.Error == "" {
				resp.Error = "extension bridge request failed"
			}
			if resp.Error == disconnectDrainReason {
				return nil, fmt.Errorf("%w: %s", errBridgeTransport, resp.Error)
			}
			if detail, ok := strings.CutPrefix(resp.Error, browser.ForeignExtensionFramePrefix); ok {
				return nil, fmt.Errorf("extension bridge: %w:%s", browser.ErrForeignExtensionFrame, detail)
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

func describeBridgeRequest(typ string, params map[string]any) string {
	if typ == "cdp" {
		if method, _ := params["method"].(string); method != "" {
			return "cdp " + method
		}
	}
	return typ
}

// getConn returns the live socket, parking up to bridgeReconnectGrace while
// the MV3 worker respawns; a brief conn==nil is normal.
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
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errBridgeNotConnected
		}
	}
}

// acquireSlot takes an in-flight slot, returning ErrBridgeBusy if none frees by
// the deadline. The returned release MUST be called. A nil sema means no cap.
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

type ctxKeySkipTabLock struct{}

// withoutTabLock exempts the abandonable settle probe from the tab lock: an
// orphaned probe holding it would block the next action on that tab. The
// in-flight cap still applies.
func withoutTabLock(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeySkipTabLock{}, true)
}

func tabLockSkipped(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeySkipTabLock{}).(bool)
	return v
}

// lockTab serializes RPCs on the tab in params. Nil unlock when the op is not
// tab-scoped or uses withoutTabLock; ErrBridgeBusy past the deadline.
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

// retainTabLock counts waiters as well as the holder, which keeps deletion safe
// across a tab close followed by numeric-id reuse.
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

// releaseTabLock deletes only when the map still names this entry, or id reuse
// could create two live locks for one tab.
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

// tabKeyFromParams normalizes tabId (int on cdp paths, string on focus/close).
// Zero or absent means not tab-scoped.
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
