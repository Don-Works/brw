package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/extensionbridge"
	httpapi "github.com/Don-Works/brw/internal/http"
	"github.com/Don-Works/brw/internal/profilepolicy"
)

// daemonHealth and bridgeStatus are hand-written copies of two documents other
// packages produce. Every other test here encodes its fixture with those same
// structs, so it is self-consistent by construction: rename a field in
// internal/http or internal/extensionbridge and they all stay green while
// `brwctl upgrade` silently stops refusing on a busy daemon (an in_flight that
// no longer decodes reads as 0) and doctor stops seeing a stale extension
// build. These two tests decode the real producers instead.

func TestDaemonHealthContract(t *testing.T) {
	identity := brwidentity.Identity{Workspace: fixtureWorkspace, Profile: fixtureProfile}
	addr := freeLoopbackAddr(t)
	server := httpapi.NewWithIdentity(addr, nil, identity)
	go func() { _ = server.ListenAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})

	url := "http://" + addr
	client := &http.Client{Timeout: 2 * time.Second}
	var health daemonHealth
	waitFor(t, "the daemon to answer /health", func() bool {
		var err error
		health, err = probeDaemonHealth(client, url)
		return err == nil
	})

	if !health.OK {
		t.Fatal("/health did not decode as ok; probeDaemonHealth reads that field to tell brwd from a port collision")
	}
	if health.Identity != identity {
		t.Fatalf("identity = %+v, want %+v; doctor uses it to catch another workspace's daemon on this port", health.Identity, identity)
	}
	// The counters are zero on an idle daemon, so presence in the document is
	// what pins the names upgrade's busy check depends on.
	raw := fetchRaw(t, client, url+"/health")
	leases, ok := raw["tab_leases"].(map[string]any)
	if !ok {
		t.Fatalf("/health has no tab_leases object: %v", raw)
	}
	for _, key := range []string{"active_tabs", "owners", "in_flight"} {
		if _, present := leases[key]; !present {
			t.Fatalf("/health tab_leases lost the %q key: %v", key, leases)
		}
	}
}

func TestBridgeStatusContract(t *testing.T) {
	const build = "1.4.0"
	addr := freeLoopbackAddr(t)
	bridge := extensionbridge.New(addr, 5*time.Second, "")
	go func() { _ = bridge.ListenAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = bridge.Shutdown(ctx)
	})

	client := &http.Client{Timeout: 2 * time.Second}
	waitFor(t, "the bridge to answer /status", func() bool {
		_, err := probeBridgeStatus(client, addr)
		return err == nil
	})

	conn := dialBridge(t, addr, build)
	var status bridgeStatus
	waitFor(t, "the extension hello to be recorded", func() bool {
		var err error
		status, err = probeBridgeStatus(client, addr)
		return err == nil && status.Connected && status.Hello.Build != ""
	})
	if status.Hello.Build != build {
		t.Fatalf("hello.build = %q, want %q; doctor compares it against the payload on disk", status.Hello.Build, build)
	}
	if status.ConnectedAt == "" {
		t.Fatal("connected_at is empty on a live connection")
	}

	_ = conn.Close(websocket.StatusNormalClosure, "done")
	waitFor(t, "the bridge to report the disconnect", func() bool {
		var err error
		status, err = probeBridgeStatus(client, addr)
		return err == nil && !status.Connected && status.DisconnectReason != ""
	})

	// Zero on an idle bridge, so presence is what pins the names.
	raw := fetchRaw(t, client, "http://"+addr+"/status")
	for _, key := range []string{"connected", "hello", "connected_at", "disconnect_reason", "pending", "inflight", "queued"} {
		if _, present := raw[key]; !present {
			t.Fatalf("/status lost the %q key: %v", key, raw)
		}
	}
}

// dialBridge connects as the extension does: a chrome-extension Origin the
// bridge accepts, and a hello frame naming the loaded build.
func dialBridge(t *testing.T, addr, build string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+addr+"/extension", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"chrome-extension://" + profilepolicy.DefaultBridgeExtensionID}},
	})
	if err != nil {
		t.Fatalf("dial the bridge: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	hello, err := json.Marshal(map[string]any{
		"type":  "hello",
		"hello": map[string]any{"source": "brw-extension", "build": build, "chrome": fixtureChromeVersion},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	return conn
}

func fetchRaw(t *testing.T, client *http.Client, url string) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := fetchJSON(client, url, &raw); err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return raw
}

// freeLoopbackAddr reserves a loopback port and hands it back. Both producers
// bind an address given at construction, so a test cannot ask them for the port
// a :0 bind chose.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
