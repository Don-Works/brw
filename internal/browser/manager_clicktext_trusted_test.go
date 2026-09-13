package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

// brw_click_text used to dispatch only synthetic in-page mouse events. Those
// carry neither event.isTrusted nor transient user activation, so a
// target="_blank" link or a window.open() button did nothing at all while the
// call still reported ok:true — the worst shape of failure, because the agent
// believes the click landed.
func TestClickTextActuatesControlsThatNeedARealGesture(t *testing.T) {
	tests := []struct {
		name string
		body string
		text string
	}{
		{
			name: "anchor with target=_blank",
			body: `<a id="x" href="/child" target="_blank">Open child</a>`,
			text: "Open child",
		},
		{
			name: "window.open from an inline onclick attribute",
			body: `<button onclick="window.open('/child','_blank')">Open child</button>`,
			text: "Open child",
		},
		{
			// The common modern shape: a handler assigned as a property, with no
			// attribute to inspect.
			name: "window.open from an onclick property handler",
			body: `<button id="b">Open child</button>
			       <script>document.getElementById('b').onclick=function(){window.open('/child','_blank');};</script>`,
			text: "Open child",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				if r.URL.Path == "/child" {
					fmt.Fprint(w, `<!doctype html><title>Child window</title><main>child</main>`)
					return
				}
				fmt.Fprintf(w, `<!doctype html><title>Opener</title><body>%s</body>`, tt.body)
			}))
			defer site.Close()

			m := newHeadlessManager(t)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			opened, err := m.Open(ctx, site.URL)
			if err != nil {
				t.Fatalf("open fixture: %v", err)
			}
			tabCtx := WithTabID(ctx, opened.Tab.ID)
			if _, err := m.ClickText(tabCtx, snapshot.ClickTextOptions{Text: tt.text}); err != nil {
				t.Fatalf("click text: %v", err)
			}

			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				tabs, listErr := m.ListTabs(ctx)
				if listErr != nil {
					t.Fatalf("list tabs: %v", listErr)
				}
				for _, tab := range tabs {
					if tab.URL == site.URL+"/child" {
						return
					}
				}
				time.Sleep(150 * time.Millisecond)
			}
			t.Fatal("clicking a control that needs a real input gesture opened no tab; " +
				"click_text reported success but actuated nothing")
		})
	}
}

// An ordinary control must keep the fast single-evaluate in-page path: the whole
// point of deferring only when needed is that the common case stays cheap.
func TestClickTextKeepsTheInPagePathForOrdinaryControls(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><title>Plain</title><body>
			<button id="b" onclick="document.title='clicked'">Press me</button></body>`)
	}))
	defer site.Close()

	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	opened, err := m.Open(ctx, site.URL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)

	result, err := m.ClickText(tabCtx, snapshot.ClickTextOptions{Text: "Press me"})
	if err != nil {
		t.Fatalf("click text: %v", err)
	}
	if !result.OK {
		t.Fatalf("click reported not-ok: %+v", result)
	}
	title, err := m.Evaluate(ctx, `document.title`)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if got, _ := title.(string); got != "clicked" {
		t.Fatalf("title = %q, want %q — the ordinary in-page click did not fire", got, "clicked")
	}
}
