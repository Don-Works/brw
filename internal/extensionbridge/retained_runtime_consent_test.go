package extensionbridge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

func TestBridgeDownloadRefusalPreservesRecipeCursor(t *testing.T) {
	b := New("", time.Second, "")
	const source = "https://protected.example.test/file"
	stub := serveRPCStub(t, b, func(kind string, _ int) (map[string]any, bool, string) {
		if kind == "get_downloads" {
			return map[string]any{"supported": true, "downloads": []browser.DownloadEntry{{GUID: "retained", URL: source, TabID: "42", State: "completed"}}}, true, ""
		}
		return map[string]any{"result": map[string]any{"value": "https://public.example.test/"}}, true, ""
	})
	defer stub.stop()
	denied := errors.New("download source refused")
	allowed := false
	ctx := browser.WithFrameReadCheck(browser.WithAllowedOrigins(browser.WithTabID(context.Background(), "42"), []string{"https://public.example.test"}), func(url string) error {
		if url == source && !allowed {
			return denied
		}
		return nil
	})
	if result, err := b.Downloads(ctx); !errors.Is(err, denied) || len(result.Downloads) != 0 {
		t.Fatalf("refused retained download exposed: result=%+v err=%v", result, err)
	}
	allowed = true
	if result, err := b.Downloads(ctx); err != nil || len(result.Downloads) != 1 || result.Downloads[0].GUID != "retained" {
		t.Fatalf("allowed retry lost retained download: result=%+v err=%v", result, err)
	}
	if result, err := b.Downloads(ctx); err != nil || len(result.Downloads) != 0 {
		t.Fatalf("processed cursor did not advance: result=%+v err=%v", result, err)
	}
}

func TestBridgeDownloadWaitWithholdsProtectedResult(t *testing.T) {
	const protected = "https://protected.example.test/"
	denied := errors.New("runtime read refused")
	for _, tc := range []struct{ name, state, source, current string }{
		{"cancelled source", "canceled", protected, "https://public.example.test/"},
		{"current page rollover", "completed", "https://public.example.test/file", protected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := New("", time.Second, "")
			stub := serveRPCStub(t, b, func(kind string, _ int) (map[string]any, bool, string) {
				if kind == "get_downloads" {
					return map[string]any{"supported": true, "downloads": []map[string]any{{"guid": "retained", "url": tc.source, "suggested_filename": "PRIVATE_FILENAME", "state": tc.state, "changed_at_ms": time.Now().UnixMilli()}}}, true, ""
				}
				return map[string]any{"result": map[string]any{"value": tc.current}}, true, ""
			})
			defer stub.stop()
			ctx := browser.WithFrameReadCheck(browser.WithTabID(context.Background(), "42"), func(url string) error {
				if url == protected {
					return denied
				}
				return nil
			})
			result, err := b.WaitForOutcome(ctx, "download", time.Second)
			if !errors.Is(err, denied) || result.OK || strings.Contains(err.Error(), "PRIVATE_FILENAME") {
				t.Fatalf("protected wait exposed: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestBridgeRuntimeRPCErrorWithholdsProtectedText(t *testing.T) {
	b := New("", time.Second, "")
	const protected = "https://protected.example.test/"
	denied := errors.New("runtime read refused")
	stub := &cdpStub{reply: func(call cdpCall, _ int) (map[string]any, string) {
		if call.Params["expression"] == "location.href" {
			return map[string]any{"result": map[string]any{"value": protected}}, ""
		}
		return nil, "PRIVATE_CALLBACK_ERROR"
	}}
	defer serveCDPStub(t, b, stub)()
	ctx := browser.WithFrameReadCheck(browser.WithTabID(context.Background(), "42"), func(string) error { return denied })
	if _, err := b.Evaluate(ctx, "document.body.innerText"); !errors.Is(err, denied) || strings.Contains(err.Error(), "PRIVATE_CALLBACK_ERROR") {
		t.Fatalf("protected RPC error exposed: %v", err)
	}
}

func TestBridgeFrameEnrichmentRechecksTopDocument(t *testing.T) {
	b := New("", time.Second, "")
	const protected = "https://protected.example.test/"
	denied := errors.New("runtime read refused")
	rolled := false
	stub := &cdpStub{reply: func(call cdpCall, _ int) (map[string]any, string) {
		if call.Method == "" {
			rolled = true
			return map[string]any{"frames": []any{}}, ""
		}
		value := any(map[string]any{"url": "https://public.example.test/", "title": "public", "elements": []any{}, "metadata": map[string]any{"version": 1}})
		if call.Params["expression"] == "location.href" {
			value = "https://public.example.test/"
			if rolled {
				value = protected
			}
		}
		return map[string]any{"result": map[string]any{"value": value}}, ""
	}}
	defer serveCDPStub(t, b, stub)()
	ctx := browser.WithFrameReadCheck(browser.WithTabID(context.Background(), "42"), func(url string) error {
		if url == protected {
			return denied
		}
		return nil
	})
	if result, err := b.Snapshot(ctx, snapshot.SnapshotOptions{IncludeFrames: true}); !errors.Is(err, denied) || result.URL != "" {
		t.Fatalf("frame enrichment escaped document guard: result=%+v err=%v", result, err)
	}
}
