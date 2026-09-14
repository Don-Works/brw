package extensionbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Don-Works/brw/internal/baseline"
	"github.com/Don-Works/brw/internal/mcp"
	"github.com/Don-Works/brw/internal/snapshot"
)

// serveBaselineStub answers the three things brw_baseline asks a transport for:
// the ARIA structure, the environment fingerprint, and a screenshot. The
// screenshot is JPEG because that is what Bridge.Screenshot actually requests
// from Page.captureScreenshot.
func serveBaselineStub(t *testing.T, b *Bridge, evaluate func(expression string) any, capture func() []byte) func() {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{testDefaultOrigin}},
	})
	if err != nil {
		srv.Close()
		t.Fatalf("dial bridge: %v", err)
	}
	conn.SetReadLimit(extensionFrameReadLimitBytes)
	waitUntil(t, b.liveConn)

	done := make(chan struct{})
	serveCtx, serveCancel := context.WithCancel(context.Background())
	go func() {
		defer close(done)
		for {
			_, data, readErr := conn.Read(serveCtx)
			if readErr != nil {
				return
			}
			var msg request
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			var result any = map[string]any{}
			switch msg.Type {
			case "capture_screenshot":
				result = map[string]any{"data": base64.StdEncoding.EncodeToString(capture())}
			case "cdp":
				if method, _ := msg.Params["method"].(string); method == "Runtime.evaluate" {
					params, _ := msg.Params["params"].(map[string]any)
					expression, _ := params["expression"].(string)
					result = map[string]any{"result": map[string]any{"value": evaluate(expression)}}
				}
			}
			answer, _ := json.Marshal(map[string]any{"id": msg.ID, "ok": true, "result": result})
			_ = conn.Write(serveCtx, websocket.MessageText, answer)
		}
	}()
	return func() {
		serveCancel()
		_ = conn.Close(websocket.StatusNormalClosure, "test done")
		srv.Close()
		<-done
	}
}

func baselineStubJPEG(t *testing.T, patched bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 20, 10))
	for y := 0; y < 10; y++ {
		for x := 0; x < 20; x++ {
			pixel := color.RGBA{R: 255, G: 255, B: 255, A: 255}
			if patched && x < 10 {
				pixel = color.RGBA{R: 20, G: 80, B: 190, A: 255}
			}
			img.SetRGBA(x, y, pixel)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

// toolSession is an MCP server speaking its real stdio JSON-RPC protocol over a
// pair of pipes, with the bridge underneath it as the browser controller.
//
// Reaching for the tool rather than for the steps it performs is the point: a
// test that evaluates, captures and compares by hand pins "the bridge can feed
// baseline.Check" and leaves the routing that decides whether brw_baseline runs
// on this transport at all completely unexercised.
type toolSession struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *bufio.Reader
	done chan error
	id   int
}

func startToolSession(t *testing.T, b *Bridge, store *baseline.Store) *toolSession {
	t.Helper()
	requests, requestWriter := io.Pipe()
	responseReader, responses := io.Pipe()
	server := mcp.New(b)
	server.SetBaselineStore(store)
	session := &toolSession{t: t, in: requestWriter, out: bufio.NewReader(responseReader), done: make(chan error, 1)}
	go func() {
		err := server.Serve(context.Background(), requests, responses)
		_ = responses.CloseWithError(io.EOF)
		session.done <- err
	}()
	t.Cleanup(func() {
		_ = requestWriter.Close()
		select {
		case err := <-session.done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the MCP server did not stop after its input closed")
		}
	})
	return session
}

// call makes one tools/call and returns the tool's decoded result.
func (s *toolSession) call(name string, arguments map[string]any) map[string]any {
	s.t.Helper()
	s.id++
	id := s.id
	encoded, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": arguments},
	})
	if err != nil {
		s.t.Fatalf("marshal request: %v", err)
	}
	if _, err := s.in.Write(append(encoded, '\n')); err != nil {
		s.t.Fatalf("write request: %v", err)
	}
	for {
		line, err := s.out.ReadBytes('\n')
		if err != nil {
			s.t.Fatalf("read response: %v", err)
		}
		var response struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(line, &response) != nil || response.ID != id {
			continue
		}
		if len(response.Error) > 0 {
			s.t.Fatalf("%s failed: %s", name, response.Error)
		}
		var envelope struct {
			IsError           bool                    `json:"isError"`
			StructuredContent map[string]any          `json:"structuredContent"`
			Content           []struct{ Text string } `json:"content"`
		}
		if err := json.Unmarshal(response.Result, &envelope); err != nil {
			s.t.Fatalf("decode %s envelope: %v", name, err)
		}
		if envelope.IsError {
			s.t.Fatalf("%s returned a tool error: %s", name, response.Result)
		}
		if envelope.StructuredContent != nil {
			return envelope.StructuredContent
		}
		if len(envelope.Content) == 0 {
			s.t.Fatalf("%s returned nothing: %s", name, response.Result)
		}
		out := map[string]any{}
		if err := json.Unmarshal([]byte(envelope.Content[0].Text), &out); err != nil {
			s.t.Fatalf("decode %s result: %v", name, err)
		}
		return out
	}
}

// The ARIA structure is computed in the page precisely so a baseline is not a
// gate only one transport can run: the bridge has no browser-level
// Accessibility domain. brw_baseline is called for real here — over stdio JSON-
// RPC, through the tool's own routing, onto the bridge's websocket protocol —
// so what is pinned is "brw_baseline works on the bridge", not "these five
// calls, in this order, would work if something made them".
func TestBridgeProducesABaselineThatGatesTheSamePage(t *testing.T) {
	label := "Pay invoice"
	patched := false

	b := New("", 5*time.Second, "")
	cleanup := serveBaselineStub(t, b,
		func(expression string) any {
			switch expression {
			case snapshot.AriaTreeExpression:
				return map[string]any{"nodes": []any{
					map[string]any{"role": "main", "children": []any{
						map[string]any{"role": "button", "name": label},
					}},
				}}
			case baseline.EnvironmentExpression:
				return map[string]any{
					"browser_build":      "Chrome/141.0.0.0",
					"viewport_width":     1280,
					"viewport_height":    800,
					"device_pixel_ratio": 1,
					"locale":             "en-GB",
				}
			default:
				// Bridge.Screenshot reads the viewport before clipping.
				return []any{1280, 800}
			}
		},
		func() []byte { return baselineStubJPEG(t, patched) },
	)
	defer cleanup()

	// The encoding is why the tool normalizes at all, so assert the transport
	// really does hand back JPEG rather than taking it on trust.
	if shot, err := b.Screenshot(bridgeTabContext()); err != nil {
		t.Fatalf("screenshot over the bridge: %v", err)
	} else if shot.MIMEType != "image/jpeg" {
		t.Fatalf("the bridge captured %q; this test exists because it captures JPEG", shot.MIMEType)
	}

	store, err := baseline.NewStore(filepath.Join(t.TempDir(), "baselines"))
	if err != nil {
		t.Fatalf("baseline.NewStore: %v", err)
	}
	session := startToolSession(t, b, store)
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	baselineCall := func(action string) map[string]any {
		t.Helper()
		return session.call("brw_baseline", map[string]any{
			"action": action, "recipe_digest": digest, "step_index": 0, "tab_id": "42",
		})
	}

	// A gate that has never seen the page is not a gate.
	missing := baselineCall("check")
	if missing["status"] != baseline.StatusMissing || missing["failed"] != true {
		t.Fatalf("first check over the bridge = %v, want a failing %q", missing, baseline.StatusMissing)
	}

	recorded := baselineCall("update")
	if recorded["status"] != baseline.StatusRecorded {
		t.Fatalf("record over the bridge = %v, want %q", recorded, baseline.StatusRecorded)
	}
	environment, _ := recorded["environment"].(map[string]any)
	if environment == nil || environment["viewport_width"] != float64(1280) {
		t.Fatalf("environment = %v, want the one the bridge measured in the page", recorded["environment"])
	}

	matched := baselineCall("check")
	if matched["status"] != baseline.StatusMatch || matched["failed"] != false {
		t.Fatalf("an unchanged page over the bridge = %v, want a passing %q", matched, baseline.StatusMatch)
	}

	// A structural regression the pixels would not show, over the same transport.
	label = ""
	ariaOnly := baselineCall("check")
	if ariaOnly["status"] != baseline.StatusDiff || ariaOnly["failed"] != true {
		t.Fatalf("an ARIA regression over the bridge = %v, want a failing %q", ariaOnly, baseline.StatusDiff)
	}
	visual, _ := ariaOnly["visual"].(map[string]any)
	if visual == nil || visual["changed"] != false {
		t.Fatalf("visual = %v, want the pixels unchanged", ariaOnly["visual"])
	}

	// And a visual one, to prove the JPEG the bridge captures is actually
	// compared rather than merely decoded.
	label = "Pay invoice"
	patched = true
	visualOnly := baselineCall("check")
	if visualOnly["status"] != baseline.StatusDiff || visualOnly["failed"] != true {
		t.Fatalf("a repaint over the bridge = %v, want a failing %q", visualOnly, baseline.StatusDiff)
	}
	repainted, _ := visualOnly["visual"].(map[string]any)
	if repainted == nil || repainted["changed"] != true {
		t.Fatalf("visual = %v, want the moved pixels reported", visualOnly["visual"])
	}
}
