package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		"POST /api/browser/open":     openResponse,
		"GET /api/browser/tabs":      tabsResponse,
		"GET /api/page/find":         elementsResponse,
		"GET /api/page/snapshot":     elementsResponse,
		"GET /api/page/read":         readResponse,
		"POST /api/page/click":       actionResponse,
		"POST /api/page/fill":        actionResponse,
		"POST /api/page/type":        actionResponse,
		"POST /api/page/press":       actionResponse,
		"POST /api/page/wait_for":    actionResponse,
		"GET /api/page/downloads":    downloadsResponse,
		"POST /api/artifacts/read":   artifactResponse,
		"GET /api/visual/screenshot": screenshotResponse,
		"GET /health":                healthResponse,
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
			wantQuery:  url.Values{"max_chars": {"100"}, "offset": {"40"}},
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
// shell script only ever sees the exit code.
func TestRefusedActionExitsNonZero(t *testing.T) {
	srv, _ := fakeDaemon(t, map[string]string{
		"POST /api/page/click": `{"ok":false,"message":"element is not clickable"}`,
	})
	t.Setenv("BRW_URL", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"click", "@e17"}, &stdout, &stderr); code != ExitActionFailed {
		t.Fatalf("exit=%d, want %d", code, ExitActionFailed)
	}
	if !strings.Contains(stderr.String(), "element is not clickable") {
		t.Fatalf("stderr = %q, want the daemon's reason", stderr.String())
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
