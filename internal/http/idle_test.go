package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"
	"time"
)

// allRoutePattern reads every route the daemon registers, not only the /api/
// ones: the idle classification has to have an answer for /health and the
// dashboard too.
var allRoutePattern = regexp.MustCompile(`mux\.HandleFunc\("[A-Z]+ ([^"]*)"`)

// TestPersistentDaemonNeverGoesIdle pins the default. The daemon brwctl
// installs as a background service must outlive every idle window there is; an
// idle exit that turned itself on would be an agent returning to a daemon that
// quietly stopped.
func TestPersistentDaemonNeverGoesIdle(t *testing.T) {
	server := New("", &fakeController{})
	if server.IdleExit() != 0 {
		t.Fatalf("a new daemon has an idle window of %s, want none", server.IdleExit())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if server.WatchIdle(ctx) {
		t.Fatal("a persistent daemon reported itself idle")
	}
}

// TestIdleDaemonExitsAfterItsWindow is the feature: a daemon started for one job
// lets go of the port and the browser when the job is over.
func TestIdleDaemonExitsAfterItsWindow(t *testing.T) {
	server := New("", &fakeController{})
	server.SetIdleExit(150 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !server.WatchIdle(ctx) {
		t.Fatal("an armed daemon never reported itself idle")
	}
}

// TestWorkPostponesTheIdleExit: the window measures silence, not uptime.
func TestWorkPostponesTheIdleExit(t *testing.T) {
	server := New("", &fakeController{})
	server.SetIdleExit(300 * time.Millisecond)

	stop := make(chan struct{})
	busy := make(chan struct{})
	go func() {
		defer close(busy)
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				rec := httptest.NewRecorder()
				server.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/browser/tabs", nil))
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	if server.WatchIdle(ctx) {
		cancel()
		close(stop)
		<-busy
		t.Fatal("a daemon serving a request every 50ms was declared idle at 300ms")
	}
	cancel()
	close(stop)
	<-busy

	// And once the traffic stops, it does go idle.
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !server.WatchIdle(ctx) {
		t.Fatal("the daemon never went idle after the traffic stopped")
	}
}

// TestALongCallIsNotSilence: a recipe run takes minutes and sends nothing while
// it runs. Measuring from the last request START would have exited underneath
// one.
func TestALongCallIsNotSilence(t *testing.T) {
	server := New("", &fakeController{})
	server.SetIdleExit(100 * time.Millisecond)

	release := make(chan struct{})
	entered := make(chan struct{})
	done := make(chan struct{})
	handler := server.idleMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/recipes/run", nil))
	}()
	<-entered

	// Hold the call open past four idle windows.
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		if idle, armed := server.idle.idleFor(time.Now()); !armed || idle != 0 {
			close(release)
			<-done
			t.Fatalf("a call still in flight reported %s of idleness (armed=%t)", idle, armed)
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	if server.WatchIdle(ctx) {
		cancel()
		close(release)
		<-done
		t.Fatal("the daemon exited while a call was still running")
	}
	cancel()
	close(release)
	<-done

	// And when the call finishes, the window starts from there.
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !server.WatchIdle(ctx) {
		t.Fatal("the daemon never went idle after the long call finished")
	}
}

// TestHealthPollsDoNotKeepAnAbandonedDaemonAlive is the reason the exemption
// table exists. A supervisor polling /health every thirty seconds would
// otherwise pin an abandoned daemon forever, which is the exact state the idle
// exit is for.
func TestHealthPollsDoNotKeepAnAbandonedDaemonAlive(t *testing.T) {
	server := New("", &fakeController{})
	server.SetIdleExit(200 * time.Millisecond)

	stop := make(chan struct{})
	polling := make(chan struct{})
	go func() {
		defer close(polling)
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				rec := httptest.NewRecorder()
				server.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
			}
		}
	}()
	defer func() {
		close(stop)
		<-polling
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !server.WatchIdle(ctx) {
		t.Fatal("health polling kept an otherwise idle daemon alive")
	}
}

// TestEveryRouteIsClassifiedForIdleActivity enumerates the route table against
// the exemption list. The question "does this count as somebody using the
// daemon?" has to have an answer for every route the daemon serves, and the
// default has to be yes: a route added later that is exempt by accident would
// let a daemon exit while it is being used.
func TestEveryRouteIsClassifiedForIdleActivity(t *testing.T) {
	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read the route table: %v", err)
	}
	matches := allRoutePattern.FindAllStringSubmatch(string(source), -1)
	if len(matches) < 30 {
		t.Fatalf("parsed only %d routes; the registration shape this test reads has changed", len(matches))
	}
	exemptSeen := map[string]bool{}
	for _, match := range matches {
		path := match[1]
		activity := countsAsActivity(path)
		if !activity {
			exemptSeen[path] = true
			if idleExemptPrefixes[path] == "" {
				t.Errorf("%s is exempt from idle activity but no reason is recorded for it", path)
			}
			continue
		}
	}
	for prefix, reason := range idleExemptPrefixes {
		if !exemptSeen[prefix] {
			t.Errorf("idleExemptPrefixes lists %s (%q), which is not a route the daemon serves", prefix, reason)
		}
	}
	// The default direction, stated as a fact rather than assumed: an unknown
	// path counts as work.
	if !countsAsActivity("/api/page/some_route_added_next_week") {
		t.Error("a route nobody has classified does not count as work, so a daemon could exit while it is being used")
	}
}
