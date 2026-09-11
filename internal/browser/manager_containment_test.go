package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/navpolicy"
	"github.com/chromedp/cdproto/target"
	"github.com/coder/websocket"
)

// TestContainmentBlocksOffAllowlistSubresources is the regression test for the
// gap that made --allowed-domains a navigation filter rather than a confinement
// boundary: an allowed page could still fetch from anywhere.
func TestContainmentBlocksOffAllowlistSubresources(t *testing.T) {
	// The off-allowlist origin records every hit. Any hit at all is a failure.
	var offAllowlistHits int
	offAllowlist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		offAllowlistHits++
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fmt.Fprint(w, "leaked")
	}))
	defer offAllowlist.Close()
	// httptest binds 127.0.0.1. The policy matches by HOST, so the off-allowlist
	// origin must be addressed by a different hostname to be genuinely off-list;
	// "localhost" resolves to the same interface but is a distinct host string.
	offAllowlistURL := asLocalhost(offAllowlist.URL)

	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!doctype html><html><body>
			<p>contained</p>
			<script>
			  window.__results = {};
			  fetch(%q).then(function(r){ return r.text(); })
			    .then(function(t){ window.__results.fetch = 'reached:' + t; })
			    .catch(function(e){ window.__results.fetch = 'blocked'; });
			</script>
			</body></html>`, offAllowlistURL+"/exfil")
	}))
	defer allowed.Close()

	m := newHeadlessManager(t)
	// Allowlist the page's own host only. httptest serves on 127.0.0.1.
	m.SetNavigationPolicy(&navpolicy.Policy{Allowed: []string{hostOfTestURL(t, allowed.URL)}})

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	var id target.ID
	if err := m.runBrowser(ctx, func(rc context.Context) error {
		var e error
		id, e = target.CreateTarget("about:blank").Do(rc)
		return e
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	tabID := string(id)
	m.refs.SetActive(tabID)
	// Touching the tab context is what arms containment.
	if _, err := m.tabContext(tabID); err != nil {
		t.Fatalf("tab context: %v", err)
	}

	if _, err := m.NavigateTo(ctx, allowed.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := m.WaitFor(ctx, "fn:window.__results && window.__results.fetch !== undefined", 10*time.Second); err != nil {
		t.Fatalf("page never recorded a fetch outcome: %v", err)
	}

	value, err := m.Evaluate(ctx, `window.__results.fetch`)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if got, _ := value.(string); got != "blocked" {
		t.Errorf("page fetch to an off-allowlist host = %q, want \"blocked\"", got)
	}
	if offAllowlistHits != 0 {
		t.Errorf("off-allowlist origin was reached %d times; containment must stop the request leaving", offAllowlistHits)
	}

	blocked := m.BlockedRequests(tabID)
	if len(blocked) == 0 {
		t.Fatal("containment must record what it refused; silent blocking is an unexplained broken page")
	}
	found := false
	for _, b := range blocked {
		if strings.Contains(b.URL, "/exfil") {
			found = true
			if !strings.Contains(b.Reason, "allowlist") {
				t.Errorf("blocked reason = %q, want it to name the allowlist", b.Reason)
			}
		}
	}
	if !found {
		t.Errorf("blocked list %+v does not mention the exfil request", blocked)
	}
}

// A WebSocket is the most direct way for a contained page to reach a host the
// allowlist excludes, and navpolicy's navigation rules do not gate ws:/wss:.
func TestContainmentBlocksOffAllowlistWebSocket(t *testing.T) {
	var upgrades int
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			upgrades++
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer wsServer.Close()

	// Same reason as above: a distinct hostname is what puts it off the allowlist.
	wsURL := "ws://" + strings.TrimPrefix(asLocalhost(wsServer.URL), "http://") + "/socket"
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!doctype html><html><body><script>
			window.__ws = 'pending';
			try {
			  var s = new WebSocket(%q);
			  s.onopen = function(){ window.__ws = 'open'; };
			  s.onerror = function(){ window.__ws = 'error'; };
			} catch (e) { window.__ws = 'error'; }
			</script></body></html>`, wsURL)
	}))
	defer allowed.Close()

	m := newHeadlessManager(t)
	m.SetNavigationPolicy(&navpolicy.Policy{Allowed: []string{hostOfTestURL(t, allowed.URL)}})

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	var id target.ID
	if err := m.runBrowser(ctx, func(rc context.Context) error {
		var e error
		id, e = target.CreateTarget("about:blank").Do(rc)
		return e
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	m.refs.SetActive(string(id))
	if _, err := m.tabContext(string(id)); err != nil {
		t.Fatalf("tab context: %v", err)
	}
	if _, err := m.NavigateTo(ctx, allowed.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := m.WaitFor(ctx, "fn:window.__ws !== 'pending'", 10*time.Second); err != nil {
		t.Fatalf("websocket never settled: %v", err)
	}
	value, err := m.Evaluate(ctx, `window.__ws`)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if got, _ := value.(string); got == "open" {
		t.Error("an off-allowlist WebSocket connected; containment must refuse it")
	}
	if upgrades != 0 {
		t.Errorf("off-allowlist server saw %d websocket upgrades, want 0", upgrades)
	}
}

// Containment must not fire at all when no policy is configured: it costs a
// Fetch round trip per request and the default posture is unrestricted.
func TestContainmentIsInertWithoutAPolicy(t *testing.T) {
	var hits int
	third := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fmt.Fprint(w, "ok")
	}))
	defer third.Close()
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><script>
			window.__r='pending';
			fetch(%q).then(function(){window.__r='reached';}).catch(function(){window.__r='blocked';});
			</script></body></html>`, third.URL+"/ping")
	}))
	defer page.Close()

	m := newHeadlessManager(t) // no SetNavigationPolicy
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	var id target.ID
	if err := m.runBrowser(ctx, func(rc context.Context) error {
		var e error
		id, e = target.CreateTarget("about:blank").Do(rc)
		return e
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	m.refs.SetActive(string(id))
	if _, err := m.tabContext(string(id)); err != nil {
		t.Fatalf("tab context: %v", err)
	}
	if _, err := m.NavigateTo(ctx, page.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := m.WaitFor(ctx, "fn:window.__r !== 'pending'", 10*time.Second); err != nil {
		t.Fatalf("fetch never settled: %v", err)
	}
	value, _ := m.Evaluate(ctx, `window.__r`)
	if got, _ := value.(string); got != "reached" {
		t.Errorf("without a policy a cross-origin fetch should succeed, got %q", got)
	}
	if hits == 0 {
		t.Error("third-party origin should have been reached with no policy set")
	}
}

// asLocalhost rewrites a 127.0.0.1 httptest URL to the equivalent localhost URL:
// the same listener, a different host string for policy purposes.
func asLocalhost(raw string) string {
	return strings.Replace(raw, "127.0.0.1", "localhost", 1)
}

func hostOfTestURL(t *testing.T, raw string) string {
	t.Helper()
	trimmed := strings.TrimPrefix(raw, "http://")
	host := trimmed
	if i := strings.Index(trimmed, ":"); i >= 0 {
		host = trimmed[:i]
	}
	return host
}

var _ = time.Second

// Over-blocking is as much a failure as under-blocking: an allowlisted page
// must still be able to use its own WebSocket, EventSource and beacons.
func TestContainmentPermitsAllowlistedChannels(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/socket", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		time.Sleep(200 * time.Millisecond)
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprint(w, "data: hello\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(300 * time.Millisecond)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!doctype html><html><body><script>
			window.__ws = 'pending'; window.__es = 'pending';
			try {
			  var s = new WebSocket('ws://' + location.host + '/socket');
			  s.onopen = function(){ window.__ws = 'open'; };
			  s.onerror = function(){ window.__ws = 'error'; };
			} catch (e) { window.__ws = 'threw'; }
			try {
			  var es = new EventSource('/events');
			  es.onmessage = function(){ window.__es = 'message'; es.close(); };
			  es.onerror = function(){ if (window.__es === 'pending') window.__es = 'error'; };
			} catch (e) { window.__es = 'threw'; }
			</script></body></html>`)
	})
	allowed := httptest.NewServer(mux)
	defer allowed.Close()

	m := newHeadlessManager(t)
	m.SetNavigationPolicy(&navpolicy.Policy{Allowed: []string{hostOfTestURL(t, allowed.URL)}})

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	var id target.ID
	if err := m.runBrowser(ctx, func(rc context.Context) error {
		var e error
		id, e = target.CreateTarget("about:blank").Do(rc)
		return e
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	m.refs.SetActive(string(id))
	if _, err := m.tabContext(string(id)); err != nil {
		t.Fatalf("tab context: %v", err)
	}
	if _, err := m.NavigateTo(ctx, allowed.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := m.WaitFor(ctx, "fn:window.__ws !== 'pending' && window.__es !== 'pending'", 15*time.Second); err != nil {
		t.Fatalf("channels never settled: %v", err)
	}
	ws, _ := m.Evaluate(ctx, `window.__ws`)
	if got, _ := ws.(string); got != "open" {
		t.Errorf("allowlisted WebSocket = %q, want \"open\" (containment must not over-block)", got)
	}
	es, _ := m.Evaluate(ctx, `window.__es`)
	if got, _ := es.(string); got != "message" {
		t.Errorf("allowlisted EventSource = %q, want \"message\"", got)
	}
}

// WebRTC has no URL to check against the allowlist, so containment closes it
// rather than filtering it.
func TestContainmentDisablesWebRTC(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><script>
			try { new RTCPeerConnection(); window.__rtc = 'constructed'; }
			catch (e) { window.__rtc = 'refused'; }
			</script></body></html>`)
	}))
	defer page.Close()

	m := newHeadlessManager(t)
	m.SetNavigationPolicy(&navpolicy.Policy{Allowed: []string{hostOfTestURL(t, page.URL)}})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	var id target.ID
	if err := m.runBrowser(ctx, func(rc context.Context) error {
		var e error
		id, e = target.CreateTarget("about:blank").Do(rc)
		return e
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	m.refs.SetActive(string(id))
	if _, err := m.tabContext(string(id)); err != nil {
		t.Fatalf("tab context: %v", err)
	}
	if _, err := m.NavigateTo(ctx, page.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := m.WaitFor(ctx, "fn:window.__rtc !== undefined", 10*time.Second); err != nil {
		t.Fatalf("rtc probe never ran: %v", err)
	}
	value, _ := m.Evaluate(ctx, `window.__rtc`)
	if got, _ := value.(string); got != "refused" {
		t.Errorf("RTCPeerConnection = %q, want \"refused\" under containment", got)
	}
}
