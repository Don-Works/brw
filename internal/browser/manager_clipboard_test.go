package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const clipboardFixture = `<!doctype html><html><head><meta charset="utf-8"><title>clipboard fixture</title></head>
<body><textarea id="sink" rows="3" cols="40"></textarea></body></html>`

func openClipboardFixture(t *testing.T, m *Manager, ctx context.Context) context.Context {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, clipboardFixture)
	}))
	t.Cleanup(srv.Close)
	opened, err := m.Open(ctx, srv.URL)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	return WithTabID(ctx, opened.Tab.ID)
}

// TestClipboardRoundTrip writes to the real system clipboard and reads it back.
// The read is the half that needs the CDP permission grant: without it Chrome
// answers "NotAllowedError: Read permission denied", so deleting
// grantClipboardPermissions fails this test.
func TestClipboardRoundTrip(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	tabCtx := openClipboardFixture(t, m, ctx)

	const payload = "brw clipboard round trip — ünïcode ✓"
	written, err := m.Clipboard(tabCtx, ClipboardOptions{Action: "write", Text: payload})
	if err != nil {
		t.Fatalf("clipboard write: %v", err)
	}
	if !written.OK || written.Action != "write" {
		t.Fatalf("write result = %+v", written)
	}
	if written.Length != len([]rune(payload)) {
		t.Fatalf("write reported length %d, want %d runes", written.Length, len([]rune(payload)))
	}
	if written.Origin == "" {
		t.Fatal("write did not report the origin it was scoped to")
	}

	read, err := m.Clipboard(tabCtx, ClipboardOptions{Action: "read"})
	if err != nil {
		t.Fatalf("clipboard read: %v", err)
	}
	if read.Text != payload {
		t.Fatalf("clipboard read %q, want %q", read.Text, payload)
	}

	// The write must have reached the SYSTEM clipboard, not some brw-local
	// buffer: the page reads it back through its own Clipboard API.
	value, err := m.Evaluate(tabCtx, "navigator.clipboard.readText()")
	if err != nil {
		t.Fatalf("page-side clipboard read: %v", err)
	}
	if text, _ := value.(string); text != payload {
		t.Fatalf("the page read %q from the clipboard, want %q", value, payload)
	}
}

// TestClipboardReadsWhatThePageCopied is the direction an agent needs for a
// "copy link" button: the page writes, brw reads.
func TestClipboardReadsWhatThePageCopied(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	tabCtx := openClipboardFixture(t, m, ctx)

	// Prime the permission through brw, then let the page do the copying.
	if _, err := m.Clipboard(tabCtx, ClipboardOptions{Action: "write", Text: "placeholder"}); err != nil {
		t.Fatalf("prime clipboard: %v", err)
	}
	if _, err := m.Evaluate(tabCtx, `navigator.clipboard.writeText('copied by the page').then(function(){ return true; })`); err != nil {
		t.Fatalf("page-side copy: %v", err)
	}
	read, err := m.Clipboard(tabCtx, ClipboardOptions{Action: "read"})
	if err != nil {
		t.Fatalf("clipboard read: %v", err)
	}
	if read.Text != "copied by the page" {
		t.Fatalf("clipboard read %q, want what the page copied", read.Text)
	}
}

func TestClipboardRejectsUnusableTargets(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := m.Clipboard(ctx, ClipboardOptions{Action: "paste"}); err == nil {
		t.Fatal("an unknown clipboard action should be rejected")
	} else if !strings.Contains(err.Error(), "read or write") {
		t.Fatalf("error %q should name the valid actions", err)
	}
	if _, err := m.Clipboard(ctx, ClipboardOptions{}); err == nil {
		t.Fatal("a missing clipboard action should be rejected")
	}

	// A page with no origin has no clipboard scope, and saying so is better than
	// a DOMException from the page.
	opened, err := m.Open(ctx, "about:blank")
	if err != nil {
		t.Fatalf("open about:blank: %v", err)
	}
	_, err = m.Clipboard(WithTabID(ctx, opened.Tab.ID), ClipboardOptions{Action: "read"})
	if err == nil {
		t.Fatal("clipboard on an origin-less page should be rejected")
	}
	if !strings.Contains(err.Error(), "origin") {
		t.Fatalf("error %q should explain that the page has no origin", err)
	}
}
