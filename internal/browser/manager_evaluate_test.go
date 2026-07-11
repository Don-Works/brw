package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestManagerEvaluateSupportsTopLevelAwait(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><title>await probe</title><main>ready</main>`))
	}))
	defer site.Close()

	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opened, err := m.Open(ctx, site.URL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	value, err := m.Evaluate(WithTabID(ctx, opened.Tab.ID), `await Promise.resolve({ok:true, value:42})`)
	if err != nil {
		t.Fatalf("top-level await: %v", err)
	}
	result, ok := value.(map[string]any)
	if !ok || result["ok"] != true || result["value"] != float64(42) {
		t.Fatalf("evaluate result = %#v", value)
	}
	parenthesized, err := m.Evaluate(WithTabID(ctx, opened.Tab.ID), `await (async()=>({kind:"parenthesized", value:9}))()`)
	if err != nil {
		t.Fatalf("parenthesized top-level await: %v", err)
	}
	parenthesizedResult, ok := parenthesized.(map[string]any)
	if !ok || parenthesizedResult["kind"] != "parenthesized" || parenthesizedResult["value"] != float64(9) {
		t.Fatalf("parenthesized await result = %#v", parenthesized)
	}
	promiseValue, err := m.Evaluate(WithTabID(ctx, opened.Tab.ID), `(async()=>({kind:"promise", value:7}))()`)
	if err != nil {
		t.Fatalf("returned promise: %v", err)
	}
	promiseResult, ok := promiseValue.(map[string]any)
	if !ok || promiseResult["kind"] != "promise" || promiseResult["value"] != float64(7) {
		t.Fatalf("returned promise result = %#v", promiseValue)
	}
}
