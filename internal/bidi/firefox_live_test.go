package bidi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

// These tests are the prototype the BiDi decision rests on. Each one answers
// one of the four questions in docs/bidi-prototype.md against a real Firefox,
// so the document's claims are measurements rather than readings of the spec.
//
// They are measurement code and run only when asked: BRW_BIDI_LIVE=1. Nothing
// in brw's tool surface routes through this package and the recorded decision
// is defer, so `go test ./...` launching five Firefoxes for it would be the
// default suite paying for work that was not adopted. Re-run the measurements
// with `BRW_BIDI_LIVE=1 go test ./internal/bidi/ -count=1 -v`, which is what
// docs/bidi-prototype.md tells a later attempt to do.

const fixtureHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>brw bidi fixture</title>
<style>
  #overlay{position:fixed;left:0;top:0;width:100%;height:200px;background:#123;z-index:10}
  #hidden{display:none}
  .ghost{opacity:0}
  #ghost-covered-wrap{position:fixed;top:60px;left:10px;z-index:1}
</style></head>
<body>
<div id="overlay"></div>
<div style="height:240px"></div>
<button id="ok">Ok</button>
<button id="hidden">Hidden</button>
<button id="disabled" disabled>Disabled</button>
<span aria-hidden="true" class="ghost"><button id="ghost">Ghost</button></span>
<div id="ghost-covered-wrap"><span aria-hidden="true" class="ghost"><button id="ghost-covered">Ghost covered</button></span></div>
<input id="field" value="">
<button id="alerter">Alert</button>
<a id="dl" href="/download.txt" download="bidi-download.txt">Download</a>
<script>
console.log('brw-bidi-console-marker');
fetch('/api/ping').then(function(r){return r.text();}).then(function(t){document.title='pinged:'+t;});
</script>
</body></html>`

func fixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(fixtureHTML))
	})
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("pong"))
	})
	mux.HandleFunc("/download.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="bidi-download.txt"`)
		_, _ = w.Write([]byte("bidi-download-body"))
	})
	mux.HandleFunc("/other", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><title>other</title><p id="other">other document</p>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type session struct {
	conn      *Conn
	contextID string
	downloads string
}

// newSession launches Firefox, opens a BiDi session and resolves the top-level
// browsing context. downloadDir is written into the profile's prefs because
// Firefox 155 has no runtime command for it; see TestBiDiDownloadsAndDialogs.
// requireLiveFirefox skips unless the measurements were explicitly asked for
// and a Firefox is there to measure.
func requireLiveFirefox(t *testing.T) {
	t.Helper()
	if os.Getenv(liveEnv) != "1" {
		t.Skipf("set %s=1 to re-run the BiDi measurements against a real Firefox (docs/bidi-prototype.md)", liveEnv)
	}
	if _, err := FindFirefox(""); err != nil {
		t.Skipf("firefox not available: %v", err)
	}
}

// liveEnv is named once so the skip message and the documentation cannot drift.
const liveEnv = "BRW_BIDI_LIVE"

func newSession(ctx context.Context, t *testing.T) *session {
	t.Helper()
	requireLiveFirefox(t)
	downloads := filepath.Join(t.TempDir(), "downloads")
	if err := os.MkdirAll(downloads, 0o700); err != nil {
		t.Fatalf("download dir: %v", err)
	}
	ff, err := LaunchFirefox(ctx, FirefoxConfig{
		ProfileDir: filepath.Join(t.TempDir(), "profile"),
		Headless:   true,
		Prefs: map[string]any{
			"browser.download.folderList":                           2,
			"browser.download.dir":                                  downloads,
			"browser.download.useDownloadDir":                       true,
			"browser.download.always_ask_before_handling_new_types": false,
			"browser.download.alwaysOpenPanel":                      false,
		},
	})
	if err != nil {
		t.Skipf("firefox did not start: %v", err)
	}
	t.Cleanup(func() { _ = ff.Close() })

	conn, err := Dial(ctx, ff.Endpoint())
	if err != nil {
		t.Fatalf("dial %s: %v", ff.Endpoint(), err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// unhandledPromptBehavior defaults to "dismiss", which answers every dialog
	// before the client sees it — browsingContext.userPromptOpened still fires,
	// but handleUserPrompt then reports "no such alert". A backend that has to
	// route dialogs to the caller must take this capability at session.new; it
	// cannot be changed afterwards.
	if err := conn.Command(ctx, "session.new", map[string]any{
		"capabilities": map[string]any{
			"alwaysMatch": map[string]any{
				"unhandledPromptBehavior": map[string]any{"default": "ignore"},
			},
		},
	}, nil); err != nil {
		t.Fatalf("session.new: %v", err)
	}
	var tree struct {
		Contexts []struct {
			Context string `json:"context"`
		} `json:"contexts"`
	}
	if err := conn.Command(ctx, "browsingContext.getTree", map[string]any{}, &tree); err != nil {
		t.Fatalf("browsingContext.getTree: %v", err)
	}
	if len(tree.Contexts) == 0 {
		t.Fatal("firefox reported no browsing context")
	}
	return &session{conn: conn, contextID: tree.Contexts[0].Context, downloads: downloads}
}

func (s *session) navigate(ctx context.Context, t *testing.T, url string) {
	t.Helper()
	if err := s.conn.Command(ctx, "browsingContext.navigate", map[string]any{
		"context": s.contextID,
		"url":     url,
		"wait":    "complete",
	}, nil); err != nil {
		t.Fatalf("navigate %s: %v", url, err)
	}
}

// callResult is the part of script.callFunction's answer these tests read.
// Realm is what makes the same-document question answerable: it names the
// realm the call actually ran in, and a realm does not outlive its document.
type callResult struct {
	Type             string          `json:"type"`
	Realm            string          `json:"realm"`
	Result           json.RawMessage `json:"result"`
	ExceptionDetails json.RawMessage `json:"exceptionDetails"`
}

// call runs fn in the page's own realm and returns the deserialized result.
// ownership:"root" is not used: these scripts return plain JSON-able values,
// which is exactly what brw's CDP path relies on too.
func (s *session) call(ctx context.Context, fn string, args []any, target map[string]any) (callResult, error) {
	if target == nil {
		target = map[string]any{"context": s.contextID}
	}
	if args == nil {
		args = []any{}
	}
	var out callResult
	err := s.conn.Command(ctx, "script.callFunction", map[string]any{
		"functionDeclaration": fn,
		"target":              target,
		"awaitPromise":        true,
		"arguments":           args,
		"resultOwnership":     "none",
		"serializationOptions": map[string]any{
			"maxObjectDepth": 20,
			"maxDomDepth":    0,
		},
	}, &out)
	return out, err
}

// mustCall fails the test on a protocol error or a page-side exception.
func (s *session) mustCall(ctx context.Context, t *testing.T, what, fn string, args ...any) callResult {
	t.Helper()
	got, err := s.call(ctx, fn, args, nil)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if got.Type != "success" {
		t.Fatalf("%s: page threw: %s", what, string(got.ExceptionDetails))
	}
	return got
}

func num(v float64) map[string]any  { return map[string]any{"type": "number", "value": v} }
func str(v string) map[string]any   { return map[string]any{"type": "string", "value": v} }
func boolean(v bool) map[string]any { return map[string]any{"type": "boolean", "value": v} }

// plain converts a BiDi RemoteValue tree back into ordinary Go values. BiDi
// serializes an object as [[key,value],...] pairs rather than as JSON, so every
// assertion on a script result has to go through this.
func plain(raw json.RawMessage) (any, error) {
	var v struct {
		Type  string          `json:"type"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	switch v.Type {
	case "undefined", "null":
		return nil, nil
	case "string", "boolean", "number":
		var out any
		if err := json.Unmarshal(v.Value, &out); err != nil {
			return nil, err
		}
		return out, nil
	case "array", "set":
		var items []json.RawMessage
		if err := json.Unmarshal(v.Value, &items); err != nil {
			return nil, err
		}
		out := make([]any, 0, len(items))
		for _, item := range items {
			decoded, err := plain(item)
			if err != nil {
				return nil, err
			}
			out = append(out, decoded)
		}
		return out, nil
	case "object", "map":
		var pairs [][2]json.RawMessage
		if err := json.Unmarshal(v.Value, &pairs); err != nil {
			return nil, err
		}
		out := map[string]any{}
		for _, pair := range pairs {
			var key string
			if err := json.Unmarshal(pair[0], &key); err != nil {
				decoded, kerr := plain(pair[0])
				if kerr != nil {
					return nil, kerr
				}
				key = fmt.Sprint(decoded)
			}
			value, err := plain(pair[1])
			if err != nil {
				return nil, err
			}
			out[key] = value
		}
		return out, nil
	default:
		return map[string]any{"__remote_type": v.Type}, nil
	}
}

func plainMap(t *testing.T, what string, raw json.RawMessage) map[string]any {
	t.Helper()
	decoded, err := plain(raw)
	if err != nil {
		t.Fatalf("%s: decode result: %v (raw %s)", what, err, string(raw))
	}
	out, ok := decoded.(map[string]any)
	if !ok {
		t.Fatalf("%s: result is %T, want an object (raw %s)", what, decoded, string(raw))
	}
	return out
}

// Question 1: does BiDi expose the events brw's settle machinery resolves waits
// from? The table is the five CDP-event equivalents named in the brief plus a
// control. Each real event must BOTH subscribe and actually arrive — a
// subscription that succeeds proves nothing on its own unless the browser
// refuses a name it does not implement, which the control establishes.
func TestBiDiSettleEventsArrive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	s := newSession(ctx, t)
	srv := fixtureServer(t)

	events := []struct {
		name  string
		match func(json.RawMessage) bool
	}{
		{name: "browsingContext.navigationStarted"},
		{name: "browsingContext.load"},
		{
			name: "log.entryAdded",
			match: func(params json.RawMessage) bool {
				return strings.Contains(string(params), "brw-bidi-console-marker")
			},
		},
		{
			name: "network.responseCompleted",
			match: func(params json.RawMessage) bool {
				return strings.Contains(string(params), "/api/ping")
			},
		},
		{name: "browsingContext.userPromptOpened"},
	}

	// The control: an event name BiDi does not define. If this subscribed
	// cleanly, a successful subscribe would carry no information and every
	// other row of this table would be vacuous.
	if err := s.conn.Subscribe(ctx, "brwNoSuch.event"); err == nil {
		t.Fatal("subscribing to an undefined event succeeded; a successful session.subscribe is then not evidence that an event exists")
	}

	for _, ev := range events {
		if err := s.conn.Subscribe(ctx, ev.name); err != nil {
			t.Fatalf("subscribe %s: %v", ev.name, err)
		}
	}

	s.navigate(ctx, t, srv.URL+"/")
	// The prompt is opened from a timer so the command returns before alert()
	// blocks the page's event loop.
	s.mustCall(ctx, t, "open prompt", `() => { setTimeout(() => window.alert('bidi-prompt'), 0); return true; }`)

	for _, ev := range events {
		waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
		got, err := s.conn.Await(waitCtx, ev.name, ev.match)
		waitCancel()
		if err != nil {
			t.Errorf("%s never arrived: %v", ev.name, err)
			continue
		}
		t.Logf("%s -> %s", ev.name, truncateForLog(string(got.Params)))
	}

	// Leave the prompt handled so the browser can be closed cleanly.
	if err := s.conn.Command(ctx, "browsingContext.handleUserPrompt", map[string]any{
		"context": s.contextID,
		"accept":  true,
	}, nil); err != nil {
		t.Errorf("handleUserPrompt: %v", err)
	}
}

// Question 2, first half: brw's actionability script is a page script, so the
// question is whether script.callFunction reaches the same verdicts. The table
// covers every branch the script can return — the AX-visible fast path, the
// hit-test fallback, disabled, and both ways of failing visibility — so a
// backend that silently lost the hit test could not pass it.
func TestBiDiReproducesActionabilityVerdicts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	s := newSession(ctx, t)
	srv := fixtureServer(t)
	s.navigate(ctx, t, srv.URL+"/")

	refs := s.installRefs(ctx, t)

	for _, tc := range []struct {
		id       string
		wantOK   bool
		wantMode string
		wantWhy  string
	}{
		{id: "ok", wantOK: true, wantMode: "ax_visible"},
		// opacity:0 under aria-hidden fails every heuristic in visible(), so a
		// pass here can only have come from the elementFromPoint hit test.
		{id: "ghost", wantOK: true, wantMode: "hit_test"},
		// Same element, with an overlay over the pixel. The only difference
		// between this row and the one above is what elementFromPoint returns.
		{id: "ghost-covered", wantOK: false, wantWhy: "not_visible"},
		{id: "hidden", wantOK: false, wantWhy: "not_visible"},
		{id: "disabled", wantOK: false, wantWhy: "disabled"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			ref, ok := refs[tc.id]
			if !ok {
				t.Fatalf("fixture element #%s got no ref from the snapshot script", tc.id)
			}
			got := s.mustCall(ctx, t, "WaitForActionable "+tc.id,
				snapshot.WaitForActionableScript, str(ref), num(1200))
			result := plainMap(t, "WaitForActionable "+tc.id, got.Result)
			if result["ok"] != tc.wantOK {
				t.Fatalf("#%s ok = %v, want %v (full result %v)", tc.id, result["ok"], tc.wantOK, result)
			}
			if tc.wantMode != "" && result["mode"] != tc.wantMode {
				t.Fatalf("#%s mode = %v, want %q (full result %v)", tc.id, result["mode"], tc.wantMode, result)
			}
			if tc.wantWhy != "" && result["reason"] != tc.wantWhy {
				t.Fatalf("#%s reason = %v, want %q (full result %v)", tc.id, result["reason"], tc.wantWhy, result)
			}
		})
	}

	// Editable: the fill script drives the real input events a framework
	// listens for, so reading the value back proves the script ran against the
	// live document and not a detached copy.
	filled := s.mustCall(ctx, t, "FillElement", snapshot.FillElementScript,
		str(refs["field"]), str("bidi typed this"), boolean(true))
	if result := plainMap(t, "FillElement", filled.Result); result["ok"] != true {
		t.Fatalf("FillElementScript returned %v", result)
	}
	value := s.mustCall(ctx, t, "read field", `() => document.getElementById('field').value`)
	if got, _ := plain(value.Result); got != "bidi typed this" {
		t.Fatalf("field value = %v, want the filled text", got)
	}
}

// Question 2, second half: brw's refs are page-side state on window.__brw, so
// they survive only as long as the document does. BiDi has to make both halves
// of that observable — the ref resolving in a later call against the same
// document, and the pinned realm going away when the document does.
func TestBiDiRefsAndSameDocumentGuarantee(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	s := newSession(ctx, t)
	srv := fixtureServer(t)
	s.navigate(ctx, t, srv.URL+"/")

	refs := s.installRefs(ctx, t)
	ref := refs["ok"]
	if ref == "" {
		t.Fatal("snapshot produced no ref for #ok")
	}

	// A separate BiDi command, minutes of wall clock later in principle: the
	// ref has to resolve to the same element.
	resolved := s.mustCall(ctx, t, "ResolveBox", snapshot.ResolveBoxScript, str(ref))
	box := plainMap(t, "ResolveBox", resolved.Result)
	if box["ok"] != true {
		t.Fatalf("ResolveBoxScript did not resolve the ref in a later call: %v", box)
	}
	live := s.mustCall(ctx, t, "live box", `() => { const r = document.getElementById('ok').getBoundingClientRect(); return {x:r.x, y:r.y, w:r.width, h:r.height}; }`)
	liveBox := plainMap(t, "live box", live.Result)
	if box["x"] != liveBox["x"] || box["y"] != liveBox["y"] {
		t.Fatalf("ref resolved to a different box than #ok: ref %v vs element %v", box, liveBox)
	}

	// The realm the call ran in is what pins the document.
	realm := resolved.Realm
	if realm == "" {
		t.Fatal("script.callFunction reported no realm; there is then nothing to pin a ref to")
	}

	s.navigate(ctx, t, srv.URL+"/other")

	// Pinned to the old realm, the call must fail rather than run against the
	// new document. This is the guarantee brw needs: a ref taken before a
	// navigation must never silently act on the page that replaced it.
	_, err := s.call(ctx, `() => 1`, nil, map[string]any{"realm": realm})
	if err == nil {
		t.Fatal("a call pinned to the previous document's realm succeeded after navigation; the same-document guarantee is not expressible")
	}
	t.Logf("pinned stale realm -> %v", err)

	// And unpinned, against the new document, the old ref must be gone rather
	// than resolving to something else.
	after := s.mustCall(ctx, t, "ResolveBox after navigation", snapshot.ResolveBoxScript, str(ref))
	if box := plainMap(t, "ResolveBox after navigation", after.Result); box["ok"] == true {
		t.Fatalf("a ref from the previous document still resolved after navigation: %v", box)
	}
}

// installRefs runs brw's own snapshot script and returns id -> ref for the
// fixture's elements. It is the same script the CDP path installs, unmodified.
//
// The mapping is read back off the DOM rather than out of the snapshot result:
// brw resolves a ref through the data-brw-ref attribute the script stamps, so
// reading the attribute is what proves the script's side effects landed in the
// document BiDi is pointing at.
func (s *session) installRefs(ctx context.Context, t *testing.T) map[string]string {
	t.Helper()
	got := s.mustCall(ctx, t, "SnapshotFunctionScript", snapshot.SnapshotFunctionScript,
		map[string]any{"type": "object", "value": [][2]any{
			{"include_hidden", map[string]any{"type": "boolean", "value": true}},
		}})
	decoded := plainMap(t, "SnapshotFunctionScript", got.Result)
	elements, _ := decoded["elements"].([]any)
	if len(elements) == 0 {
		t.Fatalf("snapshot returned no elements: %v", decoded)
	}
	stamped := s.mustCall(ctx, t, "read stamped refs",
		`() => { const out = []; for (const el of document.querySelectorAll('[data-brw-ref]')) { out.push(el.id + '=' + el.getAttribute('data-brw-ref')); } return out.join(','); }`)
	raw, _ := plain(stamped.Result)
	joined, _ := raw.(string)
	out := map[string]string{}
	for _, pair := range strings.Split(joined, ",") {
		id, ref, ok := strings.Cut(pair, "=")
		if !ok || id == "" || ref == "" {
			continue
		}
		out[id] = ref
	}
	if len(out) == 0 {
		t.Fatalf("the snapshot script stamped no data-brw-ref attributes; refs cannot be resolved (snapshot said %d elements)", len(elements))
	}
	return out
}

// Question 3: are downloads and dialogs addressable? Dialogs are, by command.
// Downloads report their lifecycle but their destination is a launch-time
// profile preference, not a runtime command — which is the difference between
// observing a download and capturing it deterministically.
func TestBiDiDownloadsAndDialogs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	s := newSession(ctx, t)
	srv := fixtureServer(t)

	for _, ev := range []string{"browsingContext.userPromptOpened", "browsingContext.downloadWillBegin", "browsingContext.downloadEnd"} {
		if err := s.conn.Subscribe(ctx, ev); err != nil {
			t.Fatalf("subscribe %s: %v", ev, err)
		}
	}
	s.navigate(ctx, t, srv.URL+"/")

	// Dialogs: open, observe, answer, and see the answer reach the page.
	s.mustCall(ctx, t, "open confirm", `() => { window.__brwConfirm = null; setTimeout(() => { window.__brwConfirm = window.confirm('bidi-confirm'); }, 0); return true; }`)
	prompt, err := s.conn.Await(ctx, "browsingContext.userPromptOpened", func(params json.RawMessage) bool {
		return strings.Contains(string(params), "bidi-confirm")
	})
	if err != nil {
		t.Fatalf("userPromptOpened never arrived: %v", err)
	}
	t.Logf("userPromptOpened -> %s", truncateForLog(string(prompt.Params)))
	if err := s.conn.Command(ctx, "browsingContext.handleUserPrompt", map[string]any{
		"context": s.contextID,
		"accept":  true,
	}, nil); err != nil {
		t.Fatalf("handleUserPrompt: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var answered any
	for time.Now().Before(deadline) {
		got := s.mustCall(ctx, t, "read confirm answer", `() => window.__brwConfirm`)
		answered, _ = plain(got.Result)
		if answered != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if answered != true {
		t.Fatalf("confirm() returned %v after handleUserPrompt accept=true; the dialog answer never reached the page", answered)
	}

	// Downloads: there is no runtime command for the destination. Establish
	// that by asking for one, so the claim in docs/bidi-prototype.md is a
	// measurement and stays true only as long as Firefox agrees.
	err = s.conn.Command(ctx, "browsingContext.setDownloadBehavior", map[string]any{
		"context":          s.contextID,
		"downloadBehavior": map[string]any{"type": "allowed", "destinationFolder": s.downloads},
	}, nil)
	if err == nil {
		t.Fatal("browsingContext.setDownloadBehavior succeeded; docs/bidi-prototype.md says it does not exist and must be corrected")
	}
	if !IsUnknownCommand(err) {
		t.Fatalf("setDownloadBehavior failed for a reason other than being unimplemented: %v", err)
	}
	t.Logf("setDownloadBehavior -> %v", err)

	s.mustCall(ctx, t, "start download", `() => { setTimeout(() => document.getElementById('dl').click(), 0); return true; }`)
	began, err := s.conn.Await(ctx, "browsingContext.downloadWillBegin", nil)
	if err != nil {
		t.Fatalf("downloadWillBegin never arrived: %v", err)
	}
	t.Logf("downloadWillBegin -> %s", truncateForLog(string(began.Params)))
	ended, err := s.conn.Await(ctx, "browsingContext.downloadEnd", nil)
	if err != nil {
		t.Fatalf("downloadEnd never arrived: %v", err)
	}
	t.Logf("downloadEnd -> %s", truncateForLog(string(ended.Params)))

	// The bytes must land where the launch-time preference put them, which is
	// what makes the destination a profile property rather than a session one.
	body, err := os.ReadFile(filepath.Join(s.downloads, "bidi-download.txt"))
	if err != nil {
		entries, _ := os.ReadDir(s.downloads)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("download did not land in the preference-configured folder: %v (folder holds %v)", err, names)
	}
	if string(body) != "bidi-download-body" {
		t.Fatalf("downloaded body = %q", string(body))
	}
}

// Question 4: can artifacts be captured within the same bounds? Both commands
// exist; the assertion is on the bytes, because a command that answers with an
// empty string is not a capture.
func TestBiDiCapturesArtifacts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	s := newSession(ctx, t)
	srv := fixtureServer(t)
	s.navigate(ctx, t, srv.URL+"/")

	for _, tc := range []struct {
		name   string
		method string
		params map[string]any
		magic  string
	}{
		{
			name:   "screenshot",
			method: "browsingContext.captureScreenshot",
			params: map[string]any{"context": s.contextID},
			magic:  "\x89PNG",
		},
		{
			name:   "pdf",
			method: "browsingContext.print",
			params: map[string]any{"context": s.contextID},
			magic:  "%PDF",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out struct {
				Data string `json:"data"`
			}
			if err := s.conn.Command(ctx, tc.method, tc.params, &out); err != nil {
				t.Fatalf("%s: %v", tc.method, err)
			}
			raw, err := base64.StdEncoding.DecodeString(out.Data)
			if err != nil {
				t.Fatalf("%s: result is not base64: %v", tc.method, err)
			}
			if !strings.HasPrefix(string(raw), tc.magic) {
				t.Fatalf("%s: %d bytes that do not start with %q", tc.method, len(raw), tc.magic)
			}
			t.Logf("%s -> %d bytes", tc.method, len(raw))
		})
	}

	// Element-scoped capture is the bound brw's brw_screenshot{ref} needs. A
	// clip by box is what the CDP path uses, so the same thing has to be
	// expressible here.
	box := s.mustCall(ctx, t, "element box", `() => { const r = document.getElementById('ok').getBoundingClientRect(); return {x:r.x, y:r.y, width:r.width, height:r.height}; }`)
	rect := plainMap(t, "element box", box.Result)
	var clipped struct {
		Data string `json:"data"`
	}
	if err := s.conn.Command(ctx, "browsingContext.captureScreenshot", map[string]any{
		"context": s.contextID,
		"origin":  "document",
		"clip": map[string]any{
			"type":   "box",
			"x":      rect["x"],
			"y":      rect["y"],
			"width":  rect["width"],
			"height": rect["height"],
		},
	}, &clipped); err != nil {
		t.Fatalf("clipped captureScreenshot: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(clipped.Data)
	if err != nil || !strings.HasPrefix(string(raw), "\x89PNG") {
		t.Fatalf("clipped capture is not a PNG (err %v, %d bytes)", err, len(raw))
	}
	t.Logf("clipped capture -> %d bytes", len(raw))
}

// docs/install.md says Firefox marks pages as automated for as long as the
// remote agent is enabled, whether or not a client is driving them. That
// sentence is why brw tells people Firefox cannot carry the signed-in-session
// use case, so it has to be a measurement rather than a reading of the source.
//
// Neither case connects a BiDi client at all: the page reports
// navigator.webdriver to the fixture server itself, so what is measured is the
// browser's state and not a session's. The off row is the control — without it
// a true reading could just as well mean the fixture always reports true.
func TestFirefoxRemoteAgentMarksEveryPageAutomated(t *testing.T) {
	requireLiveFirefox(t)
	for _, tc := range []struct {
		name        string
		remoteAgent bool
		want        string
	}{
		{name: "remote agent enabled", remoteAgent: true, want: "true"},
		{name: "remote agent disabled", remoteAgent: false, want: "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reported := make(chan string, 4)
			mux := http.NewServeMux()
			mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
				select {
				case reported <- r.URL.Query().Get("v"):
				default:
				}
				w.WriteHeader(http.StatusNoContent)
			})
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte(`<!doctype html><title>webdriver probe</title><script>
fetch('/report?v=' + String(navigator.webdriver));
</script>`))
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			path, err := FindFirefox("")
			if err != nil {
				t.Fatalf("find firefox: %v", err)
			}
			profile := filepath.Join(t.TempDir(), "profile")
			if err := os.MkdirAll(profile, 0o700); err != nil {
				t.Fatalf("profile dir: %v", err)
			}
			args := []string{"--headless", "--no-remote", "--profile", profile}
			if tc.remoteAgent {
				args = append(args, "--remote-debugging-port", "0")
			}
			args = append(args, srv.URL+"/")
			cmd := exec.Command(path, args...)
			if err := cmd.Start(); err != nil {
				t.Fatalf("start firefox: %v", err)
			}
			defer func() {
				_ = cmd.Process.Signal(syscall.SIGTERM)
				_, _ = cmd.Process.Wait()
			}()

			select {
			case got := <-reported:
				if got != tc.want {
					t.Fatalf("navigator.webdriver = %s with the remote agent %v; docs/install.md's claim about Firefox must be corrected", got, tc.remoteAgent)
				}
			case <-time.After(90 * time.Second):
				t.Fatal("the page never reported navigator.webdriver")
			}
		})
	}
}

func truncateForLog(s string) string {
	if len(s) <= 600 {
		return s
	}
	return s[:600] + "…"
}
