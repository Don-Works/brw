package extensionbridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Don-Works/brw/internal/baseline"
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

// The ARIA structure is computed in the page precisely so a baseline is not a
// gate only one transport can run: the bridge has no browser-level
// Accessibility domain. This drives the bridge's real protocol end to end —
// both expressions and a screenshot — and gates on the result, so the claim is
// pinned rather than asserted.
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

	store, err := baseline.NewStore(filepath.Join(t.TempDir(), "baselines"))
	if err != nil {
		t.Fatalf("baseline.NewStore: %v", err)
	}
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	capture := func(t *testing.T) baseline.CheckOptions {
		t.Helper()
		ctx := bridgeTabContext()
		rawTree, err := b.Evaluate(ctx, snapshot.AriaTreeExpression)
		if err != nil {
			t.Fatalf("read the ARIA structure over the bridge: %v", err)
		}
		tree, err := snapshot.ParseAriaTree(rawTree)
		if err != nil {
			t.Fatalf("ParseAriaTree: %v", err)
		}
		rawEnvironment, err := b.Evaluate(ctx, baseline.EnvironmentExpression)
		if err != nil {
			t.Fatalf("read the environment over the bridge: %v", err)
		}
		encoded, err := json.Marshal(rawEnvironment)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var environment baseline.Environment
		if err := json.Unmarshal(encoded, &environment); err != nil {
			t.Fatalf("decode the environment: %v", err)
		}
		environment.OS = runtime.GOOS
		shot, err := b.Screenshot(ctx)
		if err != nil {
			t.Fatalf("screenshot over the bridge: %v", err)
		}
		if shot.MIMEType != "image/jpeg" {
			t.Fatalf("the bridge captured %q; this test exists because it captures JPEG", shot.MIMEType)
		}
		pixels, err := baseline.NormalizePNG(shot.Data)
		if err != nil {
			t.Fatalf("normalize the bridge capture: %v", err)
		}
		return baseline.CheckOptions{
			Key:        baseline.Key{RecipeDigest: digest, StepIndex: 0, Environment: environment.Normalize()},
			Screenshot: pixels,
			Tree:       tree,
		}
	}

	recordOpts := capture(t)
	recordOpts.Update = true
	recorded, err := baseline.Check(store, recordOpts)
	if err != nil {
		t.Fatalf("record a baseline from the bridge: %v", err)
	}
	if recorded.Status != baseline.StatusRecorded {
		t.Fatalf("record = %+v, want %q", recorded, baseline.StatusRecorded)
	}

	matched, err := baseline.Check(store, capture(t))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if matched.Status != baseline.StatusMatch || matched.Failed {
		t.Fatalf("an unchanged page over the bridge = %+v, want a passing %q", matched, baseline.StatusMatch)
	}

	// A structural regression the pixels would not show, over the same transport.
	label = ""
	ariaOnly, err := baseline.Check(store, capture(t))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if ariaOnly.Status != baseline.StatusDiff || !ariaOnly.Failed {
		t.Fatalf("an ARIA regression over the bridge = %+v, want a failing %q", ariaOnly, baseline.StatusDiff)
	}
	if ariaOnly.Visual == nil || ariaOnly.Visual.Changed {
		t.Fatalf("visual = %+v, want the pixels unchanged", ariaOnly.Visual)
	}

	// And a visual one, to prove the JPEG the bridge captures is actually
	// compared rather than merely decoded.
	label = "Pay invoice"
	patched = true
	visualOnly, err := baseline.Check(store, capture(t))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if visualOnly.Status != baseline.StatusDiff || !visualOnly.Failed {
		t.Fatalf("a repaint over the bridge = %+v, want a failing %q", visualOnly, baseline.StatusDiff)
	}
	if visualOnly.Visual == nil || !visualOnly.Visual.Changed {
		t.Fatalf("visual = %+v, want the moved pixels reported", visualOnly.Visual)
	}
}
