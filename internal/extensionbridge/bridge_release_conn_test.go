package extensionbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/coder/websocket"
)

// TestStaleConnTeardownPreservesLiveState proves the connection-replacement
// fix: when a displaced (stale) connection's readLoop returns, its teardown
// must NOT drain pending RPCs that belong to the live connection, must NOT
// clear b.conn, and must NOT stamp a disconnect reason while a healthy socket
// is still active. Only the active connection's own teardown drains.
//
// MV3 service workers reconnect constantly and handleExtension replaces the old
// conn with the new one, so the old conn's readLoop returning after the swap is
// a NORMAL occurrence, not an error path.
func TestStaleConnTeardownPreservesLiveState(t *testing.T) {
	b := New("", time.Second, "")
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{testDefaultOrigin}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test done")

	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})
	live := b.serverConn()

	// Register a pending RPC that belongs to the live connection.
	ch := make(chan response, 1)
	b.mu.Lock()
	b.pending["test-999"] = ch
	b.mu.Unlock()

	// A displaced/stale connection's readLoop returns. Its conn pointer is not
	// the active one (nil here stands in for "any conn that is not b.conn").
	// With the guard this must be a no-op against live state.
	b.releaseConn(nil, "stale closed")

	b.mu.Lock()
	_, stillPending := b.pending["test-999"]
	stillConnected := b.conn == live
	reason := b.disconnectReason
	b.mu.Unlock()
	if !stillPending {
		t.Fatal("stale-conn teardown drained the LIVE connection's pending RPC")
	}
	if !stillConnected {
		t.Fatal("stale-conn teardown cleared the live b.conn")
	}
	if reason != "" {
		t.Fatalf("stale-conn teardown stamped disconnect reason %q while still connected", reason)
	}

	// The live connection's own teardown MUST drain its pending RPCs.
	b.releaseConn(live, "closed")
	select {
	case r := <-ch:
		if r.Error == "" {
			t.Fatal("drained pending RPC should carry a disconnect error")
		}
	default:
		t.Fatal("live-conn teardown did not drain the pending RPC")
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.conn != nil {
		t.Fatal("live-conn teardown left b.conn non-nil")
	}
	if b.disconnectReason != "closed" {
		t.Fatalf("disconnectReason = %q, want \"closed\"", b.disconnectReason)
	}
}

// TestStaleDecodedFramesCannotMutateReplacement proves that the generation
// check covers every decoded frame, not only chunk bodies and disconnect
// teardown. A displaced authenticated socket may already have decoded a frame
// while waiting for b.mu; after the swap its hello/active hints and responses
// must be inert, while the live replacement can still complete pending calls.
func TestStaleDecodedFramesCannotMutateReplacement(t *testing.T) {
	const authMarker = "synthetic-stale-frame-auth"
	b := New("", 5*time.Second, "")
	b.SetAuthToken(authMarker)
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

	connA, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		t.Fatalf("dial A: %v", err)
	}
	defer connA.CloseNow()
	sendIdentityHello(t, connA, authMarker, "workspace-a", "profile-a", "browser A")
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil && b.hello.Workspace == "workspace-a"
	})
	staleServerConn := b.serverConn()

	connB, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		t.Fatalf("dial B: %v", err)
	}
	defer connB.CloseNow()
	sendIdentityHello(t, connB, authMarker, "workspace-b", "profile-b", "browser B")
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil && b.conn != staleServerConn && b.hello.Workspace == "workspace-b"
	})
	liveServerConn := b.serverConn()

	directCh := make(chan response, 1)
	chunkCh := make(chan response, 1)
	lastSeen := time.Unix(123, 456).UTC()
	b.mu.Lock()
	b.active = "84"
	b.lastSeenAt = lastSeen
	b.pending["stale-direct"] = directCh
	b.pending["stale-chunk"] = chunkCh
	b.mu.Unlock()

	// Model frames the old read loop decoded before CloseNow took effect, but did
	// not dispatch until after profile B became live.
	b.handleDecodedFrame(staleServerConn, response{Type: "hello", Hello: hello{
		Source: "brw-extension", Workspace: "workspace-a-stale", Profile: "profile-a", Label: "browser A",
	}})
	b.handleDecodedFrame(staleServerConn, response{Type: "active_tab", TabID: 42})
	b.handleDecodedFrame(staleServerConn, response{Type: "tab_removed", TabID: 84})
	b.handleDecodedFrame(staleServerConn, response{
		ID: "stale-direct", OK: true, Result: json.RawMessage(`{"from":"stale"}`),
	})
	chunkIndex, chunkCount, totalBytes := 0, 2, 2
	b.handleDecodedFrame(staleServerConn, response{
		Type: "response_chunk", ID: "stale-chunk", Encoding: "base64",
		ChunkIndex: &chunkIndex, ChunkCount: &chunkCount, TotalBytes: &totalBytes, ChunkData: "eA==",
	})

	b.mu.RLock()
	gotWorkspace, gotProfile := b.hello.Workspace, b.hello.Profile
	gotActive, gotLastSeen := b.active, b.lastSeenAt
	_, directPending := b.pending["stale-direct"]
	_, chunkPending := b.pending["stale-chunk"]
	partialChunks := len(b.responseChunks)
	b.mu.RUnlock()
	if gotWorkspace != "workspace-b" || gotProfile != "profile-b" {
		t.Fatalf("stale hello overwrote live identity: workspace=%q profile=%q", gotWorkspace, gotProfile)
	}
	if gotActive != "84" {
		t.Fatalf("stale active_tab overwrote live active id: %q", gotActive)
	}
	if !gotLastSeen.Equal(lastSeen) {
		t.Fatalf("stale frame advanced live last-seen time: got %v want %v", gotLastSeen, lastSeen)
	}
	if !directPending || !chunkPending || partialChunks != 0 {
		t.Fatalf("stale responses mutated pending/chunk state: direct=%t chunk=%t partial=%d", directPending, chunkPending, partialChunks)
	}
	select {
	case got := <-directCh:
		t.Fatalf("stale direct response was dispatched: %+v", got)
	default:
	}
	select {
	case got := <-chunkCh:
		t.Fatalf("stale chunk response was dispatched: %+v", got)
	default:
	}

	// The replacement generation still owns and can complete both requests.
	b.handleDecodedFrame(liveServerConn, response{
		ID: "stale-direct", OK: true, Result: json.RawMessage(`{"from":"live"}`),
	})
	b.handleDecodedFrame(liveServerConn, response{
		ID: "stale-chunk", OK: true, Result: json.RawMessage(`{"from":"live"}`),
	})
	for name, ch := range map[string]chan response{"direct": directCh, "chunk": chunkCh} {
		select {
		case got := <-ch:
			if !got.OK || string(got.Result) != `{"from":"live"}` {
				t.Fatalf("live %s response = %+v", name, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("live %s response was not dispatched", name)
		}
	}
}

func TestTabRemovedControlInvalidatesReusableTabState(t *testing.T) {
	b := New("", time.Second, "")
	live := &websocket.Conn{}
	b.mu.Lock()
	b.conn = live
	b.active = "42"
	b.mu.Unlock()
	b.observeMu.Lock()
	b.observedState["42"] = &browser.SemanticState{URL: "https://removed.example.test/"}
	b.observeVersions["42"] = 8
	b.observeMu.Unlock()
	b.downloadsMu.Lock()
	b.downloadCursors["42"] = 12
	b.downloadsMu.Unlock()
	b.emulationMu.Lock()
	b.emulationStates["42"] = bridgeDeviceEmulationState{
		HasBaseline: true,
		Baseline:    bridgeDeviceIdentity{UserAgent: "removed-tab-agent"},
	}
	b.emulationMu.Unlock()

	b.handleDecodedFrame(live, response{Type: "tab_removed", TabID: 42})

	b.mu.RLock()
	active := b.active
	b.mu.RUnlock()
	b.observeMu.Lock()
	_, observed := b.observedState["42"]
	_, versioned := b.observeVersions["42"]
	b.observeMu.Unlock()
	b.downloadsMu.Lock()
	_, cursor := b.downloadCursors["42"]
	b.downloadsMu.Unlock()
	b.emulationMu.Lock()
	_, emulated := b.emulationStates["42"]
	b.emulationMu.Unlock()
	if active != "" || observed || versioned || cursor || emulated {
		t.Fatalf("tab_removed retained reusable-id state: active=%q observed=%t versioned=%t cursor=%t emulated=%t", active, observed, versioned, cursor, emulated)
	}
	if version, changed := b.advanceObservation("42", browser.SemanticState{URL: "https://reused.example.test/"}); version != 1 || !changed {
		t.Fatalf("reused tab did not begin with fresh observation state: version=%d changed=%t", version, changed)
	}
}

// sendIdentityHello sends a full hello frame carrying a configured identity, so
// replace-time identity comparison has something to compare.
func sendIdentityHello(t *testing.T, conn *websocket.Conn, token, workspace, profile, label string, agentTabIDs ...int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	helloPayload := map[string]any{
		"source":    "brw-extension",
		"token":     token,
		"workspace": workspace,
		"profile":   profile,
		"label":     label,
	}
	if len(agentTabIDs) > 0 {
		helloPayload["agent_tab_id"] = agentTabIDs[0]
	}
	msg, _ := json.Marshal(map[string]any{
		"type":  "hello",
		"hello": helloPayload,
	})
	if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
		t.Fatalf("write hello: %v", err)
	}
}

// TestReplaceByDifferentExtensionDrainsPending proves the two sides of the
// replace-time pending contract:
//   - a replacement by a DIFFERENT extension identity (another browser profile
//     colliding onto this bridge) fails in-flight RPCs immediately with
//     replacedDrainReason — the displaced extension can never answer them, and
//     without the drain each call would hang for its full timeout;
//   - a replacement by the SAME identity (an MV3 service worker reconnecting
//     mid-call) preserves pending, because the same worker can still answer
//     over the new socket.
func TestReplaceByDifferentExtensionDrainsPending(t *testing.T) {
	const token = "replace-test-token"
	b := New("", time.Second, "")
	b.SetAuthToken(token)
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

	// CloseNow throughout: displaced conns are already dead server-side, and a
	// graceful Close would park ~5s each waiting for a close frame that never comes.
	connA, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		t.Fatalf("dial A: %v", err)
	}
	defer connA.CloseNow()
	sendIdentityHello(t, connA, token, "w1", "p1", "browser A")
	waitUntil(t, b.liveConn)

	chDrained := make(chan response, 1)
	b.mu.Lock()
	b.pending["replace-1"] = chDrained
	b.mu.Unlock()

	// A DIFFERENT identity takes over: pending must drain with replacedDrainReason.
	connB, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		t.Fatalf("dial B: %v", err)
	}
	defer connB.CloseNow()
	sendIdentityHello(t, connB, token, "w2", "p2", "browser B")
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil && b.hello.Workspace == "w2"
	})
	select {
	case r := <-chDrained:
		if r.Error != replacedDrainReason {
			t.Fatalf("drained RPC error = %q, want %q", r.Error, replacedDrainReason)
		}
	default:
		t.Fatal("identity-changing replace did not drain the in-flight RPC")
	}

	liveB := b.serverConn()
	chKept := make(chan response, 1)
	b.mu.Lock()
	b.pending["replace-2"] = chKept
	b.mu.Unlock()

	// The SAME identity reconnects (MV3 worker churn): pending must survive.
	connC, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		t.Fatalf("dial C: %v", err)
	}
	defer connC.CloseNow()
	sendIdentityHello(t, connC, token, "w2", "p2", "browser B")
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil && b.conn != liveB
	})
	select {
	case r := <-chKept:
		t.Fatalf("same-identity replace drained the in-flight RPC (error %q)", r.Error)
	default:
		// still pending — same worker may answer on the new socket
	}
	b.mu.RLock()
	_, stillPending := b.pending["replace-2"]
	b.mu.RUnlock()
	if !stillPending {
		t.Fatal("same-identity replace removed the pending RPC")
	}
}

// TestIdentityChangingReplaceResetsOnlyProfileBoundState proves that a browser
// profile takeover cannot inherit numeric tab ownership, observation state,
// recipe download cursors/fingerprints, or emulation baselines from the
// displaced profile. It also proves the inverse: a same-identity MV3 worker
// reconnect retains those caches rather than degrading every reconnect into a
// fresh browser session.
func TestIdentityChangingReplaceResetsOnlyProfileBoundState(t *testing.T) {
	const authMarker = "synthetic-profile-state-auth"
	b := New("", 5*time.Second, "")
	b.SetAuthToken(authMarker)
	b.SetFollowFocus(false)
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

	connA, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		t.Fatalf("dial A: %v", err)
	}
	defer connA.CloseNow()
	sendIdentityHello(t, connA, authMarker, "workspace-a", "profile-a", "browser A", 0)
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil && b.hello.Workspace == "workspace-a"
	})

	// Seed every profile-bound cache with tab id 42 and a completed download.
	// Profile B deliberately reuses both the numeric tab id and GUID below.
	staleDownload := browser.DownloadEntry{
		GUID:              "reused-guid",
		URL:               "https://example.test/invoice.pdf",
		SuggestedFilename: "invoice.pdf",
		TabID:             "42",
		State:             "completed",
		ReceivedBytes:     123,
		TotalBytes:        123,
		Path:              "/tmp/profile-a-invoice.pdf",
	}
	fingerprintJSON, err := json.Marshal(staleDownload)
	if err != nil {
		t.Fatalf("marshal stale download fingerprint: %v", err)
	}
	staleFailureAt := time.Now().Add(-time.Second)
	b.mu.Lock()
	b.active = "42"
	b.autoOpenFailedAt = staleFailureAt
	b.mu.Unlock()
	b.observeMu.Lock()
	b.observedState["42"] = &browser.SemanticState{URL: "https://profile-a.example.test/"}
	b.observeVersions["42"] = 9
	b.observeMu.Unlock()
	b.downloadsMu.Lock()
	b.downloadFingerprints[staleDownload.GUID] = string(fingerprintJSON)
	b.downloadVersions[staleDownload.GUID] = 7
	b.downloadCursors["42"] = 7
	b.downloadSequence = 7
	b.downloadsMu.Unlock()
	b.emulationMu.Lock()
	b.emulationStates["42"] = bridgeDeviceEmulationState{
		HasBaseline: true,
		Baseline:    bridgeDeviceIdentity{UserAgent: "profile-a-agent"},
	}
	b.emulationMu.Unlock()

	connB, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		t.Fatalf("dial B: %v", err)
	}
	defer connB.CloseNow()
	sendIdentityHello(t, connB, authMarker, "workspace-b", "profile-b", "browser B", 0)
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil && b.hello.Workspace == "workspace-b"
	})

	b.mu.RLock()
	active, failedAt := b.active, b.autoOpenFailedAt
	b.mu.RUnlock()
	if active != "" {
		t.Fatalf("identity-changing replace retained profile A's owned tab id %q", active)
	}
	if !failedAt.IsZero() {
		t.Fatalf("identity-changing replace retained auto-open failure time %v", failedAt)
	}
	b.observeMu.Lock()
	observedStates, observedVersions := len(b.observedState), len(b.observeVersions)
	b.observeMu.Unlock()
	if observedStates != 0 || observedVersions != 0 {
		t.Fatalf("identity-changing replace retained observation state: states=%d versions=%d", observedStates, observedVersions)
	}
	b.downloadsMu.Lock()
	fingerprints, versions, cursors, sequence := len(b.downloadFingerprints), len(b.downloadVersions), len(b.downloadCursors), b.downloadSequence
	b.downloadsMu.Unlock()
	if fingerprints != 0 || versions != 0 || cursors != 0 || sequence != 0 {
		t.Fatalf("identity-changing replace retained download state: fingerprints=%d versions=%d cursors=%d sequence=%d", fingerprints, versions, cursors, sequence)
	}
	b.emulationMu.Lock()
	emulations := len(b.emulationStates)
	b.emulationMu.Unlock()
	if emulations != 0 {
		t.Fatalf("identity-changing replace retained %d device-emulation states", emulations)
	}

	// A fresh profile-B download with the same GUID and exact fingerprint must be
	// visible to a recipe. Without the reset its old version (7) is at/below the
	// inherited tab cursor (7), so this returns an empty delta.
	peerDone := make(chan error, 1)
	go func() {
		peerCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, readErr := readChunkTestRequest(peerCtx, connB)
		if readErr != nil {
			peerDone <- readErr
			return
		}
		if req.Type != "get_downloads" {
			peerDone <- errors.New("expected get_downloads request, got " + req.Type)
			return
		}
		result, marshalErr := json.Marshal(map[string]any{
			"supported": true,
			"downloads": []browser.DownloadEntry{staleDownload},
		})
		if marshalErr != nil {
			peerDone <- marshalErr
			return
		}
		peerDone <- writeChunkTestJSON(peerCtx, connB, response{ID: req.ID, OK: true, Result: result})
	}()
	recipeCtx := browser.WithAllowedOrigins(browser.WithTabID(context.Background(), "42"), []string{"https://example.test"})
	fresh, err := b.Downloads(recipeCtx)
	if err != nil {
		t.Fatalf("profile B recipe downloads: %v", err)
	}
	if fresh.Count != 1 || len(fresh.Downloads) != 1 || fresh.Downloads[0].GUID != staleDownload.GUID {
		t.Fatalf("profile B reused GUID was suppressed by profile A state: %+v", fresh)
	}
	if err := <-peerDone; err != nil {
		t.Fatalf("profile B extension peer: %v", err)
	}

	// Re-seed the remaining caches, then reconnect the same identity. These
	// values must survive normal MV3 worker churn.
	preserveFailureAt := time.Now().Add(-2 * time.Second)
	b.mu.Lock()
	b.active = "42"
	b.autoOpenFailedAt = preserveFailureAt
	b.mu.Unlock()
	preservedObservation := &browser.SemanticState{URL: "https://profile-b.example.test/"}
	b.observeMu.Lock()
	b.observedState["42"] = preservedObservation
	b.observeVersions["42"] = 11
	b.observeMu.Unlock()
	preservedEmulation := bridgeDeviceEmulationState{
		HasBaseline: true,
		Baseline:    bridgeDeviceIdentity{UserAgent: "profile-b-agent"},
	}
	b.emulationMu.Lock()
	b.emulationStates["42"] = preservedEmulation
	b.emulationMu.Unlock()
	b.downloadsMu.Lock()
	preservedSequence := b.downloadSequence
	preservedCursor := b.downloadCursors["42"]
	preservedFingerprint := b.downloadFingerprints[staleDownload.GUID]
	b.downloadsMu.Unlock()
	liveB := b.serverConn()

	connC, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		t.Fatalf("dial same-identity C: %v", err)
	}
	defer connC.CloseNow()
	sendIdentityHello(t, connC, authMarker, "workspace-b", "profile-b", "browser B", 42)
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil && b.conn != liveB
	})

	b.mu.RLock()
	active, failedAt = b.active, b.autoOpenFailedAt
	b.mu.RUnlock()
	if active != "42" || !failedAt.Equal(preserveFailureAt) {
		t.Fatalf("same-identity reconnect lost ownership/cooldown: active=%q failed_at=%v", active, failedAt)
	}
	b.observeMu.Lock()
	gotObservation, gotObservationVersion := b.observedState["42"], b.observeVersions["42"]
	b.observeMu.Unlock()
	if gotObservation != preservedObservation || gotObservationVersion != 11 {
		t.Fatalf("same-identity reconnect lost observation state: state=%+v version=%d", gotObservation, gotObservationVersion)
	}
	b.downloadsMu.Lock()
	gotSequence := b.downloadSequence
	gotCursor := b.downloadCursors["42"]
	gotFingerprint := b.downloadFingerprints[staleDownload.GUID]
	b.downloadsMu.Unlock()
	if gotSequence != preservedSequence || gotCursor != preservedCursor || gotFingerprint != preservedFingerprint {
		t.Fatalf("same-identity reconnect lost download state: sequence=%d cursor=%d fingerprint_match=%t", gotSequence, gotCursor, gotFingerprint == preservedFingerprint)
	}
	b.emulationMu.Lock()
	gotEmulation, ok := b.emulationStates["42"]
	b.emulationMu.Unlock()
	if !ok || gotEmulation != preservedEmulation {
		t.Fatalf("same-identity reconnect lost emulation state: ok=%t state=%+v", ok, gotEmulation)
	}
}

// TestReconnectReconcilesLostRemovalBeforeNumericIDReuse models the dangerous
// gap directly: tab 42 is removed while the extension socket is down, so its
// best-effort tab_removed control frame cannot arrive, and Chrome reuses 42 for
// an unrelated foreground page. A no-tab_id request begun during that gap must
// wait for the replacement hello, reject the stale pin, and never substitute
// the foreground hint. Explicit tab_id remains available by design.
func TestReconnectReconcilesLostRemovalBeforeNumericIDReuse(t *testing.T) {
	const authMarker = "synthetic-pin-reconcile-auth"
	b := New("", 5*time.Second, "")
	b.SetAuthToken(authMarker)
	b.SetFollowFocus(false)
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

	connA, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		t.Fatalf("dial A: %v", err)
	}
	sendIdentityHello(t, connA, authMarker, "workspace-a", "profile-a", "browser A", 42)
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil && b.agentPinKnown && b.active == "42"
	})

	// Seed every tab-id-keyed cache so reconciliation proves more than clearing
	// the one active string.
	b.observeMu.Lock()
	b.observedState["42"] = &browser.SemanticState{URL: "https://old-agent.example.test/"}
	b.observeVersions["42"] = 9
	b.observeMu.Unlock()
	b.downloadsMu.Lock()
	b.downloadCursors["42"] = 17
	b.downloadsMu.Unlock()
	b.emulationMu.Lock()
	b.emulationStates["42"] = bridgeDeviceEmulationState{
		HasBaseline: true,
		Baseline:    bridgeDeviceIdentity{UserAgent: "old-agent"},
	}
	b.emulationMu.Unlock()
	// Once the new hello reports no pin, suppress auto-open so the resolution
	// result itself is observable rather than requiring an open_tab fake.
	b.mu.Lock()
	b.autoOpenFailedAt = time.Now()
	b.mu.Unlock()
	// Model the other side of the reconnect window too: the server resolved this
	// implicit pin while A was healthy, but the actual RPC will not dispatch until
	// after A has gone away.
	preResolved := b.ResolveActiveTabID(context.Background())
	if preResolved != "42" {
		t.Fatalf("pre-gap resolution = %q, want 42", preResolved)
	}
	preResolvedCtx := browser.WithCurrentOwnedTabID(context.Background(), preResolved)

	_ = connA.CloseNow()
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn == nil && !b.agentPinKnown && b.active == "42"
	})

	resolveCtx, cancelResolve := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelResolve()
	resolved := make(chan string, 1)
	go func() { resolved <- b.ResolveActiveTabID(resolveCtx) }()
	staleDispatch := make(chan error, 1)
	go func() {
		_, dispatchErr := b.cdp(preResolvedCtx, "", "Runtime.evaluate", map[string]any{"expression": "true"})
		staleDispatch <- dispatchErr
	}()
	select {
	case got := <-resolved:
		t.Fatalf("gap resolution returned stale tab %q before reconnect hello", got)
	case <-time.After(100 * time.Millisecond):
		// Correct: waiting for authoritative ownership from the new generation.
	}
	select {
	case dispatchErr := <-staleDispatch:
		t.Fatalf("pre-resolved RPC returned before reconnect hello: %v", dispatchErr)
	default:
	}

	connB, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		t.Fatalf("dial B: %v", err)
	}
	defer connB.CloseNow()
	// Current extension explicitly says its owned pin was lost.
	sendIdentityHello(t, connB, authMarker, "workspace-a", "profile-a", "browser A", 0)
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil && b.agentPinKnown && b.active == ""
	})

	// The same numeric id is now merely the user's foreground tab. Isolation must
	// ignore this hint and leave ownership empty.
	activeCtx, cancelActive := context.WithTimeout(context.Background(), time.Second)
	if err := writeChunkTestJSON(activeCtx, connB, response{Type: "active_tab", TabID: 42}); err != nil {
		cancelActive()
		t.Fatalf("send reused foreground hint: %v", err)
	}
	cancelActive()
	select {
	case got := <-resolved:
		if got != "" {
			t.Fatalf("gap resolution retargeted reused/foreground tab: got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("gap resolution did not finish after authoritative reconnect hello")
	}
	select {
	case dispatchErr := <-staleDispatch:
		if dispatchErr == nil || !strings.Contains(dispatchErr.Error(), "implicit tab 42 is no longer extension-owned") {
			t.Fatalf("pre-resolved gap RPC error = %v, want deterministic ownership rejection", dispatchErr)
		}
	case <-time.After(time.Second):
		t.Fatal("pre-resolved gap RPC did not reject after reconnect reconciliation")
	}

	// Fail-closed applies only to implicit ownership. A caller that deliberately
	// names tab 42 can still target it, proving the live bridge was not wedged.
	// Its response also forms an in-order barrier after active_tab on the same
	// socket, so the state assertions below cannot pass before that control frame
	// has actually been handled.
	peerDone := make(chan error, 1)
	go func() {
		peerCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, readErr := readChunkTestRequest(peerCtx, connB)
		if readErr != nil {
			peerDone <- readErr
			return
		}
		tabID, tabIDOK := req.Params["tabId"].(float64)
		if req.Type != "cdp" || !tabIDOK || tabID != 42 {
			peerDone <- fmt.Errorf("explicit reused-tab request = type %q params %+v", req.Type, req.Params)
			return
		}
		peerDone <- writeChunkTestJSON(peerCtx, connB, response{ID: req.ID, OK: true, Result: json.RawMessage(`{"result":{"value":true}}`)})
	}()
	explicitCtx := browser.WithTabID(context.Background(), "42")
	if _, err := b.cdp(explicitCtx, "42", "Runtime.evaluate", map[string]any{"expression": "true"}); err != nil {
		t.Fatalf("explicit reused-tab call after reconciliation: %v", err)
	}
	if err := <-peerDone; err != nil {
		t.Fatalf("replacement extension peer: %v", err)
	}
	// A session lease is also server-selected, but it can legitimately own one of
	// several agent tabs and is therefore deliberately not tied to the extension's
	// single global pin. Prove the new continuity marker did not turn all implicit
	// contexts into a global-active check.
	leasePeerDone := make(chan error, 1)
	go func() {
		peerCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, readErr := readChunkTestRequest(peerCtx, connB)
		if readErr != nil {
			leasePeerDone <- readErr
			return
		}
		tabID, tabIDOK := req.Params["tabId"].(float64)
		if req.Type != "cdp" || !tabIDOK || tabID != 99 {
			leasePeerDone <- fmt.Errorf("leased-tab request = type %q params %+v", req.Type, req.Params)
			return
		}
		leasePeerDone <- writeChunkTestJSON(peerCtx, connB, response{ID: req.ID, OK: true, Result: json.RawMessage(`{"result":{"value":true}}`)})
	}()
	leasedCtx := browser.WithImplicitTabID(context.Background(), "99")
	if _, err := b.cdp(leasedCtx, "", "Runtime.evaluate", map[string]any{"expression": "true"}); err != nil {
		t.Fatalf("leased multi-agent tab after reconciliation: %v", err)
	}
	if err := <-leasePeerDone; err != nil {
		t.Fatalf("leased-tab extension peer: %v", err)
	}

	b.mu.RLock()
	active := b.active
	b.mu.RUnlock()
	b.observeMu.Lock()
	_, observed := b.observedState["42"]
	_, versioned := b.observeVersions["42"]
	b.observeMu.Unlock()
	b.downloadsMu.Lock()
	_, cursor := b.downloadCursors["42"]
	b.downloadsMu.Unlock()
	b.emulationMu.Lock()
	_, emulated := b.emulationStates["42"]
	b.emulationMu.Unlock()
	if active != "" || observed || versioned || cursor || emulated {
		t.Fatalf("reconnect retained reusable-id state: active=%q observed=%t versioned=%t cursor=%t emulated=%t", active, observed, versioned, cursor, emulated)
	}
}

// TestReconnectOldHelloIsCompatibleButFailsClosed proves agent_tab_id is an
// additive protocol field: an older extension's otherwise valid authenticated
// hello is accepted, but it cannot cause stale numeric ownership to survive.
func TestReconnectOldHelloIsCompatibleButFailsClosed(t *testing.T) {
	const authMarker = "synthetic-old-pin-hello-auth"
	b := New("", 5*time.Second, "")
	b.SetAuthToken(authMarker)
	b.SetFollowFocus(false)
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"

	connA, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		t.Fatalf("dial A: %v", err)
	}
	sendIdentityHello(t, connA, authMarker, "workspace-a", "profile-a", "browser A", 57)
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil && b.agentPinKnown && b.active == "57"
	})
	_ = connA.CloseNow()
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn == nil && !b.agentPinKnown
	})

	connB, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		t.Fatalf("dial old extension: %v", err)
	}
	defer connB.CloseNow()
	// No optional agent_tab_id argument models the previous wire schema.
	sendIdentityHello(t, connB, authMarker, "workspace-a", "profile-a", "browser A")
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil && b.agentPinKnown
	})
	b.mu.RLock()
	active, connected := b.active, b.conn != nil
	b.mu.RUnlock()
	if !connected {
		t.Fatal("old extension hello was rejected instead of accepted compatibly")
	}
	if active != "" {
		t.Fatalf("old extension hello retained stale owned tab %q", active)
	}
}
