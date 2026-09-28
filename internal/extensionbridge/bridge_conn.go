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
	// Reject any connection that does not present a chrome-extension Origin. This
	// closes the coder/websocket gap where an ABSENT Origin (a non-browser local
	// client — curl, a rogue script) is treated as same-origin and accepted; only
	// a real extension carries a chrome-extension:// Origin, and a browser web
	// page cannot forge one.
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

	// When a per-launch token is configured, authenticate the hello BEFORE this
	// connection becomes the live bridge — so an unverified client can neither
	// displace the real extension nor receive a single command. With no token
	// (library/embedder/test) the connection goes live immediately, as before.
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
	// Flap guard: two browser profiles running brw against one bridge otherwise
	// displace each other forever (the flashing icon). While holding after a
	// detected flap and the current connection is still live, reject the newcomer
	// and extend the hold so the live connection stays put. Once the live
	// connection actually dies, b.conn is nil and the next connection is accepted
	// normally — so a legitimate reconnect after a real drop still works.
	if b.conn != nil && now.Before(b.flapHoldUntil) {
		b.flapHoldUntil = now.Add(flapHoldDuration)
		b.mu.Unlock()
		_ = conn.Close(websocket.StatusTryAgainLater, "another extension already holds this bridge (flap-hold active)")
		return
	}
	if b.conn != nil {
		// A live connection is being replaced. Track the churn rate; a burst within
		// flapWindow means two extensions are colliding, not a single reconnect.
		cutoff := now.Add(-flapWindow)
		kept := b.recentReplaces[:0]
		for _, t := range b.recentReplaces {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		b.recentReplaces = append(kept, now)
		if len(b.recentReplaces) >= flapThreshold {
			// Collision: keep the CURRENT connection, reject this newcomer, hold.
			b.flapHoldUntil = now.Add(flapHoldDuration)
			b.recentReplaces = b.recentReplaces[:0]
			b.mu.Unlock()
			log.Printf("brw: extension connection flap detected (%d+ replacements in %s) on bridge %s — two browser profiles likely have brw enabled on the same bridge. Holding the current connection and rejecting extras; disable brw in the other profile, or give it its own --bridge-addr.", flapThreshold, flapWindow, b.addr)
			_ = conn.Close(websocket.StatusTryAgainLater, "extension flap: holding current connection")
			return
		}
		// CloseNow, not Close: a graceful close performs a close handshake and
		// waits up to 5s for the displaced peer's ack — while this whole block
		// holds b.mu, so an unresponsive displaced connection froze every
		// dispatch and /status probe on the bridge for those 5 seconds. The
		// socket is being force-discarded either way; tear it down immediately
		// (its readLoop unblocks at once and releaseConn no-ops as stale).
		_ = b.conn.CloseNow()
		// A logical response may never span connection generations: a missing
		// frame from the displaced socket would otherwise be silently combined
		// with a new response that happens to reuse its request id.
		b.clearAllResponseChunksLocked()
		// Pending RPCs survive a SAME-extension replace on purpose: an MV3
		// service worker reconnecting mid-call can still answer them over the
		// new socket. But when the newcomer presents a DIFFERENT identity
		// (another browser profile colliding onto this bridge), the displaced
		// extension will never answer — fail those calls now rather than
		// letting each one hang for its full timeout.
		if !sameExtensionIdentity(b.hello, verifiedHello) {
			b.drainPendingLocked(replacedDrainReason)
			b.resetProfileStateLocked()
		}
	}
	b.conn = conn
	b.hello = verifiedHello
	// A new socket can be a restarted service worker, which has lost every
	// per-tab arm; re-send them rather than trust a record of the old worker.
	b.containment.reset()
	b.webmcpArm.reset()
	// Reconcile the extension-owned pin before publishing this connection through
	// connReady. A tabs.onRemoved frame is best-effort and can be lost while the
	// MV3 worker/socket is down; the next hello is the authoritative recovery
	// boundary. Missing agent_tab_id means an old extension and is accepted but
	// fails closed (no cached ownership), never falling back to foreground state.
	b.reconcileAgentPinLocked(verifiedHello)
	b.connectedAt = now
	b.lastSeenAt = now
	b.disconnectedAt = time.Time{}
	b.disconnectReason = ""
	// Wake any calls parked in getConn waiting for the socket to come back, and
	// arm a fresh gate for the next disconnect→reconnect cycle. Always
	// close-then-replace under mu so the channel is closed exactly once.
	close(b.connReady)
	b.connReady = make(chan struct{})
	b.mu.Unlock()

	if verifiedHello.Build != "" || verifiedHello.Label != "" {
		log.Printf("extension bridge connected (build %s, label %q)", verifiedHello.Build, verifiedHello.Label)
	} else {
		log.Printf("extension bridge connected")
	}
	b.recordBridgeUsage("bridge_connect", "ok", "", "", verifiedHello.Build)

	// Keepalive: ping the extension periodically so a half-open link (laptop
	// sleep, NAT timeout, dropped Wi-Fi) is detected promptly instead of hanging
	// until a request times out. A failed ping closes the conn, which unblocks
	// readLoop's conn.Read and drains b.pending. The pinger exits cleanly when the
	// read loop returns (pingCancel) so it never leaks.
	pingCtx, pingCancel := context.WithCancel(r.Context())
	pingFailure := make(chan struct{}, 1)
	go b.keepAliveWithFailure(pingCtx, conn, pingKeepaliveInterval, pingFailure)

	readErr := b.readLoop(r.Context(), conn)
	pingCancel()
	reason, expected := bridgeDisconnectReason(readErr)
	select {
	case <-pingFailure:
		// The close frame used to unblock readLoop is intentionally routine, but
		// the ping failure that caused it is not. Preserve that classification for
		// the single unexpected-disconnect record below.
		reason, expected = "keepalive ping failed", false
	default:
	}
	// A replaced connection is stale by definition. Its read loop normally ends
	// with "use of closed network connection" after the new socket is already
	// live; do not emit a false disconnect log or ledger error for that routine
	// MV3 lifecycle event.
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

// bridgeDisconnectReason separates routine WebSocket lifecycle from a real
// transport failure. WebSocket close reasons are canonical rather than copied
// from the peer, keeping arbitrary close-frame text out of logs and status.
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

// releaseConn tears down a connection that has stopped reading. It only acts
// when conn is still the bridge's ACTIVE connection: an MV3 service worker
// reconnects constantly, and handleExtension deliberately replaces an old conn
// with a new one (b.conn = newConn). When the displaced (stale) conn's readLoop
// finally returns it must NOT drain pending RPCs that now belong to the live
// connection, nor stamp the bridge "disconnected" while b.conn points at a
// healthy socket — doing so spuriously fails in-flight calls and reports a
// disconnect reason alongside connected:true.
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

// recordTabGroupDegradation captures the important middle state where opening
// the tab succeeded but Chromium refused the organizational group assignment.
// This happens with tab-strip implementations that report a normal window yet
// reject extension grouping, or when Chromium itself regresses the capability.
// Only the fixed capability class/failure shape reaches the metadata ledger;
// the group title, URL, and raw warning never do.
func (b *Bridge) recordTabGroupDegradation(warning string) {
	if strings.TrimSpace(warning) == "" {
		return
	}
	b.mu.RLock()
	build := b.hello.Build
	b.mu.RUnlock()
	b.recordBridgeUsage("tab_group_assignment", "degraded", "capability", warning, build)
}

// drainPendingLocked fails every in-flight RPC with the given reason. Callers
// must hold b.mu.
func (b *Bridge) drainPendingLocked(reason string) {
	b.clearAllResponseChunksLocked()
	for id, ch := range b.pending {
		delete(b.pending, id)
		ch <- response{ID: id, Error: reason}
		close(ch)
	}
}

// clearResponseChunksLocked releases one partial logical response. Callers must
// hold b.mu.
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

// clearAllResponseChunksLocked releases every partial response. Callers must
// hold b.mu.
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

// decodeResponseChunk validates the self-contained envelope fields and decodes
// one frame. Cross-frame ordering and accounting are checked while holding the
// bridge mutex in consumeResponseChunk.
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
	// A padded base64 length can overstate decoded bytes by at most two. Reject
	// obviously impossible envelopes before allocating their decoded buffer.
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

// rejectResponseChunk discards the partial response and turns a protocol fault
// into the one response awaited by dispatch. A stale socket may not clear state
// belonging to its replacement, and a cancelled request receives no late reply.
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

// consumeResponseChunk accepts only a contiguous, metadata-consistent sequence
// for an outstanding request. It returns ready=true exactly once, either for the
// complete logical response or for a bounded protocol error that fails the RPC.
func (b *Bridge) consumeResponseChunk(conn *websocket.Conn, frame response) (logical response, ready bool) {
	// Avoid decoding a multi-megabyte frame for a cancelled/unknown request. This
	// first check is repeated after decoding because dispatch may cancel meanwhile.
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

// deliverResponse atomically claims an outstanding request before sending its
// result, so a final chunk, duplicate frame, cancellation, or disconnect can
// never dispatch the same logical response twice.
func (b *Bridge) deliverResponse(conn *websocket.Conn, resp response) {
	b.mu.Lock()
	// A decoded response can be waiting on b.mu while handleExtension replaces
	// its socket. Never let that displaced connection claim a request belonging
	// to the live connection generation.
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

// handleDecodedFrame applies the connection-generation gate shared by every
// successfully decoded frame. Control frames mutate live bridge state while the
// same lock still proves their socket is current; response paths repeat the
// check when they claim a pending request because chunk decoding/reassembly can
// yield between this gate and delivery.
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
		// Tokenless library/test embedders publish the socket before receiving a
		// hello for backwards compatibility. Treat a later hello as the same
		// authoritative pin boundary used by the authenticated handshake.
		b.reconcileAgentPinLocked(h)
		b.mu.Unlock()
		return
	}
	if resp.Type == "active_tab" {
		// active_tab is the USER's live-focus hint, pushed when they switch
		// tabs. Honor it only in follow-focus mode; in isolation the cached
		// active id must track the tab brw OWNS, so a user tab-switch must not
		// repoint it onto the user's tab (that is the stomping we prevent).
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

// resetProfileStateLocked discards state whose keys and values only make sense
// inside the connected browser profile. Chrome tab ids and download GUIDs can
// both be reused by another profile, so retaining any of this state across an
// identity-changing replacement can target an unrelated tab or suppress a new
// download as already observed. The same-extension MV3 reconnect path does not
// call this helper and deliberately retains all of this state.
//
// Caller must hold b.mu. The lock order is b.mu -> observeMu -> downloadsMu ->
// emulationMu; code using the subordinate locks must not acquire b.mu while one
// is held.
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

// reconcileAgentPinLocked makes the newly connected extension's owned-tab pin
// authoritative. In isolation this closes the reconnect-gap hole where a lost
// tab_removed frame left b.active pointing at a numeric tab id Chrome had since
// reused for an unrelated page. The user's foreground hint is intentionally not
// consulted. Old extensions omit AgentTabID; accepting their connection while
// clearing ownership preserves protocol compatibility without weakening
// isolation. Same-pin reconnects retain per-tab caches across ordinary MV3
// worker churn.
//
// Caller must hold b.mu and therefore follows the subordinate lock order
// documented by resetProfileStateLocked.
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
	// Chrome may already have reused next for a fresh tab while the daemon still
	// carries cache/cursor/emulation entries under that numeric id. A changed pin
	// establishes a new ownership epoch, so scrub the destination too.
	if next != "" {
		b.invalidateTabStateLocked(next)
	}
	b.active = next
}

// invalidateTabStateLocked forgets everything keyed by an authoritatively
// disappeared Chrome tab. Numeric tab ids can later be reused, so retaining
// ownership, observation/cursor state, or an emulation baseline could either
// suppress fresh events or apply old-tab state to an unrelated target.
// Caller must hold b.mu and therefore follows the same subordinate lock order
// documented by resetProfileStateLocked.
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
	// Chrome reuses numeric tab ids. The extension drops the closed tab's
	// declarativeNetRequest rules, so keeping the daemon's copy would make the
	// next brw_route on a replacement tab re-push a rule set the agent driving it
	// never asked for.
	b.routes.set(tabID, nil)
}

func (b *Bridge) invalidateTabState(tabID string) {
	b.mu.Lock()
	b.invalidateTabStateLocked(tabID)
	b.mu.Unlock()
}

// sameExtensionIdentity reports whether two hellos describe the same configured
// extension instance (source + workspace/profile/label). Build, browser UA, and
// protocol version are deliberately ignored: the same extension reconnecting
// after a code upgrade is still the same identity. Two token-less or
// identically-configured extensions compare equal — the flap guard, not this
// check, handles that pathological case.
func sameExtensionIdentity(a, b hello) bool {
	return a.Source == b.Source &&
		a.Workspace == b.Workspace &&
		a.Profile == b.Profile &&
		a.Label == b.Label
}

// pingKeepaliveInterval is how often the bridge pings the connected extension to
// detect a half-open link. Each ping is bounded by its own short deadline so a
// dead link is surfaced well within the interval.
const (
	pingKeepaliveInterval = 30 * time.Second
	pingTimeout           = 10 * time.Second
)

// keepAlive pings the extension every interval. A ping that fails (or times out)
// means the link is dead/half-open: the conn is closed, which unblocks readLoop
// and drains b.pending. The goroutine exits when ctx is cancelled (the read loop
// returned) — no leak. It pings only while this conn is still the bridge's
// active conn, so a replaced connection's pinger goes quiet on its next tick.
// interval is a parameter (not the const directly) so tests can drive it fast.
func (b *Bridge) keepAlive(ctx context.Context, conn *websocket.Conn, interval time.Duration) {
	b.keepAliveWithFailure(ctx, conn, interval, nil)
}

// keepAliveWithFailure is keepAlive with an optional one-shot failure report
// for handleExtension. Reporting before closing the socket lets the read-loop
// teardown distinguish a peer's normal StatusGoingAway from a StatusGoingAway
// sent locally to unblock a dead connection.
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

// verifyHandshake reads the first frame of a freshly-accepted connection. It
// must be a hello (any other first frame is refused). A token that is PRESENT
// must match the configured one — a wrong token is always rejected. A MISSING
// token is accepted by default (graceful: a pre-0.2.0 extension that hasn't been
// reloaded still works, so upgrading the daemon never bricks it) unless
// requireToken is set, which makes the token mandatory. Bounded by
// handshakeTimeout so a silent client cannot hold the slot open. Only called
// when b.authToken != "".
//
// A REFUSED hello is still returned alongside the error, with its token zeroed:
// the caller records the endpoint config it reported, which is the only evidence
// anywhere on the machine of which status URL the extension is really using.
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
	// Lift the secret out of the struct immediately: every path below returns
	// resp.Hello, including the rejection paths whose value is recorded on
	// /status, and a secret that is still in the struct is one refactor away from
	// being echoed there.
	presented := resp.Hello.Token
	resp.Hello.Token = ""
	if resp.Type != "hello" {
		return hello{}, fmt.Errorf("expected hello as first frame, got %q", resp.Type)
	}
	switch {
	case presented == "":
		// No token. The Origin check above rejects browser web pages, but a local
		// process running as this user can forge an Origin header, so without a
		// token the bridge authenticates nothing: any such process can displace the
		// real extension and drive the signed-in browser. Refuse by default.
		if b.requireToken {
			return resp.Hello, errors.New("missing handshake token: the extension must present the per-launch token from /status (set BRW_BRIDGE_ALLOW_TOKENLESS=1 only for a pre-0.2.0 extension)")
		}
		b.compatWarnOnce.Do(func() {
			log.Printf("WARNING: extension connected without a handshake token and BRW_BRIDGE_ALLOW_TOKENLESS is set — the bridge is authenticating nothing and any local process can drive this browser. Reload the brw extension and unset the variable.")
		})
	case subtle.ConstantTimeCompare([]byte(presented), []byte(b.authToken)) != 1:
		// A token was presented but does not match — tampering or a stale token.
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
			// A syntactically valid chunk envelope can still fail typed decoding
			// (for example chunk_index:"zero" or an overflowing total_bytes).
			// Recover just its routing header on this cold error path so the one
			// affected RPC fails immediately and its partial buffer is released,
			// rather than hanging until its context deadline.
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
