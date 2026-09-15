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

// fakeChrome stands in for a Chrome that has the opt-in on: a user data
// directory holding a DevToolsActivePort file, and something answering
// /json/version on the port it names. version is built from the port the fake
// actually bound, because the browser WebSocket URL a real Chrome reports
// carries that port and the check under test compares the two.
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

// chrome144 is the ordinary healthy answer: the opt-in on, browser target
// exposed, version new enough.
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

// The lane exists to give brw browser-target CDP against the signed-in profile,
// so discovery has to return the browser WebSocket URL and the version that
// proves the opt-in is available at all.
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

// Every way the opt-in can be off or the recorded endpoint can be wrong has to
// answer with ErrOptInOff AND the user action, because the only fix is a person
// flipping a switch. A case that answered with a bare error would be reported
// to the user as a brw fault.
func TestDiscoverRefusalsNameTheUserAction(t *testing.T) {
	// A listener that is not Chrome, for the "stale file, port reused" case.
	notChrome := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello from something else"))
	}))
	t.Cleanup(notChrome.Close)
	otherPort := serverPort(t, notChrome)

	// A port nothing is listening on: bind one and close it immediately.
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
			// The redirect shape: brw is handed a WebSocket URL by whatever
			// answered, and following it would move the whole CDP session to a
			// listener that is not the browser discovery probed.
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
			// Each refusal has to name the action that fixes it, and the two
			// refusals take different actions: an opt-in that is off is a
			// switch to flip, an old Chrome is an upgrade. A message that named
			// the wrong one would send the user to a page with no switch on it.
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

// Chrome 144 is the first release with the opt-in, so it must be accepted and
// 143 must not. A boundary written as > rather than >= would pass every other
// test in this file.
func TestDiscoverVersionBoundary(t *testing.T) {
	for _, tc := range []struct {
		browser string
		wantOK  bool
	}{
		{"Chrome/143.0.6000.0", false},
		{"Chrome/144.0.7000.0", true},
		{"Chrome/153.0.8010.37", true},
		// A Chromium fork whose version string brw cannot parse is attached to
		// rather than refused: refusing one because the string is unfamiliar is
		// a worse failure than attaching to a browser that turns out to be old.
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

// Discovery only ever reads. A run against a directory brw cannot write must
// still work, and must leave nothing behind — the directory is the profile the
// user is signed into.
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

// The default directory is resolved from the same table `brwctl setup` binds a
// bridge profile to, so the two lanes cannot end up looking in different
// places for the same browser's profile.
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

// silentOptInChrome is the endpoint shape a genuinely opted-in Chrome presents:
// the port is bound and every DevTools HTTP path answers 404.
//
// Measured on Chrome 153.0.8010.37 with the opt-in turned on by writing
// {"devtools":{"remote_debugging":{"user-enabled":true}}} into Local State —
// Chrome's own preference, the one chrome://inspect/#remote-debugging sets:
// DevToolsActivePort was written with the port and /devtools/browser/<uuid>,
// and /json/version, /json/list, /json and / all answered 404, headless and
// headed alike. A Chrome started with --remote-debugging-port serves those
// paths; the opt-in does not, which is why the browser target has to come out
// of the file.
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

// The lane brw is built for is the one it could not reach: discovery refused a
// Chrome whose user had turned the opt-in on, because it asked an HTTP endpoint
// that opt-in does not serve and read 404 as "no endpoint". The browser target
// is on the second line of DevToolsActivePort, and that is what this asserts is
// used.
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
	// There is no version to read on this lane, and saying "Chrome/0" or
	// leaving a human staring at empty brackets are both worse than saying so.
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

// The fallback narrows the trust rather than widening it. brw builds the
// WebSocket URL itself out of loopback, the recorded port and the recorded
// path, so a second line that names a scheme or an authority — the one way this
// file could redirect a CDP session off the machine — is not followed. It is
// also not an error on its own: the HTTP probe may still produce a target.
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

// When the endpoint does serve /json/version — a Chrome started with
// --remote-debugging-port=0, which is what the live tests stand up — that
// answer is still what discovery uses, including its version. The file is the
// fallback, not the new default.
func TestDiscoverPrefersTheServedVersionDocument(t *testing.T) {
	dir, port := fakeChrome(t, chrome144)
	// Overwrite the second line with a path nothing would dial, so a discovery
	// that preferred the file would be caught here.
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
