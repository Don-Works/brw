package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// call is what the stand-in daemon saw: the whole wire contract a verb is
// responsible for.
type call struct {
	method string
	path   string
	query  url.Values
	body   string
}

// fakeDaemon stands in for brwd. It records each request and replies with the
// canned JSON for the route, so a verb test asserts what actually crossed the
// socket rather than what the CLI intended to send.
func fakeDaemon(t *testing.T, responses map[string]string) (*httptest.Server, *[]call) {
	t.Helper()
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		calls = append(calls, call{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.Query(),
			body:   strings.TrimSpace(string(body)),
		})
		payload, ok := responses[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"no canned response for this route"}`)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

const (
	openResponse       = `{"tab":{"id":"1234","url":"https://example.test/","title":"Example"},"ready":true}`
	actionResponse     = `{"ok":true,"url":"https://example.test/"}`
	elementsResponse   = `{"url":"https://example.test/","title":"Example","elements":[{"ref":"e17","role":"button","name":"Sign in","tag":"button","visible":true,"in_viewport":true},{"ref":"e18","role":"textbox","name":"Email","tag":"input","visible":true,"in_viewport":true}]}`
	readResponse       = `{"url":"https://example.test/","title":"Example","main":"Example body text.","headings":[],"links":[],"forms":[],"tables":[],"metadata":{}}`
	tabsResponse       = `[{"id":"1234","url":"https://example.test/","title":"Example","type":"page","active":true}]`
	downloadsResponse  = `{"downloads":[{"guid":"g1","url":"https://example.test/report.csv","suggested_filename":"report.csv","state":"completed","received_bytes":12,"total_bytes":12,"path":"report.csv"}],"count":1,"supported":true}`
	artifactResponse   = `{"artifact_id":"art_1","offset":0,"size_bytes":11,"total_bytes":11,"text":"page text\n","encoding":"utf-8","more":false}`
	healthResponse     = `{"ok":true,"identity":{"workspace":"work","profile":"work-profile","mode":"bridge","transport":"extension-bridge"}}`
	screenshotResponse = `{"mime_type":"image/png","base64":"aGVsbG8="}`
)

func daemonResponses() map[string]string {
	return map[string]string{
		"POST /api/browser/open":           openResponse,
		"GET /api/browser/tabs":            tabsResponse,
		"GET /api/page/find":               elementsResponse,
		"GET /api/page/snapshot":           elementsResponse,
		"GET /api/page/read":               readResponse,
		"POST /api/page/click":             actionResponse,
		"POST /api/page/fill":              actionResponse,
		"POST /api/page/type":              actionResponse,
		"POST /api/page/press":             actionResponse,
		"POST /api/page/wait_for":          actionResponse,
		"GET /api/page/downloads":          downloadsResponse,
		"POST /api/artifacts/read":         artifactResponse,
		"GET /api/visual/screenshot":       screenshotResponse,
		"GET /health":                      healthResponse,
		"POST /api/page/navigate_to":       actionResponse,
		"POST /api/page/navigate":          actionResponse,
		"POST /api/page/hover":             actionResponse,
		"POST /api/page/select":            actionResponse,
		"POST /api/page/scroll":            actionResponse,
		"POST /api/page/key_down":          actionResponse,
		"POST /api/page/key_up":            actionResponse,
		"POST /api/page/focus":             actionResponse,
		"POST /api/page/cookies":           `{"action":"list","cookies":[{"name":"sid","domain":"example.test","path":"/"}],"count":1}`,
		"GET /api/page/get":                `{"value":"Example Domain"}`,
		"POST /api/page/evaluate":          `{"value":2}`,
		"GET /api/page/console":            `[{"level":"error","text":"boom"}]`,
		"POST /api/browser/close":          actionResponse,
		"POST /api/browser/focus":          actionResponse,
		"POST /api/browser/open_incognito": openResponse,
		"POST /api/page/locale":            `{"ok":true,"locale":{"locale":"en-GB","timezone":"Europe/London"}}`,
		"POST /api/page/batch":             `{"ok":true,"steps_completed":1}`,
		"POST /api/page/init_script":       `{"ok":true,"count":1,"scripts":[{"id":"1","sha256":"abc","bytes":4}]}`,
	}
}

func TestVerbsDriveTheDaemonHTTPAPI(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantMethod string
		wantPath   string
		wantQuery  url.Values
		wantBody   string
		wantStdout []string
	}{
		{
			name:       "open posts the url",
			args:       []string{"open", "https://example.test/"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/browser/open",
			wantBody:   `{"url":"https://example.test/"}`,
			wantStdout: []string{"opened https://example.test/ (tab 1234, ready)"},
		},
		{
			name:       "open carries a tab group",
			args:       []string{"open", "https://example.test/", "--group", "research"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/browser/open",
			wantBody:   `{"group":"research","url":"https://example.test/"}`,
			wantStdout: []string{"opened https://example.test/"},
		},
		{
			name:       "find queries by text and prints refs",
			args:       []string{"find", "sign in"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/page/find",
			wantQuery:  url.Values{"query": {"sign in"}},
			wantStdout: []string{"@e17", "button", "Sign in", "@e18"},
		},
		{
			name:       "find passes role and limit",
			args:       []string{"find", "--role", "button", "--limit", "5", "--viewport-only"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/page/find",
			wantQuery:  url.Values{"role": {"button"}, "limit": {"5"}, "viewport_only": {"true"}},
			wantStdout: []string{"@e17"},
		},
		{
			name:       "click accepts the ref as printed",
			args:       []string{"click", "@e17"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/page/click",
			wantBody:   `{"ref":"e17"}`,
			wantStdout: []string{"ok", "https://example.test/"},
		},
		{
			name:       "click accepts a bare ref",
			args:       []string{"click", "e17"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/page/click",
			wantBody:   `{"ref":"e17"}`,
		},
		{
			name:       "click takes a flag after the ref",
			args:       []string{"click", "@e17", "--tab", "99"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/page/click",
			wantBody:   `{"ref":"e17","tab_id":"99"}`,
		},
		{
			name:       "fill replaces by default",
			args:       []string{"fill", "@e18", "someone@example.test"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/page/fill",
			wantBody:   `{"ref":"e18","replace":true,"text":"someone@example.test"}`,
		},
		{
			name:       "fill appends when asked",
			args:       []string{"fill", "@e18", "more", "--append"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/page/fill",
			wantBody:   `{"ref":"e18","replace":false,"text":"more"}`,
		},
		{
			name:       "type sends ref and text",
			args:       []string{"type", "@e18", "hello"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/page/type",
			wantBody:   `{"ref":"e18","text":"hello"}`,
		},
		{
			name:       "press sends the key",
			args:       []string{"press", "Enter"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/page/press",
			wantBody:   `{"key":"Enter"}`,
		},
		{
			name:       "read is unbounded unless bounded",
			args:       []string{"read"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/page/read",
			wantQuery:  url.Values{},
			wantStdout: []string{"Example", "https://example.test/", "Example body text."},
		},
		{
			name:       "read bounds the window when asked",
			args:       []string{"read", "--max-chars", "100", "--offset", "40"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/page/read",
			wantQuery: url.Values{
				"max_chars": {"100"}, "offset": {"40"},
				"max_links": {"-1"}, "max_headings": {"-1"},
			},
		},
		{
			// The route bounds every list the moment one bound is present, so an
			// offset on its own has to say -1 for the rest or the prose comes
			// back capped at the route's default.
			name:       "read from an offset leaves everything else unbounded",
			args:       []string{"read", "--offset", "5000"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/page/read",
			wantQuery: url.Values{
				"max_chars": {"-1"}, "offset": {"5000"},
				"max_links": {"-1"}, "max_headings": {"-1"},
			},
		},
		{
			name:       "snapshot prints refs",
			args:       []string{"snapshot", "--limit", "20"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/page/snapshot",
			wantQuery:  url.Values{"limit": {"20"}},
			wantStdout: []string{"@e17", "@e18"},
		},
		{
			name:       "wait without a timeout leaves the daemon default",
			args:       []string{"wait", "load"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/page/wait_for",
			wantBody:   `{"condition":"load"}`,
		},
		{
			name:       "wait forwards an explicit timeout",
			args:       []string{"wait", "text:Welcome", "--timeout", "5s"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/page/wait_for",
			wantBody:   `{"condition":"text:Welcome","timeout_ms":5000}`,
		},
		{
			name:       "tabs lists the open tabs",
			args:       []string{"tabs"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/browser/tabs",
			wantStdout: []string{"*", "1234", "https://example.test/"},
		},
		{
			name:       "downloads lists captured downloads",
			args:       []string{"downloads"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/page/downloads",
			wantStdout: []string{"completed", "report.csv"},
		},
		{
			name:       "artifact read posts the handle",
			args:       []string{"artifact", "read", "art_1"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/artifacts/read",
			wantBody:   `{"artifact_id":"art_1","max_bytes":0,"offset":0}`,
			wantStdout: []string{"page text"},
		},
		{
			name:       "artifact read windows the handle",
			args:       []string{"artifact", "read", "art_1", "--offset", "16", "--max-bytes", "64"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/artifacts/read",
			wantBody:   `{"artifact_id":"art_1","max_bytes":64,"offset":16}`,
		},
		{
			name:       "health reports the daemon identity",
			args:       []string{"health"},
			wantMethod: http.MethodGet,
			wantPath:   "/health",
			wantStdout: []string{"ok", "profile=work-profile", "transport=extension-bridge"},
		},
		{
			name:       "goto navigates the current tab",
			args:       []string{"goto", "https://example.test/next"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/page/navigate_to",
			wantBody:   `{"url":"https://example.test/next"}`,
			wantStdout: []string{"ok"},
		},
		{
			name:       "back sends the history direction",
			args:       []string{"back"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/page/navigate",
			wantBody:   `{"direction":"back"}`,
		},
		{
			name:       "dblclick posts click_count 2",
			args:       []string{"dblclick", "@e17"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/page/click",
			wantBody:   `{"click_count":2,"ref":"e17"}`,
		},
		{
			name:       "hover posts the ref",
			args:       []string{"hover", "@e17"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/page/hover",
			wantBody:   `{"ref":"e17"}`,
		},
		{
			name:       "select posts ref and value",
			args:       []string{"select", "@e18", "uk"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/page/select",
			wantBody:   `{"ref":"e18","value":"uk"}`,
		},
		{
			name:       "get url queries the typed-fact route",
			args:       []string{"get", "url"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/page/get",
			wantQuery:  url.Values{"what": {"url"}},
			wantStdout: []string{"Example Domain"},
		},
		{
			name:       "get text normalises the printed ref",
			args:       []string{"get", "text", "@e17"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/page/get",
			wantQuery:  url.Values{"what": {"text"}, "target": {"e17"}},
		},
		{
			name:       "eval posts the expression",
			args:       []string{"eval", "1+1"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/page/evaluate",
			wantBody:   `{"expression":"1+1"}`,
		},
		{
			name:       "cookies lists without extra fields",
			args:       []string{"cookies"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/page/cookies",
			wantBody:   `{"action":"list"}`,
			wantStdout: []string{"sid"},
		},
		{
			name:       "tab close posts the tab id",
			args:       []string{"tab", "close", "1234"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/browser/close",
			wantBody:   `{"tab_id":"1234"}`,
		},
		{
			name:       "incognito opens an isolated context",
			args:       []string{"incognito", "https://example.test/"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/browser/open_incognito",
			wantBody:   `{"url":"https://example.test/"}`,
			wantStdout: []string{"opened https://example.test/"},
		},
		{
			name:       "locale posts bcp47 and timezone",
			args:       []string{"locale", "en-GB", "Europe/London"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/page/locale",
			wantBody:   `{"locale":"en-GB","timezone":"Europe/London"}`,
		},
		{
			name:       "batch posts parsed steps",
			args:       []string{"batch", `[{"action":"click","ref":"e1"}]`},
			wantMethod: http.MethodPost,
			wantPath:   "/api/page/batch",
			wantBody:   `{"steps":[{"action":"click","ref":"e1"}]}`,
		},
		{
			name:       "init-script add posts source and origin",
			args:       []string{"init-script", "add", "window.x=1;", "--origin", "https://app.example.test"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/page/init_script",
			wantBody:   `{"action":"add","origin":"https://app.example.test","source":"window.x=1;"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, calls := fakeDaemon(t, daemonResponses())
			t.Setenv("BRW_URL", srv.URL)

			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), tt.args, &stdout, &stderr); code != ExitOK {
				t.Fatalf("exit=%d, want %d (stderr=%q)", code, ExitOK, stderr.String())
			}
			if len(*calls) != 1 {
				t.Fatalf("daemon saw %d requests, want exactly 1: %+v", len(*calls), *calls)
			}
			got := (*calls)[0]
			if got.method != tt.wantMethod || got.path != tt.wantPath {
				t.Fatalf("request = %s %s, want %s %s", got.method, got.path, tt.wantMethod, tt.wantPath)
			}
			if tt.wantQuery != nil && got.query.Encode() != tt.wantQuery.Encode() {
				t.Fatalf("query = %q, want %q", got.query.Encode(), tt.wantQuery.Encode())
			}
			if got.body != tt.wantBody {
				t.Fatalf("body = %q, want %q", got.body, tt.wantBody)
			}
			for _, want := range tt.wantStdout {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("stdout %q does not contain %q", stdout.String(), want)
				}
			}
		})
	}
}

// A global flag means the same thing on either side of the verb.
func TestGlobalFlagsMayPrecedeTheVerb(t *testing.T) {
	srv, calls := fakeDaemon(t, daemonResponses())
	t.Setenv("BRW_URL", "http://127.0.0.1:1")

	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--daemon", srv.URL, "--json", "tabs"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit=%d (stderr=%q)", code, stderr.String())
	}
	if len(*calls) != 1 || (*calls)[0].path != "/api/browser/tabs" {
		t.Fatalf("daemon saw %+v, want one GET of the tabs route", *calls)
	}
	if stdout.String() != tabsResponse+"\n" {
		t.Fatalf("stdout = %q, want the envelope %q", stdout.String(), tabsResponse)
	}
}

// The point of --json is that the daemon's own bytes reach the pipe: a field
// this build's structs do not know about must survive.
func TestJSONPrintsTheDaemonEnvelopeVerbatim(t *testing.T) {
	const envelope = `{"ok":true,"url":"https://example.test/","field_added_after_this_build":42}`
	srv, _ := fakeDaemon(t, map[string]string{"POST /api/page/click": envelope})
	t.Setenv("BRW_URL", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"click", "@e17", "--json"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit=%d (stderr=%q)", code, stderr.String())
	}
	if stdout.String() != envelope+"\n" {
		t.Fatalf("stdout = %q, want the envelope verbatim %q", stdout.String(), envelope)
	}
}

func TestScreenshotWritesThePNGOwnerOnly(t *testing.T) {
	srv, calls := fakeDaemon(t, daemonResponses())
	t.Setenv("BRW_URL", srv.URL)
	out := filepath.Join(t.TempDir(), "shot.png")

	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"screenshot", "--out", out}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit=%d (stderr=%q)", code, stderr.String())
	}
	got := (*calls)[0]
	if got.method != http.MethodGet || got.path != "/api/visual/screenshot" || got.query.Get("base64") != "1" {
		t.Fatalf("request = %s %s?%s, want a base64 screenshot GET", got.method, got.path, got.query.Encode())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := base64.StdEncoding.DecodeString("aGVsbG8=")
	if !bytes.Equal(data, want) {
		t.Fatalf("file = %q, want the decoded PNG bytes %q", data, want)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("screenshot mode = %o, want 0600", perm)
	}
	if !strings.Contains(stdout.String(), "image/png") {
		t.Fatalf("stdout = %q, want the written file reported", stdout.String())
	}
}

func TestExitCodes(t *testing.T) {
	srv, _ := fakeDaemon(t, map[string]string{})
	tests := []struct {
		name     string
		daemon   string
		args     []string
		wantExit int
		wantErr  string
	}{
		{
			name:     "the daemon refusing the action",
			daemon:   srv.URL,
			args:     []string{"click", "@e17"},
			wantExit: ExitActionFailed,
			wantErr:  "no canned response",
		},
		{
			name: "nothing listening",
			// Port 1 on loopback has nothing listening, so the request never
			// reaches a daemon.
			daemon:   "http://127.0.0.1:1",
			args:     []string{"click", "@e17"},
			wantExit: ExitNoDaemon,
			wantErr:  "no brw daemon reachable",
		},
		{
			name:     "an unknown verb",
			daemon:   srv.URL,
			args:     []string{"frobnicate"},
			wantExit: ExitUsage,
			wantErr:  "unknown command",
		},
		{
			name:     "a missing argument",
			daemon:   srv.URL,
			args:     []string{"click"},
			wantExit: ExitUsage,
			wantErr:  "needs exactly one ref",
		},
		{
			name:     "an unknown flag",
			daemon:   srv.URL,
			args:     []string{"click", "@e17", "--nope"},
			wantExit: ExitUsage,
			wantErr:  "unknown flag --nope",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BRW_URL", tt.daemon)
			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), tt.args, &stdout, &stderr); code != tt.wantExit {
				t.Fatalf("exit=%d, want %d (stderr=%q)", code, tt.wantExit, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want it to mention %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

// An action the daemon answers 200 with ok:false is still a failure, and a
// shell script only ever sees the exit code. The reason is printed once: the
// verb's own output already carries it, so a second copy on stderr shows the
// line twice in a terminal.
func TestRefusedActionExitsNonZero(t *testing.T) {
	const reason = "element is not clickable"
	tests := []struct {
		name       string
		args       []string
		wantStdout int
		wantStderr int
	}{
		{
			// Once, on stdout, from the verb's own output.
			name:       "human output",
			args:       []string{"click", "@e17"},
			wantStdout: 1,
			wantStderr: 0,
		},
		{
			// --json prints the envelope, which carries the reason; stderr is
			// where a person reads it, since nothing renders it in this mode.
			name:       "json output",
			args:       []string{"click", "@e17", "--json"},
			wantStdout: 1,
			wantStderr: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := fakeDaemon(t, map[string]string{
				"POST /api/page/click": `{"ok":false,"message":"` + reason + `"}`,
			})
			t.Setenv("BRW_URL", srv.URL)

			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), tt.args, &stdout, &stderr); code != ExitActionFailed {
				t.Fatalf("exit=%d, want %d", code, ExitActionFailed)
			}
			if got := strings.Count(stdout.String(), reason); got != tt.wantStdout {
				t.Fatalf("stdout = %q, want the reason %d time(s)", stdout.String(), tt.wantStdout)
			}
			if got := strings.Count(stderr.String(), reason); got != tt.wantStderr {
				t.Fatalf("stderr = %q, want the reason %d time(s)", stderr.String(), tt.wantStderr)
			}
		})
	}
}

// slowDaemon answers nothing until the client gives up, which is what a daemon
// mid-action looks like from here.
func slowDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Drain the body first: net/http only starts watching for the client
		// going away once the request body has been consumed, so a handler that
		// ignores it never sees the disconnect and blocks the server's Close.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --timeout is advertised as the per-action timeout, so it has to be able to
// shorten one. Only the verb that hands its timeout to the daemon waits longer
// than it asked for, and that verb is not this one.
func TestShortTimeoutAbortsTheAction(t *testing.T) {
	srv := slowDaemon(t)
	t.Setenv("BRW_URL", srv.URL)

	var stdout, stderr bytes.Buffer
	started := time.Now()
	code := Run(context.Background(), []string{"click", "@e17", "--timeout", "200ms"}, &stdout, &stderr)
	elapsed := time.Since(started)

	if code != ExitActionFailed {
		t.Fatalf("exit=%d, want %d (stderr=%q)", code, ExitActionFailed, stderr.String())
	}
	if elapsed > 5*time.Second {
		t.Fatalf("--timeout 200ms waited %s before giving up", elapsed)
	}
	if !strings.Contains(stderr.String(), "timed out after 200ms") {
		t.Fatalf("stderr = %q, want the timeout named", stderr.String())
	}
	if strings.Contains(stderr.String(), errNoDaemon.Error()) {
		t.Fatalf("stderr = %q; a daemon that answered too slowly is present, not missing", stderr.String())
	}
}

// The wait verb is the exception: the daemon enforces its timeout server-side,
// so the client has to outlast it rather than cut the answer off.
func TestWaitKeepsClientHeadroomOverTheDaemonTimeout(t *testing.T) {
	srv, calls := fakeDaemon(t, daemonResponses())
	t.Setenv("BRW_URL", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"wait", "load", "--timeout", "5s"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit=%d (stderr=%q)", code, stderr.String())
	}
	if (*calls)[0].body != `{"condition":"load","timeout_ms":5000}` {
		t.Fatalf("body = %q, want the timeout forwarded to the daemon", (*calls)[0].body)
	}
	for _, v := range verbs() {
		forwards := v.name == "wait" || strings.HasPrefix(v.name, "assert ")
		if v.serverTimeout != forwards {
			t.Errorf("verb %q serverTimeout=%v; only a verb that forwards timeout_ms may claim it", v.name, v.serverTimeout)
		}
	}
}

// Ctrl-C is wired into the context cmd/brw passes in. Reporting that as "no
// daemon reachable" tells a script to start a daemon that is already running.
func TestCancellationIsNotAMissingDaemon(t *testing.T) {
	srv := slowDaemon(t)
	t.Setenv("BRW_URL", srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(100*time.Millisecond, cancel)
	defer timer.Stop()

	var stdout, stderr bytes.Buffer
	code := Run(ctx, []string{"click", "@e17"}, &stdout, &stderr)
	if code != ExitActionFailed {
		t.Fatalf("exit=%d, want %d (stderr=%q)", code, ExitActionFailed, stderr.String())
	}
	if strings.Contains(stderr.String(), errNoDaemon.Error()) {
		t.Fatalf("stderr = %q; the daemon answered the connection, it was the caller that stopped", stderr.String())
	}
	if !strings.Contains(stderr.String(), "cancelled") {
		t.Fatalf("stderr = %q, want the cancellation named", stderr.String())
	}
}

// A capability the transport does not have answers 200 with an empty list, so
// without the flag being read a script cannot tell it from "no downloads".
func TestUnsupportedDownloadsExitNonZero(t *testing.T) {
	const note = "the extension bridge cannot observe downloads"
	const envelope = `{"downloads":[],"count":0,"supported":false,"note":"` + note + `"}`
	tests := []struct {
		name       string
		args       []string
		wantStdout int
		wantStderr int
	}{
		// The human line is on stdout, so stderr stays clear; --json renders
		// nothing, and the envelope's own note is the one on stdout.
		{name: "human output", args: []string{"downloads"}, wantStdout: 1, wantStderr: 0},
		{name: "json output", args: []string{"downloads", "--json"}, wantStdout: 1, wantStderr: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := fakeDaemon(t, map[string]string{"GET /api/page/downloads": envelope})
			t.Setenv("BRW_URL", srv.URL)

			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), tt.args, &stdout, &stderr); code != ExitActionFailed {
				t.Fatalf("exit=%d, want %d", code, ExitActionFailed)
			}
			if got := strings.Count(stdout.String(), note); got != tt.wantStdout {
				t.Fatalf("stdout = %q, want the note %d time(s)", stdout.String(), tt.wantStdout)
			}
			if got := strings.Count(stderr.String(), note); got != tt.wantStderr {
				t.Fatalf("stderr = %q, want the note %d time(s)", stderr.String(), tt.wantStderr)
			}
		})
	}
}

// A transport that can observe downloads still succeeds, so the check above is
// reading the flag rather than failing the verb outright.
func TestSupportedDownloadsSucceed(t *testing.T) {
	srv, _ := fakeDaemon(t, daemonResponses())
	t.Setenv("BRW_URL", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"downloads"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit=%d, want %d (stderr=%q)", code, ExitOK, stderr.String())
	}
}

// os.WriteFile applies its mode only when it creates the file, and --out
// defaults to a fixed name in the working directory, so overwriting yesterday's
// screenshot is the common case.
func TestScreenshotNarrowsAnExistingFile(t *testing.T) {
	srv, _ := fakeDaemon(t, daemonResponses())
	t.Setenv("BRW_URL", srv.URL)
	out := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(out, []byte("older screenshot"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"screenshot", "--out", out}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit=%d (stderr=%q)", code, stderr.String())
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("overwritten screenshot mode = %o, want 0600", perm)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := base64.StdEncoding.DecodeString("aGVsbG8=")
	if !bytes.Equal(data, want) {
		t.Fatalf("file = %q, want the new PNG bytes %q", data, want)
	}
}

// The write narrows an existing file before it truncates it, so a filesystem
// that rejects fchmod costs the caller this screenshot rather than the one
// already on disk.
func TestScreenshotSurvivesAFilesystemThatRejectsTheMode(t *testing.T) {
	const previous = "older screenshot"
	wanted, _ := base64.StdEncoding.DecodeString("aGVsbG8=")
	tests := []struct {
		name     string
		narrow   error
		wantCode int
		wantFile []byte
	}{
		{name: "the mode is accepted", wantCode: ExitOK, wantFile: wanted},
		{name: "the filesystem rejects the mode", narrow: errors.New("operation not supported"), wantCode: ExitActionFailed, wantFile: []byte(previous)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := fakeDaemon(t, daemonResponses())
			t.Setenv("BRW_URL", srv.URL)
			out := filepath.Join(t.TempDir(), "shot.png")
			if err := os.WriteFile(out, []byte(previous), 0o644); err != nil {
				t.Fatal(err)
			}
			if tt.narrow != nil {
				restore := narrowToOwner
				narrowToOwner = func(*os.File) error { return tt.narrow }
				t.Cleanup(func() { narrowToOwner = restore })
			}

			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), []string{"screenshot", "--out", out}, &stdout, &stderr); code != tt.wantCode {
				t.Fatalf("exit=%d, want %d (stderr=%q)", code, tt.wantCode, stderr.String())
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, tt.wantFile) {
				t.Fatalf("file = %q, want %q", data, tt.wantFile)
			}
		})
	}
}

// The walk-through in the CLI's own help: open, find, click, read, snapshot
// against one daemon, each verb reaching its own route.
func TestOpenFindClickReadSnapshotDriveOneDaemon(t *testing.T) {
	srv, calls := fakeDaemon(t, daemonResponses())
	t.Setenv("BRW_URL", srv.URL)

	sequence := [][]string{
		{"open", "https://example.test/"},
		{"find", "sign in"},
		{"click", "@e17"},
		{"read"},
		{"snapshot"},
	}
	for _, args := range sequence {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), args, &stdout, &stderr); code != ExitOK {
			t.Fatalf("brw %s exit=%d (stderr=%q)", strings.Join(args, " "), code, stderr.String())
		}
		if stdout.Len() == 0 {
			t.Fatalf("brw %s printed nothing", strings.Join(args, " "))
		}
	}

	var got []string
	for _, c := range *calls {
		got = append(got, c.method+" "+c.path)
	}
	want := []string{
		"POST /api/browser/open",
		"GET /api/page/find",
		"POST /api/page/click",
		"GET /api/page/read",
		"GET /api/page/snapshot",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("daemon saw %v, want %v", got, want)
	}
}

func TestSplitFlagsSeparatesFlagsFromPositionals(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		wantFlags      []string
		wantPositional []string
		wantErr        string
	}{
		{
			name:           "flag after a positional",
			args:           []string{"@e17", "--json"},
			wantFlags:      []string{"--json"},
			wantPositional: []string{"@e17"},
		},
		{
			name:           "value flag after a positional",
			args:           []string{"@e17", "--tab", "99"},
			wantFlags:      []string{"--tab", "99"},
			wantPositional: []string{"@e17"},
		},
		{
			name:           "inline value",
			args:           []string{"--tab=99", "@e17"},
			wantFlags:      []string{"--tab=99"},
			wantPositional: []string{"@e17"},
		},
		{
			name:           "everything after -- is positional",
			args:           []string{"--", "--not-a-flag"},
			wantPositional: []string{"--not-a-flag"},
		},
		{
			name:    "a value flag with nothing after it",
			args:    []string{"--tab"},
			wantErr: "flag --tab needs a value",
		},
		{
			name:    "an unknown flag",
			args:    []string{"--nope"},
			wantErr: "unknown flag --nope",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newTestFlagSet()
			flags, positional, err := splitFlags(fs, tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(flags, " ") != strings.Join(tt.wantFlags, " ") {
				t.Fatalf("flags = %v, want %v", flags, tt.wantFlags)
			}
			if strings.Join(positional, " ") != strings.Join(tt.wantPositional, " ") {
				t.Fatalf("positional = %v, want %v", positional, tt.wantPositional)
			}
		})
	}
}

func newTestFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("brw test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registerGlobalFlags(fs, &options{})
	return fs
}
