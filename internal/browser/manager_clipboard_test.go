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

// TestClipboardRoundTrip writes through Chrome's Clipboard API and reads it
// back. The read is the half that needs the CDP permission grant: without it
// Chrome answers "NotAllowedError: Read permission denied", so deleting
// grantClipboardPermissions fails this test.
//
// What it does NOT prove is that the text reached the OS pasteboard. Headless
// Chrome keeps an in-process clipboard, so a native paste in another application
// would not find this payload; headed Chrome writes through. The clipboard here
// is Chrome's, which is the boundary brw controls either way.
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

	// The write reached the browser's clipboard, not a brw-local buffer: the
	// page reads it back through its own Clipboard API, which is the path a
	// page's own paste handler uses.
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
	} else if !strings.Contains(err.Error(), "read, write or revoke") {
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

// pageCanReadClipboard reports whether the page's own script may call
// navigator.clipboard.readText(). That is the capability the permission grant
// hands to the ORIGIN, so it is what has to be measured — the brw-side read
// would grant itself the permission first and always succeed.
func pageCanReadClipboard(t *testing.T, m *Manager, ctx context.Context) (bool, string) {
	t.Helper()
	value, err := m.Evaluate(ctx, "navigator.clipboard.readText().then(function(t){return 'ok:'+t;},function(e){return 'err:'+e.name;})")
	if err != nil {
		t.Fatalf("page-side clipboard probe: %v", err)
	}
	answer, _ := value.(string)
	return strings.HasPrefix(answer, "ok:"), answer
}

// TestClipboardGrantsOnlyWhatTheActionNeeds is the containment test.
//
// Chrome maps clipboard-write WITH allow_without_sanitization — and
// clipboard-read either way — to the single CLIPBOARD_READ_WRITE permission, so
// sending both descriptors for a write handed the origin unsanitized clipboard
// READ: from then on any script on that page could call readText() at will, for
// the life of the browser, with no further involvement from brw. A write must
// leave the origin unable to read.
func TestClipboardGrantsOnlyWhatTheActionNeeds(t *testing.T) {
	tests := []struct {
		name        string
		action      string
		text        string
		wantCanRead bool
	}{
		{name: "write grants no read", action: "write", text: "written by brw", wantCanRead: false},
		{name: "read grants read", action: "read", wantCanRead: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A fresh browser per case: a permission override lives in the
			// browser context, so sharing one would leak the read grant.
			m := newHeadlessManager(t)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			tabCtx := openClipboardFixture(t, m, ctx)

			if _, err := m.Clipboard(tabCtx, ClipboardOptions{Action: tt.action, Text: tt.text}); err != nil {
				t.Fatalf("clipboard %s: %v", tt.action, err)
			}
			canRead, answer := pageCanReadClipboard(t, m, tabCtx)
			if canRead != tt.wantCanRead {
				t.Fatalf("after clipboard %s the page-side readText answered %q; page can read = %v, want %v",
					tt.action, answer, canRead, tt.wantCanRead)
			}
		})
	}
}

// TestClipboardRevokeDropsTheGrant covers the operator's undo. The grant
// deliberately outlives the call (a read-modify-write flow would break if it did
// not), so there has to be a way to take it back without restarting the browser.
func TestClipboardRevokeDropsTheGrant(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	tabCtx := openClipboardFixture(t, m, ctx)

	if _, err := m.Clipboard(tabCtx, ClipboardOptions{Action: "read"}); err != nil {
		t.Fatalf("clipboard read: %v", err)
	}
	if canRead, answer := pageCanReadClipboard(t, m, tabCtx); !canRead {
		t.Fatalf("after a read the page should hold the grant; readText answered %q", answer)
	}

	revoked, err := m.Clipboard(tabCtx, ClipboardOptions{Action: "revoke"})
	if err != nil {
		t.Fatalf("clipboard revoke: %v", err)
	}
	if !revoked.OK || revoked.Action != "revoke" {
		t.Fatalf("revoke result = %+v", revoked)
	}
	if canRead, answer := pageCanReadClipboard(t, m, tabCtx); canRead {
		t.Fatalf("the origin could still read the clipboard after revoke; readText answered %q", answer)
	}
}
