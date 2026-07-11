package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

func TestManagerClickProvidesUserActivationForPopupControls(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/child" {
			fmt.Fprint(w, `<!doctype html><title>Popup child</title><main>popup opened</main>`)
			return
		}
		fmt.Fprint(w, `<!doctype html><title>Popup opener</title>
<button id="open" aria-haspopup="dialog">Open popup</button>
<script>document.getElementById('open').addEventListener('click',()=>window.open('/child','brwPopup','popup=yes,width=420,height=320'))</script>`)
	}))
	defer site.Close()

	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	opened, err := m.Open(ctx, site.URL)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)
	snap, err := m.Snapshot(tabCtx, snapshot.SnapshotOptions{})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var ref string
	for _, el := range snap.Elements {
		if el.Name == "Open popup" {
			ref = el.Ref
			break
		}
	}
	if ref == "" {
		t.Fatalf("popup control missing from snapshot: %+v", snap.Elements)
	}
	if _, err := m.Click(tabCtx, ref); err != nil {
		t.Fatalf("click popup control: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tabs, listErr := m.ListTabs(ctx)
		if listErr != nil {
			t.Fatalf("list tabs: %v", listErr)
		}
		for _, tab := range tabs {
			if strings.Contains(tab.URL, "/child") {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("trusted click did not open popup within deadline")
}
