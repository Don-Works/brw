package extensionbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type chunkTestResult struct {
	raw json.RawMessage
	err error
}

type chunkTestStats struct {
	logicalBytes int
	frameCount   int
	maxFrame     int
}

func newChunkTestHarness(t *testing.T, timeout time.Duration) (*Bridge, string, *websocket.Conn) {
	t.Helper()
	b := New("", timeout, "")
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	conn := connectChunkTestExtension(t, b, wsURL)
	return b, wsURL, conn
}

func connectChunkTestExtension(t *testing.T, b *Bridge, wsURL string) *websocket.Conn {
	t.Helper()
	conn, err := dialExtension(t, wsURL, testDefaultOrigin)
	if err != nil {
		t.Fatalf("dial extension bridge: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	waitUntil(t, b.liveConn)
	return conn
}

func readChunkTestRequest(ctx context.Context, conn *websocket.Conn) (request, error) {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return request{}, err
	}
	var req request
	if err := json.Unmarshal(data, &req); err != nil {
		return request{}, fmt.Errorf("decode bridge request: %w", err)
	}
	if req.ID == "" {
		return request{}, errors.New("bridge request has no id")
	}
	return req, nil
}

func writeChunkTestJSON(ctx context.Context, conn *websocket.Conn, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

func chunkTestInt(value int) *int { return &value }

func makeChunkTestFrames(logical response, chunkSize int) ([][]byte, chunkTestStats, error) {
	serialized, err := json.Marshal(logical)
	if err != nil {
		return nil, chunkTestStats{}, err
	}
	if chunkSize < 1 {
		return nil, chunkTestStats{}, errors.New("chunk size must be positive")
	}
	count := (len(serialized) + chunkSize - 1) / chunkSize
	if count > maxChunkedResponseChunks {
		return nil, chunkTestStats{}, fmt.Errorf("test response needs %d chunks", count)
	}
	stats := chunkTestStats{logicalBytes: len(serialized), frameCount: count}
	frames := make([][]byte, 0, count)
	for index := 0; index < count; index++ {
		start := index * chunkSize
		end := min(start+chunkSize, len(serialized))
		frame, err := json.Marshal(response{
			Type:       "response_chunk",
			ID:         logical.ID,
			Encoding:   "base64",
			ChunkIndex: chunkTestInt(index),
			ChunkCount: chunkTestInt(count),
			TotalBytes: chunkTestInt(len(serialized)),
			ChunkData:  base64.StdEncoding.EncodeToString(serialized[start:end]),
		})
		if err != nil {
			return nil, chunkTestStats{}, err
		}
		if len(frame) >= extensionFrameReadLimitBytes {
			return nil, chunkTestStats{}, fmt.Errorf("chunk frame is %d bytes, read limit is %d", len(frame), extensionFrameReadLimitBytes)
		}
		stats.maxFrame = max(stats.maxFrame, len(frame))
		frames = append(frames, frame)
	}
	return frames, stats, nil
}

func writeChunkTestFrames(ctx context.Context, conn *websocket.Conn, frames [][]byte) error {
	for _, frame := range frames {
		if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
			return err
		}
	}
	return nil
}

func assertChunkTestStateEmpty(t *testing.T, b *Bridge) {
	t.Helper()
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.responseChunks) != 0 || b.responseChunkBytes != 0 {
		t.Fatalf("partial chunk state leaked: assemblies=%d bytes=%d", len(b.responseChunks), b.responseChunkBytes)
	}
}

func TestChunkedResponseCrossesFrameLimitAndBridgeStaysUsable(t *testing.T) {
	b, _, conn := newChunkTestHarness(t, 8*time.Second)
	peerCtx, cancelPeer := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelPeer()
	peerDone := make(chan error, 1)
	statsCh := make(chan chunkTestStats, 1)
	largeValue := strings.Repeat("x", 5<<20)

	go func() {
		req, err := readChunkTestRequest(peerCtx, conn)
		if err != nil {
			peerDone <- err
			return
		}
		result, err := json.Marshal(map[string]string{"value": largeValue})
		if err != nil {
			peerDone <- err
			return
		}
		frames, stats, err := makeChunkTestFrames(response{ID: req.ID, OK: true, Result: result}, 2<<20)
		if err != nil {
			peerDone <- err
			return
		}
		statsCh <- stats
		if err := writeChunkTestFrames(peerCtx, conn, frames); err != nil {
			peerDone <- err
			return
		}

		req, err = readChunkTestRequest(peerCtx, conn)
		if err != nil {
			peerDone <- err
			return
		}
		peerDone <- writeChunkTestJSON(peerCtx, conn, response{
			ID: req.ID, OK: true, Result: json.RawMessage(`{"value":"still-usable"}`),
		})
	}()

	callCtx, cancelCall := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancelCall()
	raw, err := b.dispatch(callCtx, "chunk_test_large", nil)
	if err != nil {
		t.Fatalf("large chunked dispatch: %v", err)
	}
	stats := <-statsCh
	if stats.logicalBytes <= extensionFrameReadLimitBytes {
		t.Fatalf("logical fixture is %d bytes, want above %d-byte frame limit", stats.logicalBytes, extensionFrameReadLimitBytes)
	}
	if stats.frameCount < 2 || stats.maxFrame >= extensionFrameReadLimitBytes {
		t.Fatalf("bad chunk framing: %+v (frame limit %d)", stats, extensionFrameReadLimitBytes)
	}
	var largeGot struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &largeGot); err != nil {
		t.Fatalf("decode large result: %v", err)
	}
	if largeGot.Value != largeValue {
		t.Fatalf("large result mismatch: got %d bytes, want %d", len(largeGot.Value), len(largeValue))
	}
	assertChunkTestStateEmpty(t, b)

	raw, err = b.dispatch(callCtx, "chunk_test_after_large", nil)
	if err != nil {
		t.Fatalf("dispatch after large response: %v", err)
	}
	if string(raw) != `{"value":"still-usable"}` {
		t.Fatalf("result after large response = %s", raw)
	}
	if err := <-peerDone; err != nil {
		t.Fatalf("fake extension: %v", err)
	}
	assertChunkTestStateEmpty(t, b)
}

func TestMalformedChunkFailsOnlyItsRequestAndBridgeStaysUsable(t *testing.T) {
	tests := []struct {
		name          string
		errorContains string
		frames        func(string) []map[string]any
	}{
		{
			name: "malformed base64",
			frames: func(id string) []map[string]any {
				return []map[string]any{{
					"type": "response_chunk", "id": id, "encoding": "base64",
					"chunk_index": 0, "chunk_count": 1, "total_bytes": 1, "data": "%%%",
				}}
			},
		},
		{
			name: "out of order",
			frames: func(id string) []map[string]any {
				return []map[string]any{{
					"type": "response_chunk", "id": id, "encoding": "base64",
					"chunk_index": 1, "chunk_count": 2, "total_bytes": 2, "data": base64.StdEncoding.EncodeToString([]byte("x")),
				}}
			},
		},
		{
			name: "oversized total",
			frames: func(id string) []map[string]any {
				return []map[string]any{{
					"type": "response_chunk", "id": id, "encoding": "base64",
					"chunk_index": 0, "chunk_count": 1, "total_bytes": maxChunkedResponseBytes + 1, "data": base64.StdEncoding.EncodeToString([]byte("x")),
				}}
			},
		},
		{
			name: "missing metadata",
			frames: func(id string) []map[string]any {
				return []map[string]any{{
					"type": "response_chunk", "id": id, "encoding": "base64",
					"chunk_count": 1, "total_bytes": 1, "data": base64.StdEncoding.EncodeToString([]byte("x")),
				}}
			},
		},
		{
			name: "metadata has wrong json type",
			frames: func(id string) []map[string]any {
				return []map[string]any{{
					"type": "response_chunk", "id": id, "encoding": "base64",
					"chunk_index": "zero", "chunk_count": 1, "total_bytes": 1, "data": base64.StdEncoding.EncodeToString([]byte("x")),
				}}
			},
		},
		{
			name: "metadata changes",
			frames: func(id string) []map[string]any {
				return []map[string]any{
					{
						"type": "response_chunk", "id": id, "encoding": "base64",
						"chunk_index": 0, "chunk_count": 2, "total_bytes": 2, "data": base64.StdEncoding.EncodeToString([]byte("x")),
					},
					{
						"type": "response_chunk", "id": id, "encoding": "base64",
						"chunk_index": 1, "chunk_count": 3, "total_bytes": 2, "data": base64.StdEncoding.EncodeToString([]byte("y")),
					},
				}
			},
		},
		{
			name: "decoded bytes exceed total",
			frames: func(id string) []map[string]any {
				return []map[string]any{{
					"type": "response_chunk", "id": id, "encoding": "base64",
					"chunk_index": 0, "chunk_count": 1, "total_bytes": 1, "data": base64.StdEncoding.EncodeToString([]byte("xx")),
				}}
			},
		},
		{
			name: "reassembled payload is invalid json",
			frames: func(id string) []map[string]any {
				return []map[string]any{{
					"type": "response_chunk", "id": id, "encoding": "base64",
					"chunk_index": 0, "chunk_count": 1, "total_bytes": 1, "data": base64.StdEncoding.EncodeToString([]byte("{")),
				}}
			},
		},
		{
			name: "reassembled response id changes",
			frames: func(id string) []map[string]any {
				payload := []byte(`{"id":"different","ok":true}`)
				return []map[string]any{{
					"type": "response_chunk", "id": id, "encoding": "base64",
					"chunk_index": 0, "chunk_count": 1, "total_bytes": len(payload), "data": base64.StdEncoding.EncodeToString(payload),
				}}
			},
		},
		{
			name:          "direct extension error replaces partial response",
			errorContains: "deliberate extension failure",
			frames: func(id string) []map[string]any {
				return []map[string]any{
					{
						"type": "response_chunk", "id": id, "encoding": "base64",
						"chunk_index": 0, "chunk_count": 2, "total_bytes": 2, "data": base64.StdEncoding.EncodeToString([]byte("x")),
					},
					{"id": id, "ok": false, "error": "deliberate extension failure"},
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, _, conn := newChunkTestHarness(t, 4*time.Second)
			peerCtx, cancelPeer := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancelPeer()
			peerDone := make(chan error, 1)
			go func() {
				req, err := readChunkTestRequest(peerCtx, conn)
				if err != nil {
					peerDone <- err
					return
				}
				for _, frame := range tc.frames(req.ID) {
					if err := writeChunkTestJSON(peerCtx, conn, frame); err != nil {
						peerDone <- err
						return
					}
				}
				req, err = readChunkTestRequest(peerCtx, conn)
				if err != nil {
					peerDone <- err
					return
				}
				peerDone <- writeChunkTestJSON(peerCtx, conn, response{
					ID: req.ID, OK: true, Result: json.RawMessage(`{"value":"recovered"}`),
				})
			}()

			callCtx, cancelCall := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancelCall()
			wantError := tc.errorContains
			if wantError == "" {
				wantError = "invalid chunked extension response"
			}
			if _, err := b.dispatch(callCtx, "chunk_test_malformed", nil); err == nil || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("malformed response error = %v", err)
			}
			assertChunkTestStateEmpty(t, b)
			raw, err := b.dispatch(callCtx, "chunk_test_recovery", nil)
			if err != nil {
				t.Fatalf("bridge did not recover: %v", err)
			}
			if string(raw) != `{"value":"recovered"}` {
				t.Fatalf("recovery result = %s", raw)
			}
			if err := <-peerDone; err != nil {
				t.Fatalf("fake extension: %v", err)
			}
			assertChunkTestStateEmpty(t, b)
		})
	}
}

func TestChunkedResponseCancellationCleansStateAndBridgeStaysUsable(t *testing.T) {
	b, _, conn := newChunkTestHarness(t, 5*time.Second)
	peerCtx, cancelPeer := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancelPeer()
	chunkSent := make(chan struct{})
	peerDone := make(chan error, 1)
	go func() {
		req, err := readChunkTestRequest(peerCtx, conn)
		if err != nil {
			peerDone <- err
			return
		}
		if err := writeChunkTestJSON(peerCtx, conn, map[string]any{
			"type": "response_chunk", "id": req.ID, "encoding": "base64",
			"chunk_index": 0, "chunk_count": 2, "total_bytes": 2,
			"data": base64.StdEncoding.EncodeToString([]byte("x")),
		}); err != nil {
			peerDone <- err
			return
		}
		close(chunkSent)
		req, err = readChunkTestRequest(peerCtx, conn)
		if err != nil {
			peerDone <- err
			return
		}
		peerDone <- writeChunkTestJSON(peerCtx, conn, response{
			ID: req.ID, OK: true, Result: json.RawMessage(`{"value":"after-cancel"}`),
		})
	}()

	callCtx, cancelCall := context.WithCancel(context.Background())
	callDone := make(chan chunkTestResult, 1)
	go func() {
		raw, err := b.dispatch(callCtx, "chunk_test_cancel", nil)
		callDone <- chunkTestResult{raw: raw, err: err}
	}()
	<-chunkSent
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return len(b.responseChunks) == 1 && b.responseChunkBytes == 1
	})
	cancelCall()
	out := <-callDone
	if !errors.Is(out.err, context.Canceled) {
		t.Fatalf("cancelled dispatch error = %v", out.err)
	}
	assertChunkTestStateEmpty(t, b)

	recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancelRecovery()
	raw, err := b.dispatch(recoveryCtx, "chunk_test_after_cancel", nil)
	if err != nil {
		t.Fatalf("dispatch after cancellation: %v", err)
	}
	if string(raw) != `{"value":"after-cancel"}` {
		t.Fatalf("result after cancellation = %s", raw)
	}
	if err := <-peerDone; err != nil {
		t.Fatalf("fake extension: %v", err)
	}
}

func TestChunkedResponseDisconnectCleansStateAndReconnects(t *testing.T) {
	b, wsURL, conn := newChunkTestHarness(t, 5*time.Second)
	peerCtx, cancelPeer := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancelPeer()
	chunkSent := make(chan struct{})
	peerDone := make(chan error, 1)
	go func() {
		req, err := readChunkTestRequest(peerCtx, conn)
		if err != nil {
			peerDone <- err
			return
		}
		err = writeChunkTestJSON(peerCtx, conn, map[string]any{
			"type": "response_chunk", "id": req.ID, "encoding": "base64",
			"chunk_index": 0, "chunk_count": 2, "total_bytes": 2,
			"data": base64.StdEncoding.EncodeToString([]byte("x")),
		})
		if err == nil {
			close(chunkSent)
		}
		peerDone <- err
	}()

	disconnectCtx, cancelDisconnect := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDisconnect()
	callDone := make(chan chunkTestResult, 1)
	go func() {
		raw, err := b.dispatch(disconnectCtx, "chunk_test_disconnect", nil)
		callDone <- chunkTestResult{raw: raw, err: err}
	}()
	<-chunkSent
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return len(b.responseChunks) == 1
	})
	if err := conn.CloseNow(); err != nil {
		t.Fatalf("drop extension connection: %v", err)
	}
	out := <-callDone
	if out.err == nil || !strings.Contains(out.err.Error(), disconnectDrainReason) {
		t.Fatalf("disconnected dispatch error = %v", out.err)
	}
	if err := <-peerDone; err != nil {
		t.Fatalf("fake extension before disconnect: %v", err)
	}
	waitUntil(t, func() bool { return !b.liveConn() })
	assertChunkTestStateEmpty(t, b)

	reconnected := connectChunkTestExtension(t, b, wsURL)
	recoveryPeer := make(chan error, 1)
	go func() {
		req, err := readChunkTestRequest(peerCtx, reconnected)
		if err != nil {
			recoveryPeer <- err
			return
		}
		recoveryPeer <- writeChunkTestJSON(peerCtx, reconnected, response{
			ID: req.ID, OK: true, Result: json.RawMessage(`{"value":"after-reconnect"}`),
		})
	}()
	recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancelRecovery()
	raw, err := b.dispatch(recoveryCtx, "chunk_test_after_disconnect", nil)
	if err != nil {
		t.Fatalf("dispatch after reconnect: %v", err)
	}
	if string(raw) != `{"value":"after-reconnect"}` {
		t.Fatalf("result after reconnect = %s", raw)
	}
	if err := <-recoveryPeer; err != nil {
		t.Fatalf("fake extension after reconnect: %v", err)
	}
}

func TestShutdownDrainsPendingPartialResponseWithoutPeerCloseHandshake(t *testing.T) {
	b, _, conn := newChunkTestHarness(t, 5*time.Second)
	peerCtx, cancelPeer := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPeer()
	partialSent := make(chan struct{})
	peerDone := make(chan error, 1)
	go func() {
		req, err := readChunkTestRequest(peerCtx, conn)
		if err != nil {
			peerDone <- err
			return
		}
		err = writeChunkTestJSON(peerCtx, conn, map[string]any{
			"type": "response_chunk", "id": req.ID, "encoding": "base64",
			"chunk_index": 0, "chunk_count": 2, "total_bytes": 2,
			"data": base64.StdEncoding.EncodeToString([]byte("x")),
		})
		if err != nil {
			peerDone <- err
			return
		}
		close(partialSent)
		// Deliberately neither read nor close again. A graceful WebSocket close
		// would wait on this unresponsive peer's handshake.
		peerDone <- nil
	}()

	callCtx, cancelCall := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCall()
	callDone := make(chan chunkTestResult, 1)
	go func() {
		raw, err := b.dispatch(callCtx, "chunk_test_shutdown", nil)
		callDone <- chunkTestResult{raw: raw, err: err}
	}()
	<-partialSent
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return len(b.pending) == 1 && len(b.responseChunks) == 1 && b.responseChunkBytes == 1
	})

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	start := time.Now()
	err := b.Shutdown(shutdownCtx)
	cancelShutdown()
	if err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("shutdown waited for unresponsive peer: %v", elapsed)
	}

	out := <-callDone
	if out.err == nil || !strings.Contains(out.err.Error(), shutdownDrainReason) {
		t.Fatalf("shutdown-drained dispatch error = %v", out.err)
	}
	if err := <-peerDone; err != nil {
		t.Fatalf("fake extension before becoming unresponsive: %v", err)
	}
	b.mu.RLock()
	connected := b.conn != nil
	shuttingDown := b.shuttingDown
	pending := len(b.pending)
	partials := len(b.responseChunks)
	partialBytes := b.responseChunkBytes
	b.mu.RUnlock()
	if connected || !shuttingDown || pending != 0 || partials != 0 || partialBytes != 0 {
		t.Fatalf("shutdown state connected=%t shutting_down=%t pending=%d partials=%d bytes=%d",
			connected, shuttingDown, pending, partials, partialBytes)
	}

	// A post-shutdown call fails immediately and cannot repopulate pending.
	postCtx, cancelPost := context.WithTimeout(context.Background(), time.Second)
	defer cancelPost()
	start = time.Now()
	_, err = b.dispatch(postCtx, "chunk_test_after_shutdown", nil)
	if !errors.Is(err, errBridgeShuttingDown) {
		t.Fatalf("post-shutdown dispatch error = %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 100*time.Millisecond {
		t.Fatalf("post-shutdown dispatch did not fail immediately: %v", elapsed)
	}
	b.mu.RLock()
	pending = len(b.pending)
	b.mu.RUnlock()
	if pending != 0 {
		t.Fatalf("post-shutdown dispatch repopulated pending: %d", pending)
	}
}

func TestShutdownSignalsReconnectGateAndIsIdempotent(t *testing.T) {
	b := New("", time.Second, "")
	b.mu.RLock()
	ready := b.connReady
	b.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := b.Shutdown(ctx); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	select {
	case <-ready:
	default:
		t.Fatal("shutdown did not signal reconnect waiters")
	}
	if _, err := b.getConn(ctx); !errors.Is(err, errBridgeShuttingDown) {
		t.Fatalf("getConn after shutdown error = %v", err)
	}
	if err := b.Shutdown(ctx); err != nil {
		t.Fatalf("idempotent shutdown: %v", err)
	}
	assertChunkTestStateEmpty(t, b)
}

func TestChunkedResponsesInterleaveByRequestAndDispatchOnce(t *testing.T) {
	b, _, conn := newChunkTestHarness(t, 6*time.Second)
	peerCtx, cancelPeer := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelPeer()
	peerDone := make(chan error, 1)
	go func() {
		requests := make([]request, 0, 2)
		for len(requests) < 2 {
			req, err := readChunkTestRequest(peerCtx, conn)
			if err != nil {
				peerDone <- err
				return
			}
			requests = append(requests, req)
		}
		allFrames := make([][][]byte, 0, 2)
		for _, req := range requests {
			name, _ := req.Params["name"].(string)
			result, err := json.Marshal(map[string]string{"name": name, "body": strings.Repeat(name+"-", 1024)})
			if err != nil {
				peerDone <- err
				return
			}
			logical := response{ID: req.ID, OK: true, Result: result}
			serialized, err := json.Marshal(logical)
			if err != nil {
				peerDone <- err
				return
			}
			frames, _, err := makeChunkTestFrames(logical, (len(serialized)+1)/2)
			if err != nil {
				peerDone <- err
				return
			}
			if len(frames) != 2 {
				peerDone <- fmt.Errorf("expected two frames, got %d", len(frames))
				return
			}
			allFrames = append(allFrames, frames)
		}
		// Interleave two response IDs, then duplicate one completed final frame.
		// The duplicate must be ignored rather than delivered twice or retained.
		for _, frame := range [][]byte{allFrames[0][0], allFrames[1][0], allFrames[0][1], allFrames[0][1], allFrames[1][1]} {
			if err := conn.Write(peerCtx, websocket.MessageText, frame); err != nil {
				peerDone <- err
				return
			}
		}

		req, err := readChunkTestRequest(peerCtx, conn)
		if err != nil {
			peerDone <- err
			return
		}
		peerDone <- writeChunkTestJSON(peerCtx, conn, response{
			ID: req.ID, OK: true, Result: json.RawMessage(`{"value":"after-duplicate"}`),
		})
	}()

	callCtx, cancelCall := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancelCall()
	results := make(chan struct {
		name string
		chunkTestResult
	}, 2)
	for _, name := range []string{"alpha", "beta"} {
		name := name
		go func() {
			raw, err := b.dispatch(callCtx, "chunk_test_interleaved", map[string]any{"name": name})
			results <- struct {
				name string
				chunkTestResult
			}{name: name, chunkTestResult: chunkTestResult{raw: raw, err: err}}
		}()
	}
	for range 2 {
		out := <-results
		if out.err != nil {
			t.Fatalf("interleaved dispatch %s: %v", out.name, out.err)
		}
		var got struct {
			Name string `json:"name"`
			Body string `json:"body"`
		}
		if err := json.Unmarshal(out.raw, &got); err != nil {
			t.Fatalf("decode interleaved result %s: %v", out.name, err)
		}
		if got.Name != out.name || got.Body != strings.Repeat(out.name+"-", 1024) {
			t.Fatalf("interleaved result crossed request IDs: call=%q result=%q", out.name, got.Name)
		}
	}
	assertChunkTestStateEmpty(t, b)

	raw, err := b.dispatch(callCtx, "chunk_test_after_duplicate", nil)
	if err != nil {
		t.Fatalf("dispatch after duplicate final frame: %v", err)
	}
	if string(raw) != `{"value":"after-duplicate"}` {
		t.Fatalf("result after duplicate = %s", raw)
	}
	if err := <-peerDone; err != nil {
		t.Fatalf("fake extension: %v", err)
	}
	assertChunkTestStateEmpty(t, b)
}
