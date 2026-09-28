package extensionbridge

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/usagelog"
	"github.com/coder/websocket"
)

func (b *Bridge) handleExtension(w http.ResponseWriter, r *http.Request) {
	// coder/websocket treats an ABSENT Origin as same-origin, so a local non-browser
	// client would pass; a web page cannot forge a chrome-extension:// Origin.
	if !extensionOriginOK(r.Header.Get("Origin")) {
		http.Error(w, "forbidden: a chrome-extension origin is required", http.StatusForbidden)
		return
	}
	allowedID := b.effectiveExtensionID()
	originPatterns := []string{"chrome-extension://*"}
	if allowedID != "" {
		originPatterns = []string{"chrome-extension://" + allowedID}
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: originPatterns,
	})
	if err != nil {
		if ok, skipped := b.acceptLog.admit(r.Header.Get("Origin"), time.Now()); ok {
			if skipped > 0 {
				log.Printf("extension websocket accept: %v (%d more from this origin since the last line)", err, skipped)
			} else {
				log.Printf("extension websocket accept: %v", err)
			}
		}
		return
	}
	conn.SetReadLimit(extensionFrameReadLimitBytes)

	if allowedID == "" {
		log.Printf("WARNING: extension bridge accepting connections from any Chrome extension (chrome-extension://*); set a profile policy with bridge_extension_id to restrict")
	}

	// Authenticate before this connection goes live, so an unverified client can
	// neither displace the real extension nor receive a command.
	verifiedHello := hello{}
	if b.authToken != "" {
		h, herr := b.verifyHandshake(r.Context(), conn)
		if herr != nil {
			log.Printf("extension bridge handshake rejected: %v", herr)
			b.recordHandshakeRejection(h, herr)
			_ = conn.Close(websocket.StatusPolicyViolation, "handshake failed")
			return
		}
		verifiedHello = h
	}

	b.mu.Lock()
	now := time.Now().UTC()
	if b.shuttingDown {
		b.mu.Unlock()
		_ = conn.CloseNow()
		return
	}
	// Flap guard: two profiles on one bridge displace each other forever. While
	// holding with a live connection, reject the newcomer and extend the hold.
	if b.conn != nil && now.Before(b.flapHoldUntil) {
		b.flapHoldUntil = now.Add(flapHoldDuration)
		b.mu.Unlock()
		_ = conn.Close(websocket.StatusTryAgainLater, "another extension already holds this bridge (flap-hold active)")
		return
	}
	if b.conn != nil {
		cutoff := now.Add(-flapWindow)
		kept := b.recentReplaces[:0]
		for _, t := range b.recentReplaces {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		b.recentReplaces = append(kept, now)
		if len(b.recentReplaces) >= flapThreshold {
			b.flapHoldUntil = now.Add(flapHoldDuration)
			b.recentReplaces = b.recentReplaces[:0]
			b.mu.Unlock()
			log.Printf("brw: extension connection flap detected (%d+ replacements in %s) on bridge %s — two browser profiles likely have brw enabled on the same bridge. Holding the current connection and rejecting extras; disable brw in the other profile, or give it its own --bridge-addr.", flapThreshold, flapWindow, b.addr)
			_ = conn.Close(websocket.StatusTryAgainLater, "extension flap: holding current connection")
			return
		}
		// CloseNow, not Close: a graceful close waits up to 5s for the peer's ack while
		// b.mu is held, freezing every dispatch and /status probe.
		_ = b.conn.CloseNow()
		// A logical response must not span connection generations: a new response
		// could reuse the request id of a displaced partial one.
		b.clearAllResponseChunksLocked()
		// Pending RPCs survive a same-extension replace (a reconnecting MV3 worker
		// can still answer them); a different identity never will, so fail them now.
		if !sameExtensionIdentity(b.hello, verifiedHello) {
			b.drainPendingLocked(replacedDrainReason)
			b.resetProfileStateLocked()
		}
	}
	b.conn = conn
	b.hello = verifiedHello
	// A new socket can be a restarted service worker that has lost every per-tab arm.
	b.containment.reset()
	b.webmcpArm.reset()
	// Reconcile the pin before publishing via connReady: a tabs.onRemoved frame
	// can be lost while the worker is down, so the hello is authoritative.
	b.reconcileAgentPinLocked(verifiedHello)
	b.connectedAt = now
	b.lastSeenAt = now
	b.disconnectedAt = time.Time{}
	b.disconnectReason = ""
	// Close-then-replace under mu so the channel is closed exactly once.
	close(b.connReady)
	b.connReady = make(chan struct{})
	b.mu.Unlock()

	if verifiedHello.Build != "" || verifiedHello.Label != "" {
		log.Printf("extension bridge connected (build %s, label %q)", verifiedHello.Build, verifiedHello.Label)
	} else {
		log.Printf("extension bridge connected")
	}
	b.recordBridgeUsage("bridge_connect", "ok", "", "", verifiedHello.Build)

	// Ping so a half-open link (sleep, NAT timeout) is detected before a request times out.
	pingCtx, pingCancel := context.WithCancel(r.Context())
	pingFailure := make(chan struct{}, 1)
	go b.keepAliveWithFailure(pingCtx, conn, pingKeepaliveInterval, pingFailure)

	readErr := b.readLoop(r.Context(), conn)
	pingCancel()
	reason, expected := bridgeDisconnectReason(readErr)
	select {
	case <-pingFailure:
		// The close frame that unblocked readLoop looks routine; the ping failure is not.
		reason, expected = "keepalive ping failed", false
	default:
	}
	// A replaced connection's read loop ends with "use of closed network
	// connection"; that is routine MV3 lifecycle, not a disconnect.
	if !b.releaseConn(conn, reason) {
		return
	}
	if expected {
		log.Printf("extension bridge disconnected cleanly: %s", reason)
		b.recordBridgeUsage("bridge_disconnect", "ok", "", "", verifiedHello.Build)
		return
	}
	log.Printf("extension bridge disconnected unexpectedly: %s", reason)
	b.recordBridgeUsage("bridge_disconnect", "error", "transport", reason, verifiedHello.Build)
}

// bridgeDisconnectReason separates routine WebSocket lifecycle from transport
// failure. Close reasons are canonical so peer close-frame text stays out of logs.
func bridgeDisconnectReason(err error) (reason string, expected bool) {
	if err == nil {
		return "connection closed", true
	}
	if errors.Is(err, context.Canceled) {
		return "context canceled", true
	}
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure:
		return "normal closure", true
	case websocket.StatusGoingAway:
		return "peer going away", true
	}
	if status := websocket.CloseStatus(err); status != -1 {
		return fmt.Sprintf("websocket close status %d", status), false
	}
	return err.Error(), false
}

// releaseConn acts only when conn is still the ACTIVE connection: a displaced
// conn's read loop must not drain the live connection's pending RPCs or mark
// the bridge disconnected.
func (b *Bridge) releaseConn(conn *websocket.Conn, reason string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != conn {
		return false
	}
	b.conn = nil
	b.agentPinKnown = false
	b.disconnectedAt = time.Now().UTC()
	b.disconnectReason = reason
	b.drainPendingLocked(disconnectDrainReason)
	return true
}

func (b *Bridge) recordBridgeUsage(operation, outcome, errorClass, errorMessage, extensionBuild string) {
	if b.usage == nil {
		return
	}
	fingerprint := ""
	if errorMessage != "" {
		fingerprint = usagelog.Fingerprint(errorMessage)
	}
	_ = b.usage.Record(usagelog.Event{
		Layer: "bridge", Operation: operation, Outcome: outcome,
		ErrorClass: errorClass, ErrorFingerprint: fingerprint,
		Retryable: usagelog.Retryable(errorClass), ExtensionBuild: extensionBuild,
	})
}

// recordTabGroupDegradation records a tab that opened but whose group
// assignment Chromium refused. Only the capability class reaches the ledger,
// never the title, URL or raw warning.
func (b *Bridge) recordTabGroupDegradation(warning string) {
	if strings.TrimSpace(warning) == "" {
		return
	}
	b.mu.RLock()
	build := b.hello.Build
	b.mu.RUnlock()
	b.recordBridgeUsage("tab_group_assignment", "degraded", "capability", warning, build)
}

// drainPendingLocked fails every in-flight RPC. Caller holds b.mu.
func (b *Bridge) drainPendingLocked(reason string) {
	b.clearAllResponseChunksLocked()
	for id, ch := range b.pending {
		delete(b.pending, id)
		ch <- response{ID: id, Error: reason}
		close(ch)
	}
}

// clearResponseChunksLocked releases one partial response. Caller holds b.mu.
func (b *Bridge) clearResponseChunksLocked(id string) {
	assembly := b.responseChunks[id]
	if assembly == nil {
		return
	}
	b.responseChunkBytes -= assembly.received
	if b.responseChunkBytes < 0 {
		b.responseChunkBytes = 0
	}
	delete(b.responseChunks, id)
}

// clearAllResponseChunksLocked releases every partial response. Caller holds b.mu.
func (b *Bridge) clearAllResponseChunksLocked() {
	clear(b.responseChunks)
	b.responseChunkBytes = 0
}

func (b *Bridge) clearResponseChunks(id string) {
	b.mu.Lock()
	b.clearResponseChunksLocked(id)
	b.mu.Unlock()
}

func invalidChunkResponse(id string, err error) response {
	return response{
		ID:    id,
		Error: "invalid chunked extension response: " + err.Error(),
	}
}

// decodeResponseChunk validates one frame's own fields. Cross-frame checks run
// under b.mu in consumeResponseChunk.
func decodeResponseChunk(frame response) (index, count, total int, data []byte, err error) {
	if frame.Encoding != "base64" {
		return 0, 0, 0, nil, fmt.Errorf("unsupported encoding %q", frame.Encoding)
	}
	if frame.ChunkIndex == nil || frame.ChunkCount == nil || frame.TotalBytes == nil {
		return 0, 0, 0, nil, errors.New("missing chunk metadata")
	}
	index, count, total = *frame.ChunkIndex, *frame.ChunkCount, *frame.TotalBytes
	if count < 1 || count > maxChunkedResponseChunks {
		return 0, 0, 0, nil, fmt.Errorf("chunk_count %d is outside 1..%d", count, maxChunkedResponseChunks)
	}
	if index < 0 || index >= count {
		return 0, 0, 0, nil, fmt.Errorf("chunk_index %d is outside 0..%d", index, count-1)
	}
	if total < 1 || total > maxChunkedResponseBytes {
		return 0, 0, 0, nil, fmt.Errorf("total_bytes %d is outside 1..%d", total, maxChunkedResponseBytes)
	}
	if frame.ChunkData == "" {
		return 0, 0, 0, nil, errors.New("empty chunk data")
	}
	// Padded base64 overstates decoded bytes by at most two; reject before allocating.
	if decodedUpperBound := base64.StdEncoding.DecodedLen(len(frame.ChunkData)); decodedUpperBound > total+2 {
		return 0, 0, 0, nil, fmt.Errorf("encoded chunk cannot fit within total_bytes %d", total)
	}
	data, err = base64.StdEncoding.DecodeString(frame.ChunkData)
	if err != nil {
		return 0, 0, 0, nil, fmt.Errorf("invalid base64 data: %w", err)
	}
	if len(data) == 0 || len(data) > total {
		return 0, 0, 0, nil, fmt.Errorf("decoded chunk length %d is invalid for total_bytes %d", len(data), total)
	}
	return index, count, total, data, nil
}

// rejectResponseChunk fails the awaiting RPC on a protocol fault. A stale socket
// may not clear its replacement's state, and a cancelled request gets no reply.
func (b *Bridge) rejectResponseChunk(conn *websocket.Conn, id string, err error) (response, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != conn {
		return response{}, false
	}
	b.clearResponseChunksLocked(id)
	if _, pending := b.pending[id]; !pending {
		return response{}, false
	}
	return invalidChunkResponse(id, err), true
}

// consumeResponseChunk accepts only a contiguous, consistent sequence for an
// outstanding request and returns ready=true exactly once.
func (b *Bridge) consumeResponseChunk(conn *websocket.Conn, frame response) (logical response, ready bool) {
	// Skip decoding for a cancelled request; rechecked after decoding because
	// dispatch may cancel meanwhile.
	b.mu.Lock()
	if b.conn != conn {
		b.mu.Unlock()
		return response{}, false
	}
	if _, pending := b.pending[frame.ID]; !pending {
		b.clearResponseChunksLocked(frame.ID)
		b.mu.Unlock()
		return response{}, false
	}
	b.mu.Unlock()

	index, count, total, chunk, err := decodeResponseChunk(frame)
	if err != nil {
		return b.rejectResponseChunk(conn, frame.ID, err)
	}

	b.mu.Lock()
	if b.conn != conn {
		b.mu.Unlock()
		return response{}, false
	}
	if _, pending := b.pending[frame.ID]; !pending {
		b.clearResponseChunksLocked(frame.ID)
		b.mu.Unlock()
		return response{}, false
	}

	assembly := b.responseChunks[frame.ID]
	if assembly == nil {
		if index != 0 {
			b.mu.Unlock()
			return b.rejectResponseChunk(conn, frame.ID, fmt.Errorf("first chunk has index %d, want 0", index))
		}
		if b.responseChunks == nil {
			b.responseChunks = make(map[string]*responseChunkAssembly)
		}
		assembly = &responseChunkAssembly{
			conn:       conn,
			chunkCount: count,
			totalBytes: total,
			parts:      make([][]byte, 0, count),
		}
		b.responseChunks[frame.ID] = assembly
	} else {
		if assembly.conn != conn {
			b.mu.Unlock()
			return b.rejectResponseChunk(conn, frame.ID, errors.New("chunk sequence changed connection"))
		}
		if count != assembly.chunkCount || total != assembly.totalBytes {
			b.mu.Unlock()
			return b.rejectResponseChunk(conn, frame.ID, errors.New("chunk metadata changed during response"))
		}
	}
	if index != assembly.nextIndex {
		want := assembly.nextIndex
		b.mu.Unlock()
		return b.rejectResponseChunk(conn, frame.ID, fmt.Errorf("chunk index %d arrived out of order; want %d", index, want))
	}
	if assembly.received > total-len(chunk) {
		b.mu.Unlock()
		return b.rejectResponseChunk(conn, frame.ID, fmt.Errorf("decoded bytes exceed declared total_bytes %d", total))
	}
	if b.responseChunkBytes > maxBufferedChunkedResponsesBytes-len(chunk) {
		b.mu.Unlock()
		return b.rejectResponseChunk(conn, frame.ID, fmt.Errorf("buffered chunks exceed %d-byte bridge limit", maxBufferedChunkedResponsesBytes))
	}

	newReceived := assembly.received + len(chunk)
	isFinal := index == count-1
	if isFinal && newReceived != total {
		b.mu.Unlock()
		return b.rejectResponseChunk(conn, frame.ID, fmt.Errorf("final byte count %d does not match total_bytes %d", newReceived, total))
	}
	if !isFinal && newReceived >= total {
		b.mu.Unlock()
		return b.rejectResponseChunk(conn, frame.ID, fmt.Errorf("response reached total_bytes %d before final chunk", total))
	}

	assembly.parts = append(assembly.parts, chunk)
	assembly.received = newReceived
	assembly.nextIndex++
	b.responseChunkBytes += len(chunk)
	if !isFinal {
		b.mu.Unlock()
		return response{}, false
	}

	parts := assembly.parts
	b.clearResponseChunksLocked(frame.ID)
	b.mu.Unlock()

	serialized := make([]byte, total)
	offset := 0
	for _, part := range parts {
		offset += copy(serialized[offset:], part)
	}
	if offset != total {
		return invalidChunkResponse(frame.ID, fmt.Errorf("reassembled byte count %d does not match total_bytes %d", offset, total)), true
	}
	if err := json.Unmarshal(serialized, &logical); err != nil {
		return invalidChunkResponse(frame.ID, fmt.Errorf("reassembled payload is not valid JSON: %w", err)), true
	}
	if logical.ID != frame.ID {
		return invalidChunkResponse(frame.ID, fmt.Errorf("reassembled response id %q does not match envelope id", logical.ID)), true
	}
	if logical.Type == "response_chunk" {
		return invalidChunkResponse(frame.ID, errors.New("reassembled payload is another chunk envelope")), true
	}
	return logical, true
}

// deliverResponse claims the request atomically so no response is dispatched twice.
func (b *Bridge) deliverResponse(conn *websocket.Conn, resp response) {
	b.mu.Lock()
	// A decoded response may wait on b.mu while its socket is replaced.
	if b.conn != conn {
		b.mu.Unlock()
		return
	}
	ch := b.pending[resp.ID]
	delete(b.pending, resp.ID)
	b.clearResponseChunksLocked(resp.ID)
	b.mu.Unlock()
	if ch != nil {
		ch <- resp
		close(ch)
	}
}

// handleDecodedFrame gates every decoded frame on connection generation.
// Response paths recheck when claiming, since chunk decoding can yield between.
func (b *Bridge) handleDecodedFrame(conn *websocket.Conn, resp response) {
	b.mu.Lock()
	if b.conn != conn {
		b.mu.Unlock()
		return
	}
	b.lastSeenAt = time.Now().UTC()
	if resp.Type == "hello" {
		h := resp.Hello
		h.Token = "" // never store or echo the handshake secret
		b.hello = h
		// Tokenless embedders go live before the hello; treat a later hello as the pin boundary.
		b.reconcileAgentPinLocked(h)
		b.mu.Unlock()
		return
	}
	if resp.Type == "active_tab" {
		// active_tab is the user's focus hint. In isolation b.active must track the
		// tab brw owns, so it is honoured only in follow-focus mode.
		if resp.TabID != 0 && b.followFocus {
			b.active = strconv.Itoa(resp.TabID)
		}
		b.mu.Unlock()
		return
	}
	if resp.Type == "tab_removed" {
		if resp.TabID != 0 {
			b.invalidateTabStateLocked(strconv.Itoa(resp.TabID))
		}
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()

	if resp.Type == "response_chunk" {
		if resp.ID == "" {
			log.Printf("extension bridge invalid response chunk: missing id")
			return
		}
		logical, ready := b.consumeResponseChunk(conn, resp)
		if ready {
			b.deliverResponse(conn, logical)
		}
		return
	}
	if resp.ID == "" {
		return
	}
	b.deliverResponse(conn, resp)
}

// resetProfileStateLocked discards profile-scoped state: another profile can
// reuse tab ids and download GUIDs. Not called on a same-extension reconnect.
//
// Caller holds b.mu. Lock order: b.mu -> observeMu -> downloadsMu ->
// emulationMu; never acquire b.mu while holding a subordinate lock.
func (b *Bridge) resetProfileStateLocked() {
	b.active = ""
	b.agentPinKnown = false
	b.autoOpenFailedAt = time.Time{}

	b.observeMu.Lock()
	b.observedState = map[string]*browser.SemanticState{}
	b.observeVersions = map[string]int64{}
	b.observeMu.Unlock()

	b.downloadsMu.Lock()
	b.downloadFingerprints = map[string]string{}
	b.downloadVersions = map[string]uint64{}
	b.downloadCursors = map[string]uint64{}
	b.downloadSequence = 0
	b.downloadsMu.Unlock()

	b.emulationMu.Lock()
	b.emulationStates = map[string]bridgeDeviceEmulationState{}
	b.emulationMu.Unlock()
}

// reconcileAgentPinLocked makes the new connection's owned-tab pin
// authoritative, so a lost tab_removed cannot leave b.active on a tab id Chrome
// has reused. A missing AgentTabID clears ownership. Caller holds b.mu (lock
// order as resetProfileStateLocked).
func (b *Bridge) reconcileAgentPinLocked(h hello) {
	b.agentPinKnown = true
	if b.followFocus {
		return
	}

	previous := strings.TrimSpace(b.active)
	next := ""
	if h.AgentTabID != nil && *h.AgentTabID > 0 {
		next = strconv.Itoa(*h.AgentTabID)
	}
	if previous == next {
		return
	}
	if previous != "" {
		b.invalidateTabStateLocked(previous)
	}
	// Chrome may have reused next already; a changed pin is a new ownership epoch.
	if next != "" {
		b.invalidateTabStateLocked(next)
	}
	b.active = next
}

// invalidateTabStateLocked forgets everything keyed by a closed tab, since
// Chrome reuses numeric tab ids. Caller holds b.mu (lock order as
// resetProfileStateLocked).
func (b *Bridge) invalidateTabStateLocked(tabID string) {
	tabID = strings.TrimSpace(tabID)
	if tabID == "" {
		return
	}
	if b.active == tabID {
		b.active = ""
	}
	b.observeMu.Lock()
	delete(b.observedState, tabID)
	delete(b.observeVersions, tabID)
	b.observeMu.Unlock()
	b.downloadsMu.Lock()
	delete(b.downloadCursors, tabID)
	b.downloadsMu.Unlock()
	b.emulationMu.Lock()
	delete(b.emulationStates, tabID)
	b.emulationMu.Unlock()
	// The extension drops a closed tab's rules; a stale copy would be re-pushed
	// onto a replacement tab that reuses the id.
	b.routes.set(tabID, nil)
}

func (b *Bridge) invalidateTabState(tabID string) {
	b.mu.Lock()
	b.invalidateTabStateLocked(tabID)
	b.mu.Unlock()
}

// sameExtensionIdentity compares source, workspace, profile and label only, so
// an upgraded extension is the same identity. Identical configs collide; the
// flap guard handles that.
func sameExtensionIdentity(a, b hello) bool {
	return a.Source == b.Source &&
		a.Workspace == b.Workspace &&
		a.Profile == b.Profile &&
		a.Label == b.Label
}

const (
	pingKeepaliveInterval = 30 * time.Second
	pingTimeout           = 10 * time.Second
)

// keepAlive closes conn when a ping fails, which unblocks readLoop. It stops
// once conn is no longer the active connection.
func (b *Bridge) keepAlive(ctx context.Context, conn *websocket.Conn, interval time.Duration) {
	b.keepAliveWithFailure(ctx, conn, interval, nil)
}

// keepAliveWithFailure reports a failure before closing so the teardown can tell
// our own StatusGoingAway from the peer's.
func (b *Bridge) keepAliveWithFailure(ctx context.Context, conn *websocket.Conn, interval time.Duration, failures chan<- struct{}) {
	if interval <= 0 {
		interval = pingKeepaliveInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.mu.RLock()
			current := b.conn == conn
			b.mu.RUnlock()
			if !current {
				return
			}
			pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if failures != nil {
					select {
					case failures <- struct{}{}:
					default:
					}
				}
				_ = conn.Close(websocket.StatusGoingAway, "keepalive ping failed")
				return
			}
		}
	}
}

// verifyHandshake reads the first frame, which must be a hello, within
// handshakeTimeout. A wrong token is always rejected; a missing one when
// requireToken is set. A refused hello is returned with the error (token
// zeroed) so its reported endpoints can be recorded.
func (b *Bridge) verifyHandshake(ctx context.Context, conn *websocket.Conn) (hello, error) {
	verifyCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	_, data, err := conn.Read(verifyCtx)
	if err != nil {
		return hello{}, fmt.Errorf("read hello: %w", err)
	}
	var resp response
	if err := json.Unmarshal(data, &resp); err != nil {
		return hello{}, fmt.Errorf("invalid hello frame: %w", err)
	}
	// Zero the secret at once: rejection paths return resp.Hello to /status.
	presented := resp.Hello.Token
	resp.Hello.Token = ""
	if resp.Type != "hello" {
		return hello{}, fmt.Errorf("expected hello as first frame, got %q", resp.Type)
	}
	switch {
	case presented == "":
		// A local process can forge an Origin header, so without a token the bridge
		// authenticates nothing.
		if b.requireToken {
			return resp.Hello, errors.New("missing handshake token: the extension must present the per-launch token from /status (set BRW_BRIDGE_ALLOW_TOKENLESS=1 only for a pre-0.2.0 extension)")
		}
		b.compatWarnOnce.Do(func() {
			log.Printf("WARNING: extension connected without a handshake token and BRW_BRIDGE_ALLOW_TOKENLESS is set — the bridge is authenticating nothing and any local process can drive this browser. Reload the brw extension and unset the variable.")
		})
	case subtle.ConstantTimeCompare([]byte(presented), []byte(b.authToken)) != 1:
		return resp.Hello, errors.New("invalid handshake token")
	}
	return resp.Hello, nil
}

func (b *Bridge) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var resp response
		if err := json.Unmarshal(data, &resp); err != nil {
			// A chunk envelope can be valid JSON yet fail typed decoding. Recover its
			// routing header so that RPC fails now instead of at its deadline.
			var header struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			}
			if json.Unmarshal(data, &header) == nil && header.Type == "response_chunk" && header.ID != "" {
				if logical, ready := b.rejectResponseChunk(conn, header.ID, fmt.Errorf("malformed envelope: %w", err)); ready {
					b.deliverResponse(conn, logical)
				}
			}
			log.Printf("extension bridge invalid message: %v", err)
			continue
		}
		b.handleDecodedFrame(conn, resp)
	}
}

func formatStatusTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}
