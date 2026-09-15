package cdp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeActivePort plants the file Chrome writes into its own profile.
func writeActivePort(t *testing.T, dir string, port int, extra string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strconv.Itoa(port) + "\n" + extra
	if err := os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAutoConnectPrefersTheBrowsersOwnPortFile(t *testing.T) {
	dir := writeActivePort(t, filepath.Join(t.TempDir(), "profile"), 41111, "/devtools/browser/0000-0000\n")
	probed := map[int]bool{}
	endpoint, err := AutoConnect(context.Background(), AutoConnectOptions{
		UserDataDirs: []string{dir},
		Ports:        []int{9222},
		Probe: func(_ context.Context, port int) (string, error) {
			probed[port] = true
			if port == 41111 || port == 9222 {
				return "Chrome/141.0.0.0", nil
			}
			return "", errors.New("closed")
		},
	})
	if err != nil {
		t.Fatalf("AutoConnect: %v", err)
	}
	if endpoint.Port != 41111 {
		t.Fatalf("attached to port %d, want the one the browser wrote into its own profile", endpoint.Port)
	}
	if endpoint.Source != SourceActivePortFile || endpoint.From != dir {
		t.Fatalf("endpoint = %+v, want the file source and the directory it came from", endpoint)
	}
	if endpoint.URL != "http://127.0.0.1:41111" {
		t.Fatalf("URL = %q", endpoint.URL)
	}
	if probed[9222] {
		// The conventional port is a guess. Probing it when the precise answer
		// already worked would mean brw could attach to a browser nobody named.
		t.Error("the conventional port was probed even though the profile named one that answered")
	}
}

func TestAutoConnectFallsBackToProbingPortsInOrder(t *testing.T) {
	var order []int
	endpoint, err := AutoConnect(context.Background(), AutoConnectOptions{
		UserDataDirs: []string{filepath.Join(t.TempDir(), "no-such-profile")},
		Ports:        []int{9222, 9223, 9224},
		Probe: func(_ context.Context, port int) (string, error) {
			order = append(order, port)
			if port == 9223 {
				return "Chromium/141.0.0.0", nil
			}
			return "", errors.New("connection refused")
		},
	})
	if err != nil {
		t.Fatalf("AutoConnect: %v", err)
	}
	if endpoint.Port != 9223 || endpoint.Source != SourcePortProbe || endpoint.Browser != "Chromium/141.0.0.0" {
		t.Fatalf("endpoint = %+v", endpoint)
	}
	if len(order) != 2 || order[0] != 9222 || order[1] != 9223 {
		t.Fatalf("probed %v, want the list in order, stopping at the first that answered", order)
	}
}

// TestAutoConnectSkipsAStaleActivePortFile: the file outlives the browser that
// wrote it, so the port in it is a claim to verify rather than an answer.
func TestAutoConnectSkipsAStaleActivePortFile(t *testing.T) {
	stale := writeActivePort(t, filepath.Join(t.TempDir(), "stale"), 41111, "")
	live := writeActivePort(t, filepath.Join(t.TempDir(), "live"), 41222, "")
	endpoint, err := AutoConnect(context.Background(), AutoConnectOptions{
		UserDataDirs: []string{stale, live},
		Ports:        []int{},
		Probe: func(_ context.Context, port int) (string, error) {
			if port == 41222 {
				return "Chrome/141.0.0.0", nil
			}
			return "", errors.New("connection refused")
		},
	})
	if err != nil {
		t.Fatalf("AutoConnect: %v", err)
	}
	if endpoint.Port != 41222 || endpoint.From != live {
		t.Fatalf("endpoint = %+v, want the profile whose browser is still running", endpoint)
	}
}

// TestAutoConnectNamesWhereItLookedWhenItFindsNothing. "no endpoint found" with
// no list is a message an operator cannot act on.
func TestAutoConnectNamesWhereItLookedWhenItFindsNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	_, err := AutoConnect(context.Background(), AutoConnectOptions{
		UserDataDirs: []string{dir},
		Ports:        []int{9222, 9223},
		Probe:        func(context.Context, int) (string, error) { return "", errors.New("connection refused") },
	})
	if !errors.Is(err, ErrNoEndpoint) {
		t.Fatalf("err = %v, want ErrNoEndpoint", err)
	}
	for _, want := range []string{filepath.Join(dir, "DevToolsActivePort"), "127.0.0.1:9222", "127.0.0.1:9223", "--remote"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// TestProbeRefusesSomethingThatIsNotABrowser is the guard that keeps discovery
// from handing brw a debugger session with whatever happens to be on 9222. The
// check is the DevTools handshake, not the port number.
func TestProbeRefusesSomethingThatIsNotABrowser(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantOK  bool
	}{
		{
			name: "a real devtools endpoint",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/json/version" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				fmt.Fprint(w, `{"Browser":"Chrome/141.0.0.0","webSocketDebuggerUrl":"ws://127.0.0.1/devtools/browser/x"}`)
			},
			wantOK: true,
		},
		{
			name: "some other JSON service",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"status":"ok","service":"something else"}`)
			},
		},
		{
			name: "a devtools-shaped answer with no debugger url",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"Browser":"Chrome/141.0.0.0"}`)
			},
		},
		{
			name:    "an HTML page",
			handler: func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "<html>hello</html>") },
		},
		{
			name:    "a server that refuses",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			port := serverPort(t, server)

			browser, err := ProbeEndpoint(context.Background(), port)
			if tc.wantOK {
				if err != nil || browser == "" {
					t.Fatalf("a real endpoint was rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("attached to %s as though it were a browser (%q)", tc.name, browser)
			}
		})
	}
}

// TestAutoConnectDrivesARealListener joins the two halves: a listener that
// answers like DevTools is found by the real probe, through the real discovery.
func TestAutoConnectDrivesARealListener(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"Browser":"Chrome/141.0.0.0","webSocketDebuggerUrl":"ws://127.0.0.1/devtools/browser/x"}`)
	}))
	defer server.Close()
	port := serverPort(t, server)
	dir := writeActivePort(t, filepath.Join(t.TempDir(), "profile"), port, "/devtools/browser/x\n")

	endpoint, err := AutoConnect(context.Background(), AutoConnectOptions{UserDataDirs: []string{dir}, Ports: []int{}})
	if err != nil {
		t.Fatalf("AutoConnect: %v", err)
	}
	if endpoint.Port != port || endpoint.Browser != "Chrome/141.0.0.0" {
		t.Fatalf("endpoint = %+v", endpoint)
	}
}

func TestIsAutoConnect(t *testing.T) {
	for _, value := range []string{"auto", "AUTO", " auto "} {
		if !IsAutoConnect(value) {
			t.Errorf("IsAutoConnect(%q) = false", value)
		}
	}
	for _, value := range []string{"", "http://127.0.0.1:9222", "automatic"} {
		if IsAutoConnect(value) {
			t.Errorf("IsAutoConnect(%q) = true", value)
		}
	}
}

func serverPort(t *testing.T, server *httptest.Server) int {
	t.Helper()
	_, portText, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return port
}
