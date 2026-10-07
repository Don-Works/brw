package extensionbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/snapshot"
)

func TestRuntimeOriginGuardWithEmptyNavigationPolicy(t *testing.T) {
	t.Setenv("BRW_SCREENSHOT_ALLOW_OUTSIDE_HOME", "1")
	const protected = "http://127.0.0.1:9223/approvals"
	const privateContent = "PRIVATE_OPERATOR_CONTENT"
	blocked := errors.New("operator origin refused")
	for _, tc := range []struct {
		name string
		call func(context.Context, *Bridge) (any, error)
	}{
		{"evaluate", func(ctx context.Context, b *Bridge) (any, error) { return b.Evaluate(ctx, "document.body.innerText") }},
		{"read", func(ctx context.Context, b *Bridge) (any, error) { return b.Read(ctx) }},
		{"read data", func(ctx context.Context, b *Bridge) (any, error) { return b.ReadData(ctx) }},
		{"console", func(ctx context.Context, b *Bridge) (any, error) { return b.ConsoleMessages(ctx) }},
		{"dialog", func(ctx context.Context, b *Bridge) (any, error) { return b.Dialog(ctx, browser.DialogOptions{}) }},
		{"replay", func(ctx context.Context, b *Bridge) (any, error) {
			return b.ReplayRequest(ctx, browser.ReplayRequestParams{URL: "/api"})
		}},
		{"cached snapshot", func(ctx context.Context, b *Bridge) (any, error) { return b.Snapshot(ctx, snapshot.SnapshotOptions{}) }},
		{"live snapshot", func(ctx context.Context, b *Bridge) (any, error) {
			return b.snapshotLive(ctx, snapshot.SnapshotOptions{})
		}},
		{"screenshot", func(ctx context.Context, b *Bridge) (any, error) { return b.Screenshot(ctx) }},
		{"PDF", func(ctx context.Context, b *Bridge) (any, error) { return b.CapturePDF(ctx) }},
		{"PDF stream", func(ctx context.Context, b *Bridge) (any, error) {
			stream, err := b.CapturePDFStream(ctx)
			if stream != nil {
				stream.Close()
			}
			return stream, err
		}},
		{"saved screenshot", func(ctx context.Context, b *Bridge) (any, error) {
			return b.SaveScreenshot(ctx, browser.ScreenshotSaveOptions{SavePath: filepath.Join(t.TempDir(), "capture.png"), Format: "png"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := New("", time.Second, "")
			page := map[string]any{"url": protected, "title": privateContent, "main": privateContent, "elements": []any{}, "metadata": map[string]any{"version": 1}}
			stub := &cdpStub{reply: func(call cdpCall, _ int) (map[string]any, string) {
				switch call.Method {
				case "Runtime.evaluate":
					value := any(page)
					if expression, _ := call.Params["expression"].(string); expression == "location.href" {
						value = protected
					} else if expression == "[Math.round(window.innerWidth), Math.round(window.innerHeight)]" {
						value = []int{100, 100}
					}
					return map[string]any{"result": map[string]any{"value": value}}, ""
				case "Page.printToPDF":
					return map[string]any{"stream": "private-stream", "data": base64.StdEncoding.EncodeToString([]byte(privateContent))}, ""
				case "IO.close":
					return map[string]any{}, ""
				default:
					return map[string]any{"cached": true, "snapshot": page, "data": base64.StdEncoding.EncodeToString([]byte(privateContent)), "width": 1, "height": 1}, ""
				}
			}}
			cleanup := serveCDPStub(t, b, stub)
			defer cleanup()
			ctx := browser.WithFrameReadCheck(browser.WithTabID(context.Background(), "42"), func(raw string) error {
				if raw == protected {
					return blocked
				}
				return nil
			})
			result, err := tc.call(ctx, b)
			if !errors.Is(err, blocked) {
				t.Fatalf("runtime origin guard bypassed: result=%+v err=%v", result, err)
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), privateContent) {
				t.Fatal("refused operator content escaped in the returned result")
			}
			if tc.name == "PDF stream" {
				methods := stub.methods()
				if methods[len(methods)-1] != "IO.close" {
					t.Fatalf("refused PDF handle leaked: %v", methods)
				}
			}
		})
	}
}

func TestBridgeReadReturnedOriginOverridesCurrentURL(t *testing.T) {
	const protected = "http://127.0.0.1:9223/approvals"
	blocked := errors.New("operator origin refused")
	b := New("", time.Second, "")
	stub := &cdpStub{reply: func(call cdpCall, _ int) (map[string]any, string) {
		value := any(map[string]any{"url": protected, "title": "private", "main": "private"})
		if call.Params["expression"] == "location.href" {
			value = "https://public.example.test/"
		}
		return map[string]any{"result": map[string]any{"value": value}}, ""
	}}
	cleanup := serveCDPStub(t, b, stub)
	defer cleanup()
	ctx := browser.WithFrameReadCheck(browser.WithTabID(context.Background(), "42"), func(raw string) error {
		if raw == protected {
			return blocked
		}
		return nil
	})
	result, err := b.Read(ctx)
	if !errors.Is(err, blocked) || !reflect.DeepEqual(result, readability.PageRead{}) {
		t.Fatalf("returned origin bypassed: result=%+v err=%v", result, err)
	}
}

func TestBridgeReplayProtectedRedirectDoesNotReturnBody(t *testing.T) {
	const protected = "http://127.0.0.1:9223/approvals"
	blocked := errors.New("operator origin refused")
	b := New("", time.Second, "")
	stub := &cdpStub{reply: func(call cdpCall, _ int) (map[string]any, string) {
		value := any(map[string]any{"url": protected, "body": "private", "status": 200, "ok": true})
		if call.Params["expression"] == "location.href" {
			value = "https://public.example.test/"
		}
		return map[string]any{"result": map[string]any{"value": value}}, ""
	}}
	cleanup := serveCDPStub(t, b, stub)
	defer cleanup()
	ctx := browser.WithFetchCheck(browser.WithTabID(context.Background(), "42"), func(raw string) error {
		if raw == protected {
			return blocked
		}
		return nil
	})
	result, err := b.ReplayRequest(ctx, browser.ReplayRequestParams{URL: "/redirect"})
	if !errors.Is(err, blocked) || result != (snapshot.ReplayResult{}) {
		t.Fatalf("redirected response bypassed: result=%+v err=%v", result, err)
	}
}

func TestBridgeFrameReadRejectsReturnedProtectedOrUnknownDocument(t *testing.T) {
	const protected = "http://127.0.0.1:9223/approvals"
	b := New("", time.Second, "")
	stub := &cdpStub{reply: func(cdpCall, int) (map[string]any, string) {
		return map[string]any{"frames": []any{
			map[string]any{"origin": "https://public.test", "url": "https://public.test/", "snapshot": map[string]any{"url": protected, "title": "private"}},
			map[string]any{"origin": "https://public.test", "url": "https://public.test/", "snapshot": map[string]any{"title": "unknown private"}},
			map[string]any{"origin": "https://public.test", "url": "https://public.test/", "snapshot": map[string]any{"url": "https://public.test/", "title": "allowed"}},
		}}, ""
	}}
	cleanup := serveCDPStub(t, b, stub)
	defer cleanup()
	ctx := browser.WithFrameReadCheck(browser.WithTabID(context.Background(), "42"), func(raw string) error {
		if raw == protected {
			return errors.New("refused")
		}
		return nil
	})
	frames, err := b.callCrossOriginFrames(ctx, []string{"https://public.test"}, "fixture()")
	if err != nil || len(frames) != 3 || frames[0].Snapshot != nil || frames[1].Snapshot != nil || frames[2].Snapshot == nil || frames[2].Snapshot.Title != "allowed" {
		t.Fatalf("returned frame origin bypassed: %+v %v", frames, err)
	}
}

func TestBridgeProtectedExceptionTextDoesNotEscapeAsAnError(t *testing.T) {
	const protected = "http://127.0.0.1:9223/approvals"
	blocked := errors.New("operator origin refused")
	b := New("", time.Second, "")
	stub := &cdpStub{reply: func(call cdpCall, _ int) (map[string]any, string) {
		if call.Params["expression"] == "location.href" {
			return map[string]any{"result": map[string]any{"value": protected}}, ""
		}
		return map[string]any{"exceptionDetails": map[string]any{
			"exception": map[string]any{"description": "PRIVATE_OPERATOR_EXCEPTION"},
		}}, ""
	}}
	cleanup := serveCDPStub(t, b, stub)
	defer cleanup()
	ctx := browser.WithFrameReadCheck(browser.WithTabID(context.Background(), "42"), func(raw string) error {
		if raw == protected {
			return blocked
		}
		return nil
	})
	_, err := b.Evaluate(ctx, "throw Error(document.body.innerText)")
	if !errors.Is(err, blocked) || strings.Contains(err.Error(), "PRIVATE_OPERATOR_EXCEPTION") {
		t.Fatalf("protected exception leaked: %v", err)
	}
}

func TestBridgeRuntimeOriginRequiresCurrentDocumentURL(t *testing.T) {
	b := New("", time.Second, "")
	stub := &cdpStub{reply: func(call cdpCall, _ int) (map[string]any, string) {
		value := any("private document")
		if call.Params["expression"] == "location.href" {
			value = nil
		}
		return map[string]any{"result": map[string]any{"value": value}}, ""
	}}
	cleanup := serveCDPStub(t, b, stub)
	defer cleanup()
	ctx := browser.WithFrameReadCheck(browser.WithTabID(context.Background(), "42"), func(string) error { return nil })
	result, err := b.Evaluate(ctx, "document.body.innerText")
	if err == nil || !strings.Contains(err.Error(), "destination is unavailable") || result != nil {
		t.Fatalf("unknown current origin returned data: %v %v", result, err)
	}
}
