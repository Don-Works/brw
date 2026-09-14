package httpclient

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	httpapi "github.com/Don-Works/brw/internal/http"
	"github.com/Don-Works/brw/internal/sessionstate"
)

// Fabricated fixture material, low entropy on purpose.
const (
	fixtureWireKey         = "fixture-session-state-key-for-tests-0001"
	fixtureWireCookieName  = "fixture-session-cookie-one"
	fixtureWireCookieValue = "fixture-session-value-one"
)

// recordingTransport keeps every request and response body that crossed the
// proxy boundary, so a test can assert on the actual bytes rather than on the
// decoded struct — the struct is exactly what a future field addition would
// change without anyone noticing.
type recordingTransport struct {
	inner  http.RoundTripper
	bodies [][]byte
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		sent, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		r.bodies = append(r.bodies, sent)
		req.Body = io.NopCloser(bytes.NewReader(sent))
		req.ContentLength = int64(len(sent))
	}
	resp, err := r.inner.RoundTrip(req)
	if err != nil || resp.Body == nil {
		return resp, err
	}
	received, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	r.bodies = append(r.bodies, received)
	resp.Body = io.NopCloser(bytes.NewReader(received))
	return resp, nil
}

// The claim in docs/auth-model.md is that an --upstream-http session forwards
// the COMMAND and never the session material. This runs the real proxy client
// against the real daemon HTTP handler with a real sealed snapshot behind it,
// and reads every byte that crossed.
func TestSessionStateNeverPutsCookieMaterialOnTheWire(t *testing.T) {
	store, err := sessionstate.NewStore(sessionstate.Config{
		Root: filepath.Join(t.TempDir(), "state"),
		Key:  []byte(fixtureWireKey),
	})
	if err != nil {
		t.Fatalf("sessionstate.NewStore: %v", err)
	}
	meta, err := store.Save(sessionstate.Snapshot{
		Origins: []string{"https://app.example.test"},
		Cookies: []sessionstate.Cookie{{
			Name: fixtureWireCookieName, Value: fixtureWireCookieValue,
			Domain: "app.example.test", Path: "/",
		}},
	}, sessionstate.SaveOptions{})
	if err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	// A Manager with no browser still answers the metadata verbs, which is all
	// this test needs: the point is the wire, not the cookie jar.
	manager := &browser.Manager{}
	manager.SetSessionStateStore(store)

	addr := freeLoopbackAddr(t)
	server := httpapi.New(addr, manager)
	go func() { _ = server.ListenAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})

	client, err := New("http://"+addr, 5*time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	capture := &recordingTransport{inner: http.DefaultTransport}
	client.client.Transport = capture
	waitForDaemon(t, "http://"+addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	listed, err := client.SessionState(ctx, browser.SessionStateOptions{Action: browser.SessionStateActionList})
	if err != nil {
		t.Fatalf("list over the proxy: %v", err)
	}
	if len(listed.Snapshots) != 1 || listed.Snapshots[0].ID != meta.ID {
		t.Fatalf("list = %+v, want the one seeded snapshot", listed.Snapshots)
	}
	if listed.Snapshots[0].CookieCount != 1 {
		t.Fatalf("cookie_count = %d, want 1 — the count is what a caller gets instead of the cookies", listed.Snapshots[0].CookieCount)
	}

	// A failing restore must not leak material through the error path either.
	if _, err := client.SessionState(ctx, browser.SessionStateOptions{
		Action:     browser.SessionStateActionRestore,
		SnapshotID: "st_00000000000000000000000000000000",
	}); err == nil {
		t.Fatal("restoring an unknown snapshot id must fail")
	}

	if len(capture.bodies) < 4 {
		t.Fatalf("captured %d bodies, want at least the two request/response pairs", len(capture.bodies))
	}
	// The origin is deliberately NOT in this list: the caller named it on the
	// way in, so echoing it back discloses nothing it did not already have.
	// The cookie name and value are the material, and neither may appear.
	for i, body := range capture.bodies {
		for _, secret := range []string{fixtureWireCookieValue, fixtureWireCookieName} {
			if bytes.Contains(body, []byte(secret)) {
				t.Fatalf("body %d crossed the proxy boundary carrying %q: %s", i, secret, body)
			}
		}
	}
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return addr
}

func waitForDaemon(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the test daemon never answered /health")
}

// A controller that cannot do session state at all has to say so by name,
// rather than 404 or return an empty result the caller reads as "no snapshots".
func TestDaemonWithoutTheCapabilityRefusesStateByName(t *testing.T) {
	addr := freeLoopbackAddr(t)
	server := httpapi.New(addr, stubController{})
	go func() { _ = server.ListenAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	client, err := New("http://"+addr, 5*time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	waitForDaemon(t, "http://"+addr)
	_, err = client.SessionState(context.Background(), browser.SessionStateOptions{Action: browser.SessionStateActionList})
	if err == nil || !strings.Contains(err.Error(), "does not support session snapshots") {
		t.Fatalf("error = %v, want a named capability refusal", err)
	}
}

// stubController satisfies browser.Controller by embedding the interface and
// implements nothing else, so the daemon sees a transport with no session-state
// capability. Every method it does not define would panic if called, which is
// the correct outcome: this test must never reach one.
type stubController struct{ browser.Controller }
