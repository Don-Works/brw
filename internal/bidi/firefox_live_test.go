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

func requireLiveFirefox(t *testing.T) {
	t.Helper()
	if os.Getenv(liveEnv) != "1" {
		t.Skipf("set %s=1 to re-run the BiDi measurements against a real Firefox (docs/bidi-prototype.md)", liveEnv)
	}
	if _, err := FindFirefox(""); err != nil {
		t.Skipf("firefox not available: %v", err)
	}
}

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

type callResult struct {
	Type             string          `json:"type"`
	Realm            string          `json:"realm"`
	Result           json.RawMessage `json:"result"`
	ExceptionDetails json.RawMessage `json:"exceptionDetails"`
}

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

	if err := s.conn.Subscribe(ctx, "brwNoSuch.event"); err == nil {
		t.Fatal("subscribing to an undefined event succeeded; a successful session.subscribe is then not evidence that an event exists")
	}

	for _, ev := range events {
		if err := s.conn.Subscribe(ctx, ev.name); err != nil {
			t.Fatalf("subscribe %s: %v", ev.name, err)
		}
	}

	s.navigate(ctx, t, srv.URL+"/")

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

	if err := s.conn.Command(ctx, "browsingContext.handleUserPrompt", map[string]any{
		"context": s.contextID,
		"accept":  true,
	}, nil); err != nil {
		t.Errorf("handleUserPrompt: %v", err)
	}
}

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

		{id: "ghost", wantOK: true, wantMode: "hit_test"},

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

	realm := resolved.Realm
	if realm == "" {
		t.Fatal("script.callFunction reported no realm; there is then nothing to pin a ref to")
	}

	s.navigate(ctx, t, srv.URL+"/other")

	_, err := s.call(ctx, `() => 1`, nil, map[string]any{"realm": realm})
	if err == nil {
		t.Fatal("a call pinned to the previous document's realm succeeded after navigation; the same-document guarantee is not expressible")
	}
	t.Logf("pinned stale realm -> %v", err)

	after := s.mustCall(ctx, t, "ResolveBox after navigation", snapshot.ResolveBoxScript, str(ref))
	if box := plainMap(t, "ResolveBox after navigation", after.Result); box["ok"] == true {
		t.Fatalf("a ref from the previous document still resolved after navigation: %v", box)
	}
}

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
