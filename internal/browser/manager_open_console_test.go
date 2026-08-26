package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestOpenCapturesLoadTimeConsoleOutput covers the canonical browser-debugging
// ask — "open the page and check the console for errors on load". Open used to
// create the target already navigating to the URL, so everything the page
// logged while booting was emitted before brw attached Runtime, and the first
// console read after an open came back empty.
func TestOpenCapturesLoadTimeConsoleOutput(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body><h1>Boot</h1>
<script>
  console.log("boot-log");
  console.warn("boot-warn");
  console.error("boot-error");
  undefinedFunctionCalledAtLoad();
</script>
</body></html>`))
	}))
	defer site.Close()

	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	opened, err := m.Open(ctx, site.URL)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Tab.URL == "" || strings.HasPrefix(opened.Tab.URL, "about:") {
		t.Fatalf("open landed on %q, want the requested page", opened.Tab.URL)
	}

	tabCtx := WithTabID(ctx, opened.Tab.ID)
	messages, err := m.ConsoleMessages(tabCtx)
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, msg := range messages {
		seen[msg.Level+":"+msg.Text] = true
	}
	for _, want := range []string{"log:boot-log", "warn:boot-warn", "error:boot-error"} {
		if !seen[want] {
			t.Errorf("load-time console message %q not captured; got %+v", want, messages)
		}
	}

	// An uncaught exception thrown while the page boots is the thing an agent is
	// usually hunting, so it must survive the same attach window.
	var sawException bool
	for _, msg := range messages {
		if msg.Level == "error" && strings.Contains(msg.Text, "undefinedFunctionCalledAtLoad") {
			sawException = true
		}
	}
	if !sawException {
		t.Errorf("load-time uncaught exception not captured; got %+v", messages)
	}
}

// Open must still reach the requested page and report readiness, whatever the
// attach-then-navigate sequence does internally.
func TestOpenStillReachesTheRequestedPage(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><title>Landed</title></head><body><h1>Landed</h1></body></html>`))
	}))
	defer site.Close()

	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	opened, err := m.Open(ctx, site.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !opened.Ready {
		t.Errorf("open reported not ready for a trivial page")
	}
	if !strings.HasPrefix(opened.Tab.URL, site.URL) {
		t.Fatalf("tab URL = %q, want %q", opened.Tab.URL, site.URL)
	}

	read, err := m.Read(WithTabID(ctx, opened.Tab.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read.Main, "Landed") {
		t.Fatalf("page content after open = %q", read.Main)
	}
}

// about:blank opens must not pay for a navigation they do not need.
func TestOpenBlankTabStillWorks(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	opened, err := m.Open(ctx, "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	if opened.Tab.ID == "" {
		t.Fatal("about:blank open returned no tab")
	}
}
