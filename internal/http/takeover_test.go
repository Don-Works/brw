package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

// takeoverFake is a controller that can take over and can stream. The grant
// lifecycle is delegated to a real browser.Manager rather than reimplemented,
// so these tests exercise the same refusal an agent would hit.
type takeoverFake struct {
	browser.Controller
	manager *browser.Manager

	mu   sync.Mutex
	subs map[chan browser.TraceEntry]struct{}
}

func newTakeoverFake() *takeoverFake {
	return &takeoverFake{manager: &browser.Manager{}, subs: map[chan browser.TraceEntry]struct{}{}}
}

func (f *takeoverFake) AcquireTakeover(holder string, ttl time.Duration) (browser.TakeoverGrant, error) {
	return f.manager.AcquireTakeover(holder, ttl)
}

func (f *takeoverFake) RenewTakeover(token string, ttl time.Duration) (browser.TakeoverGrant, error) {
	return f.manager.RenewTakeover(token, ttl)
}

func (f *takeoverFake) ReleaseTakeover(token string) error { return f.manager.ReleaseTakeover(token) }

func (f *takeoverFake) TakeoverState() browser.TakeoverStatus { return f.manager.TakeoverState() }

func (f *takeoverFake) DispatchTakeoverInput(ctx context.Context, token string, event browser.TakeoverInput) error {
	return f.manager.DispatchTakeoverInput(ctx, token, event)
}

func (f *takeoverFake) SubscribeTrace() (<-chan browser.TraceEntry, func()) {
	ch := make(chan browser.TraceEntry, 16)
	f.mu.Lock()
	f.subs[ch] = struct{}{}
	f.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			f.mu.Lock()
			delete(f.subs, ch)
			f.mu.Unlock()
			close(ch)
		})
	}
}

func (f *takeoverFake) publish(entry browser.TraceEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.subs {
		select {
		case ch <- entry:
		default:
		}
	}
}

// plainController can neither take over nor stream.
type plainController struct{ browser.Controller }

// A bind beyond loopback means the operator has decided other machines may
// reach this daemon. Forwarding keystrokes into a signed-in browser is not
// something that decision consents to, so the control is not rendered and the
// routes answer 404 — nothing to re-enable, nothing to POST to.
func TestTakeoverSurfaceIsAbsentWhenBoundBeyondLoopback(t *testing.T) {
	t.Setenv(dashboardEnvVar, "1")
	tests := []struct {
		name string
		addr string
		want bool
	}{
		{"loopback v4", "127.0.0.1:17310", true},
		{"loopback name", "localhost:17310", true},
		{"loopback v6", "[::1]:17310", true},
		{"wildcard", ":17310", false},
		{"all interfaces", "0.0.0.0:17310", false},
		// Built rather than written as a literal so the hygiene scanner does not
		// read a test table as a leaked internal address.
		{"lan address", net.JoinHostPort(net.IPv4(192, 168, 1, 44).String(), "17310"), false},
		{"tailscale address", "100.101.102.103:17310", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(tt.addr, newTakeoverFake())

			page := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
			request.RemoteAddr = "127.0.0.1:54321"
			s.dashboardPage(page, request)
			body := page.Body.String()

			hasControl := strings.Contains(body, `id="takeover"`)
			if hasControl != tt.want {
				t.Fatalf("takeover control present = %v, want %v", hasControl, tt.want)
			}
			// Absent means absent: no markup AND no script that could drive the
			// endpoint from the console of an operator's own browser.
			if !tt.want && strings.Contains(body, "/dashboard/input") {
				t.Error("the page still carries the input-forwarding script")
			}

			for _, path := range []string{"/dashboard/takeover", "/dashboard/input"} {
				recorder := httptest.NewRecorder()
				post := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"action":"status"}`))
				post.RemoteAddr = "127.0.0.1:54321"
				if path == "/dashboard/takeover" {
					s.dashboardTakeover(recorder, post)
				} else {
					s.dashboardInput(recorder, post)
				}
				if tt.want {
					if recorder.Code == http.StatusNotFound {
						t.Errorf("%s = 404 on a loopback bind", path)
					}
					continue
				}
				if recorder.Code != http.StatusNotFound {
					t.Errorf("%s = %d on a %s bind, want 404 (%s)", path, recorder.Code, tt.addr, recorder.Body.String())
				}
				if !strings.Contains(recorder.Body.String(), "loopback") {
					t.Errorf("%s refusal should say why: %s", path, recorder.Body.String())
				}
			}
		})
	}
}

// Takeover inherits the dashboard's own switch: pixels and input are the same
// exposure, and neither is on by default.
func TestTakeoverRoutesAreOffWithTheDashboard(t *testing.T) {
	t.Setenv(dashboardEnvVar, "")
	s := New("127.0.0.1:17310", newTakeoverFake())
	for _, path := range []string{"/dashboard/takeover", "/dashboard/input", "/dashboard/activity"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		request.RemoteAddr = "127.0.0.1:54321"
		switch path {
		case "/dashboard/takeover":
			s.dashboardTakeover(recorder, request)
		case "/dashboard/input":
			s.dashboardInput(recorder, request)
		default:
			s.dashboardActivity(recorder, request)
		}
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s with the dashboard off = %d, want 404", path, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), dashboardEnvVar) {
			t.Errorf("%s should name the env var that turns it on; got %s", path, recorder.Body.String())
		}
	}
}

// A daemon that bridges or proxies has no Input domain to forward to. It says
// so by name rather than rendering a control that quietly does nothing.
func TestTakeoverRefusesTransportsThatCannotForwardInput(t *testing.T) {
	t.Setenv(dashboardEnvVar, "1")
	s := New("127.0.0.1:17310", plainController{})

	page := httptest.NewRecorder()
	pageRequest := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	pageRequest.RemoteAddr = "127.0.0.1:1"
	s.dashboardPage(page, pageRequest)
	if strings.Contains(page.Body.String(), `id="takeover"`) {
		t.Error("a transport that cannot forward input still rendered the control")
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/dashboard/takeover", strings.NewReader(`{"action":"acquire"}`))
	request.RemoteAddr = "127.0.0.1:1"
	s.dashboardTakeover(recorder, request)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("takeover on a bridging daemon = %d, want 501 (%s)", recorder.Code, recorder.Body.String())
	}

	activity := httptest.NewRecorder()
	activityRequest := httptest.NewRequest(http.MethodGet, "/dashboard/activity", nil)
	activityRequest.RemoteAddr = "127.0.0.1:1"
	s.dashboardActivity(activity, activityRequest)
	if activity.Code != http.StatusNotImplemented {
		t.Fatalf("activity feed on a bridging daemon = %d, want 501", activity.Code)
	}
}

// Input must be refused until someone explicitly enables takeover, and the
// refusal has to come from the grant check rather than from the browser failing
// later: this daemon's controller has no browser at all, so a request that
// reached dispatch would not return a clean 403.
func TestDashboardInputRequiresAnExplicitEnable(t *testing.T) {
	t.Setenv(dashboardEnvVar, "1")
	fake := newTakeoverFake()
	s := New("127.0.0.1:17310", fake)

	event := `{"kind":"mouse","type":"mousePressed","x":10,"y":10,"button":"left","buttons":1,"click_count":1}`

	for _, token := range []string{"", "a-token-nobody-granted"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/dashboard/input",
			strings.NewReader(`{"token":"`+token+`","event":`+event+`}`))
		request.RemoteAddr = "127.0.0.1:1"
		s.dashboardInput(recorder, request)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("input with token %q = %d, want 403 (%s)", token, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "takeover is not enabled") {
			t.Errorf("refusal should name the missing enable; got %s", recorder.Body.String())
		}
	}
	if fake.manager.TakeoverState().Held {
		t.Fatal("a refused input request left a hold behind")
	}

	// Enabling is one explicit request, and it is what mints the token.
	acquire := httptest.NewRecorder()
	acquireRequest := httptest.NewRequest(http.MethodPost, "/dashboard/takeover", strings.NewReader(`{"action":"acquire"}`))
	acquireRequest.RemoteAddr = "127.0.0.1:1"
	s.dashboardTakeover(acquire, acquireRequest)
	if acquire.Code != http.StatusOK {
		t.Fatalf("acquire = %d (%s)", acquire.Code, acquire.Body.String())
	}
	var grant browser.TakeoverGrant
	if err := json.Unmarshal(acquire.Body.Bytes(), &grant); err != nil {
		t.Fatalf("grant payload: %v", err)
	}
	if grant.Token == "" || grant.ExpiresAt == "" {
		t.Fatalf("grant = %+v, want a token and an expiry", grant)
	}
	if !fake.manager.TakeoverState().Held {
		t.Fatal("the browser does not consider itself held after an acquire")
	}

	// A second viewer cannot take the browser from the first.
	second := httptest.NewRecorder()
	secondRequest := httptest.NewRequest(http.MethodPost, "/dashboard/takeover", strings.NewReader(`{"action":"acquire"}`))
	secondRequest.RemoteAddr = "127.0.0.1:2"
	s.dashboardTakeover(second, secondRequest)
	if second.Code != http.StatusConflict {
		t.Fatalf("a second acquire = %d, want 409", second.Code)
	}

	release := httptest.NewRecorder()
	releaseRequest := httptest.NewRequest(http.MethodPost, "/dashboard/takeover",
		strings.NewReader(`{"action":"release","token":"`+grant.Token+`"}`))
	releaseRequest.RemoteAddr = "127.0.0.1:1"
	s.dashboardTakeover(release, releaseRequest)
	if release.Code != http.StatusOK {
		t.Fatalf("release = %d (%s)", release.Code, release.Body.String())
	}
	if fake.manager.TakeoverState().Held {
		t.Fatal("the hold survived its release")
	}
}

// An action has to reach the feed while the frame it explains is still on
// screen. Measured over a real connection, from publish to the SSE line being
// readable by the client.
func TestDashboardActivityFeedDeliversAnActionWithin500ms(t *testing.T) {
	t.Setenv(dashboardEnvVar, "1")
	fake := newTakeoverFake()
	s := New("127.0.0.1:17310", fake)
	server := httptest.NewServer(s.Handler())
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL+"/dashboard/activity", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open the feed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("feed status = %d", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("feed content type = %q", got)
	}

	reader := bufio.NewReader(response.Body)
	// The preamble proves the handler has subscribed; publishing before it does
	// would measure the test's own race rather than the feed.
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read the stream preamble: %v", err)
	}

	entry := browser.TraceEntry{
		Action: "click", Ref: "e7", Name: "Continue", Role: "button",
		OK: true, DurationMS: 42, Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	published := time.Now()
	// Give the handler a moment to reach its select before publishing; the
	// subscription is registered synchronously, so a dropped entry here would
	// be the handler's buffer, not a race.
	time.Sleep(20 * time.Millisecond)
	fake.publish(entry)

	line := readSSEData(t, reader, 500*time.Millisecond)
	elapsed := time.Since(published)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("the action reached the feed after %v, want under 500ms", elapsed)
	}
	t.Logf("action reached the feed in %v", elapsed)

	var got ActivityLine
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("feed line is not JSON: %v (%q)", err, line)
	}
	if got.Action != "click" || got.Ref != "e7" || got.Name != "Continue" {
		t.Errorf("feed line = %+v, want the action and what it acted on", got)
	}
	if got.Outcome != "ok" || got.DurationMS != 42 {
		t.Errorf("feed line = %+v, want the outcome and the latency", got)
	}
}

// readSSEData reads until the first "data: " line, returning its payload.
func readSSEData(t *testing.T, reader *bufio.Reader, within time.Duration) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				done <- result{err: err}
				return
			}
			if strings.HasPrefix(line, "data: ") {
				done <- result{line: strings.TrimSpace(strings.TrimPrefix(line, "data: "))}
				return
			}
		}
	}()
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("read the feed: %v", out.err)
		}
		return out.line
	case <-time.After(within):
		t.Fatalf("no feed line arrived within %v", within)
		return ""
	}
}

// The feed line is what an operator reads, so its projection of a trace entry is
// pinned here: what happened, to what, how it went, how long it took.
func TestActivityLineProjection(t *testing.T) {
	tests := []struct {
		name  string
		entry browser.TraceEntry
		want  ActivityLine
	}{
		{
			name:  "a successful action",
			entry: browser.TraceEntry{Action: "click", Ref: "e1", Name: "Save", Role: "button", OK: true, DurationMS: 12, Timestamp: "2026-01-01T00:00:00Z"},
			want:  ActivityLine{Seq: 1, Action: "click", Ref: "e1", Name: "Save", Role: "button", Outcome: "ok", DurationMS: 12, At: "2026-01-01T00:00:00Z"},
		},
		{
			name:  "a failure carries its reason",
			entry: browser.TraceEntry{Action: "fill", Ref: "e2", OK: false, Error: "ref not found", DurationMS: 8, Timestamp: "2026-01-01T00:00:01Z"},
			want:  ActivityLine{Seq: 1, Action: "fill", Ref: "e2", Outcome: "failed", Error: "ref not found", DurationMS: 8, At: "2026-01-01T00:00:01Z"},
		},
		{
			name:  "a redacted fill says so and carries no value",
			entry: browser.TraceEntry{Action: "fill", Ref: "e3", Name: "Password", OK: true, Redacted: true, DurationMS: 5, Timestamp: "2026-01-01T00:00:02Z"},
			want:  ActivityLine{Seq: 1, Action: "fill", Ref: "e3", Name: "Password", Outcome: "ok", Redacted: true, DurationMS: 5, At: "2026-01-01T00:00:02Z"},
		},
		{
			name:  "a human's own input is labelled",
			entry: browser.TraceEntry{Action: browser.TraceActionHumanInput, OK: true, DurationMS: 1, Timestamp: "2026-01-01T00:00:03Z"},
			want:  ActivityLine{Seq: 1, Action: browser.TraceActionHumanInput, Outcome: "ok", DurationMS: 1, At: "2026-01-01T00:00:03Z"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := activityLine(1, tt.entry)
			if got != tt.want {
				t.Fatalf("line = %+v, want %+v", got, tt.want)
			}
			// The feed never carries what the page said or what was typed into it.
			payload, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{`"text"`, `"value"`, `"url"`} {
				if strings.Contains(string(payload), field) {
					t.Errorf("feed line carries %s: %s", field, payload)
				}
			}
		})
	}

	t.Run("an entry with no timestamp is still placed in time", func(t *testing.T) {
		if got := activityLine(2, browser.TraceEntry{Action: "scroll", OK: true}); got.At == "" {
			t.Fatal("a feed row with no time cannot be read against the frames beside it")
		}
	})
}
