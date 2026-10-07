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

var allRoutePattern = regexp.MustCompile(`mux\.HandleFunc\("[A-Z]+ ([^"]*)"`)

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

func TestIdleDaemonExitsAfterItsWindow(t *testing.T) {
	server := New("", &fakeController{})
	server.SetIdleExit(150 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !server.WatchIdle(ctx) {
		t.Fatal("an armed daemon never reported itself idle")
	}
}

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

	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !server.WatchIdle(ctx) {
		t.Fatal("the daemon never went idle after the traffic stopped")
	}
}

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

	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !server.WatchIdle(ctx) {
		t.Fatal("the daemon never went idle after the long call finished")
	}
}

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

	if !countsAsActivity("/api/page/some_route_added_next_week") {
		t.Error("a route nobody has classified does not count as work, so a daemon could exit while it is being used")
	}
}

func TestUseThatDidNotArriveOverHTTPStillCounts(t *testing.T) {
	server := New("", &fakeController{})
	server.SetIdleExit(100 * time.Millisecond)

	done := server.NoteActivity()

	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		if idle, armed := server.idle.idleFor(time.Now()); !armed || idle != 0 {
			t.Fatalf("work reported through NoteActivity showed %s of idleness (armed=%t)", idle, armed)
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	if server.WatchIdle(ctx) {
		cancel()
		t.Fatal("the daemon exited while an MCP call was still running")
	}
	cancel()

	done()
	done()

	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !server.WatchIdle(ctx) {
		t.Fatal("the daemon never went idle after the MCP call finished")
	}
}
