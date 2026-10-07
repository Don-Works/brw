package chromeoptin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func fakeChrome(t *testing.T, version func(port int) map[string]any) (dir string, port int) {
	t.Helper()
	var doc map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	port = serverPort(t, srv)
	doc = version(port)
	dir = t.TempDir()
	writeActivePort(t, dir, strconv.Itoa(port)+"\n/devtools/browser/fake-browser-id\n")
	return dir, port
}

func chrome144(port int) map[string]any {
	return map[string]any{
		"Browser":              "Chrome/144.0.7000.0",
		"webSocketDebuggerUrl": browserWS(port),
	}
}

func serverPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	_, portText, ok := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	if !ok {
		t.Fatalf("cannot read a port out of %s", srv.URL)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("port %q: %v", portText, err)
	}
	return port
}

func writeActivePort(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, activePortFile), []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", activePortFile, err)
	}
}

func browserWS(port int) string {
	return fmt.Sprintf("ws://127.0.0.1:%d/devtools/browser/fake-browser-id", port)
}

func TestDiscoverReturnsTheBrowserTarget(t *testing.T) {
	dir, port := fakeChrome(t, chrome144)
	got, err := Discover(context.Background(), Options{UserDataDir: dir})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.Port != port {
		t.Fatalf("port = %d, want the one %s named (%d)", got.Port, activePortFile, port)
	}
	if got.HTTPURL != fmt.Sprintf("http://127.0.0.1:%d", port) {
		t.Fatalf("HTTPURL = %q", got.HTTPURL)
	}
	if got.BrowserWSURL != browserWS(port) {
		t.Fatalf("BrowserWSURL = %q", got.BrowserWSURL)
	}
	if got.Major != 144 {
		t.Fatalf("Major = %d, want 144", got.Major)
	}
	if got.UserDataDir != dir {
		t.Fatalf("UserDataDir = %q, want %q", got.UserDataDir, dir)
	}
}

func TestDiscoverRefusalsNameTheUserAction(t *testing.T) {

	notChrome := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello from something else"))
	}))
	t.Cleanup(notChrome.Close)
	otherPort := serverPort(t, notChrome)

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadPort := serverPort(t, dead)
	dead.Close()

	for _, tc := range []struct {
		name    string
		dir     func(t *testing.T) string
		wantErr error
	}{
		{
			name:    "no file at all, which is the shipped default",
			dir:     func(t *testing.T) string { return t.TempDir() },
			wantErr: ErrOptInOff,
		},
		{
			name: "file whose first line is not a port",
			dir: func(t *testing.T) string {
				dir := t.TempDir()
				writeActivePort(t, dir, "not-a-port\n/devtools/browser/x\n")
				return dir
			},
			wantErr: ErrOptInOff,
		},
		{
			name: "file naming a port nothing is listening on",
			dir: func(t *testing.T) string {
				dir := t.TempDir()
				writeActivePort(t, dir, strconv.Itoa(deadPort)+"\n/devtools/browser/x\n")
				return dir
			},
			wantErr: ErrOptInOff,
		},
		{
			name: "port taken over by something that is not a DevTools endpoint",
			dir: func(t *testing.T) string {
				dir := t.TempDir()
				writeActivePort(t, dir, strconv.Itoa(otherPort)+"\n/devtools/browser/x\n")
				return dir
			},
			wantErr: ErrOptInOff,
		},
		{
			name: "endpoint exposing no browser target",
			dir: func(t *testing.T) string {
				dir, _ := fakeChrome(t, func(int) map[string]any {
					return map[string]any{"Browser": "Chrome/144.0.7000.0"}
				})
				return dir
			},
			wantErr: ErrOptInOff,
		},
		{

			name: "browser target on a different port",
			dir: func(t *testing.T) string {
				dir, _ := fakeChrome(t, func(int) map[string]any {
					return map[string]any{
						"Browser":              "Chrome/144.0.7000.0",
						"webSocketDebuggerUrl": browserWS(otherPort),
					}
				})
				return dir
			},
			wantErr: ErrOptInOff,
		},
		{
			name: "browser target off this machine",
			dir: func(t *testing.T) string {
				dir, _ := fakeChrome(t, func(port int) map[string]any {
					return map[string]any{
						"Browser":              "Chrome/144.0.7000.0",
						"webSocketDebuggerUrl": fmt.Sprintf("ws://browser.invalid:%d/devtools/browser/x", port),
					}
				})
				return dir
			},
			wantErr: ErrOptInOff,
		},
		{
			name: "browser target that is not a websocket url",
			dir: func(t *testing.T) string {
				dir, _ := fakeChrome(t, func(port int) map[string]any {
					return map[string]any{
						"Browser":              "Chrome/144.0.7000.0",
						"webSocketDebuggerUrl": fmt.Sprintf("http://127.0.0.1:%d/devtools/browser/x", port),
					}
				})
				return dir
			},
			wantErr: ErrOptInOff,
		},
		{
			name: "chrome older than the opt-in",
			dir: func(t *testing.T) string {
				dir, _ := fakeChrome(t, func(port int) map[string]any {
					return map[string]any{
						"Browser":              "Chrome/143.0.6000.0",
						"webSocketDebuggerUrl": browserWS(port),
					}
				})
				return dir
			},
			wantErr: ErrChromeTooOld,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Discover(context.Background(), Options{UserDataDir: tc.dir(t)})
			if err == nil {
				t.Fatal("discovery succeeded")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error %v does not wrap %v", err, tc.wantErr)
			}

			want := "chrome://inspect"
			if errors.Is(err, ErrChromeTooOld) {
				want = "Chrome 144 or newer"
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not name the action (%q)", err, want)
			}
		})
	}
}

func TestDiscoverVersionBoundary(t *testing.T) {
	for _, tc := range []struct {
		browser string
		wantOK  bool
	}{
		{"Chrome/143.0.6000.0", false},
		{"Chrome/144.0.7000.0", true},
		{"Chrome/153.0.8010.37", true},

		{"SomeFork", true},
	} {
		t.Run(tc.browser, func(t *testing.T) {
			dir, _ := fakeChrome(t, func(port int) map[string]any {
				return map[string]any{
					"Browser":              tc.browser,
					"webSocketDebuggerUrl": browserWS(port),
				}
			})
			_, err := Discover(context.Background(), Options{UserDataDir: dir})
			if tc.wantOK && err != nil {
				t.Fatalf("Discover(%s) = %v, want success", tc.browser, err)
			}
			if !tc.wantOK && !errors.Is(err, ErrChromeTooOld) {
				t.Fatalf("Discover(%s) = %v, want ErrChromeTooOld", tc.browser, err)
			}
		})
	}
}

func TestDiscoverWritesNothing(t *testing.T) {
	dir, _ := fakeChrome(t, chrome144)
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if _, err := Discover(context.Background(), Options{UserDataDir: dir}); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(before) != len(after) {
		t.Fatalf("discovery changed the user data directory: %d entries before, %d after", len(before), len(after))
	}
}

func TestDefaultUserDataDirMatchesTheSetupTable(t *testing.T) {
	for _, tc := range []struct {
		goos    string
		browser string
		want    string
	}{
		{"darwin", "chrome", "Library/Application Support/Google/Chrome"},
		{"linux", "chrome", ".config/google-chrome"},
		{"darwin", "brave", "Library/Application Support/BraveSoftware/Brave-Browser"},
	} {
		got := DefaultUserDataDir(tc.goos, tc.browser)
		if !strings.HasSuffix(filepath.ToSlash(got), tc.want) {
			t.Fatalf("DefaultUserDataDir(%s, %s) = %q, want a path ending %q", tc.goos, tc.browser, got, tc.want)
		}
	}
	if got := DefaultUserDataDir("darwin", "a-browser-brw-has-never-heard-of"); got != "" {
		t.Fatalf("an unknown browser resolved to %q; the caller must be told to name the directory", got)
	}
}

func silentOptInChrome(t *testing.T, secondLine string) (dir string, port int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	port = serverPort(t, srv)
	dir = t.TempDir()
	writeActivePort(t, dir, strconv.Itoa(port)+"\n"+secondLine)
	return dir, port
}

func TestDiscoverReadsTheBrowserTargetOffTheFileWhenHTTPIsSilent(t *testing.T) {
	dir, port := silentOptInChrome(t, "/devtools/browser/opted-in-uuid\n")
	got, err := Discover(context.Background(), Options{UserDataDir: dir})
	if err != nil {
		t.Fatalf("discovery refused a Chrome whose opt-in is on: %v", err)
	}
	want := fmt.Sprintf("ws://127.0.0.1:%d/devtools/browser/opted-in-uuid", port)
	if got.BrowserWSURL != want {
		t.Fatalf("BrowserWSURL = %q, want %q", got.BrowserWSURL, want)
	}
	if got.Port != port {
		t.Fatalf("Port = %d, want %d", got.Port, port)
	}

	if got.Browser != "" {
		t.Fatalf("Browser = %q; nothing served a version document", got.Browser)
	}
	if got.Major != 0 {
		t.Fatalf("Major = %d; nothing served a version to parse", got.Major)
	}
	if !strings.Contains(got.BrowserLabel(), "no DevTools HTTP endpoints") {
		t.Fatalf("BrowserLabel() = %q, which does not say why there is no version", got.BrowserLabel())
	}
}

func TestDiscoverRefusesASecondLineThatIsNotAPath(t *testing.T) {
	for _, tc := range []struct {
		name       string
		secondLine string
	}{
		{name: "no second line at all", secondLine: ""},
		{name: "an absolute websocket url elsewhere", secondLine: "ws://browser.invalid:9222/devtools/browser/x\n"},
		{name: "a protocol-relative authority", secondLine: "//browser.invalid/devtools/browser/x\n"},
		{name: "a relative path", secondLine: "devtools/browser/x\n"},
		{name: "an opaque reference", secondLine: "mailto:x\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := silentOptInChrome(t, tc.secondLine)
			_, err := Discover(context.Background(), Options{UserDataDir: dir})
			if err == nil {
				t.Fatal("discovery accepted a browser target it cannot vouch for")
			}
			if !errors.Is(err, ErrOptInOff) {
				t.Fatalf("error %v does not wrap ErrOptInOff", err)
			}
			if !strings.Contains(err.Error(), "chrome://inspect") {
				t.Fatalf("error %q does not name the action", err)
			}
		})
	}
}

func TestDiscoverPrefersTheServedVersionDocument(t *testing.T) {
	dir, port := fakeChrome(t, chrome144)

	writeActivePort(t, dir, strconv.Itoa(port)+"\n/devtools/browser/from-the-file\n")
	got, err := Discover(context.Background(), Options{UserDataDir: dir})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.BrowserWSURL != browserWS(port) {
		t.Fatalf("BrowserWSURL = %q, want the served %q", got.BrowserWSURL, browserWS(port))
	}
	if got.Major != 144 {
		t.Fatalf("Major = %d, want the served 144", got.Major)
	}
}
