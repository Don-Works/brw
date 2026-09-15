// Package chromeoptin discovers the DevTools endpoint a Chrome 144+ user has
// turned on for themselves at chrome://inspect/#remote-debugging.
//
// It only ever reads. Chrome 136 stopped honouring --remote-debugging-port
// against the default user data directory precisely so that no program could
// arrange full CDP access to a signed-in profile without the person noticing,
// and the 144 opt-in is the sanctioned replacement: a switch the human flips.
// Nothing in this package launches a browser, writes a preference, or otherwise
// arranges for the opt-in to be on — a brw that did that would have re-created
// the hole Chrome closed.
package chromeoptin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// MinimumChromeMajor is the first Chrome that offers the opt-in. Older Chromes
// either refuse remote debugging on the default profile outright (136-143) or
// take --remote-debugging-port against it (135 and earlier), and neither is
// this lane.
const MinimumChromeMajor = 144

// activePortFile is where Chrome records the port its DevTools server bound.
// The opt-in allocates a port dynamically — there is no flag to pin it and no
// fixed default — so this file is the discovery channel. Its first line is the
// port and its second is the browser target's WebSocket path.
const activePortFile = "DevToolsActivePort"

// ErrOptInOff reports that no opt-in endpoint is reachable for a user data
// directory. It is a distinct error because it is not a fault: an opt-in that
// is off is the shipped default, and the answer is an instruction to a human,
// never a retry or a launch.
var ErrOptInOff = errors.New("chrome remote debugging opt-in is off")

// ErrChromeTooOld reports a Chrome that predates the opt-in.
var ErrChromeTooOld = errors.New("this chrome predates the remote debugging opt-in")

// UserAction is what a person has to do, named the same way everywhere it is
// reported so the instruction does not drift between brwd's log line, doctor's
// fix and the tool error an agent sees.
const UserAction = "open chrome://inspect/#remote-debugging in the Chrome you want brw to drive and turn on remote debugging; brw will not and cannot turn it on for you"

// Endpoint is a discovered, live opt-in DevTools endpoint.
type Endpoint struct {
	// HTTPURL is the http://127.0.0.1:<port> base brw attaches to.
	HTTPURL string
	// Port is the dynamically allocated port Chrome recorded.
	Port int
	// BrowserWSURL is the browser target's WebSocket URL. Its presence is what
	// separates this lane from the extension bridge: an extension cannot attach
	// chrome.debugger to the browser target at all.
	BrowserWSURL string
	// Browser is Chrome's own version string, e.g. "Chrome/144.0.7000.0".
	Browser string
	// Major is Browser's major version.
	Major int
	// UserDataDir is the directory the endpoint was discovered from.
	UserDataDir string
}

// Options configure discovery.
type Options struct {
	// UserDataDir is the Chrome user data directory to look in. Required: the
	// caller resolves the platform default, because which browser's profile is
	// meant is a policy question and not one this package should guess.
	UserDataDir string
	// Client probes the endpoint. A nil client gets a short-timeout loopback
	// client.
	Client *http.Client
}

// Discover resolves the opt-in endpoint for a user data directory.
//
// Both halves are required. The file alone proves nothing: Chrome leaves it
// behind when it exits, and the port it names is then free for any other
// process to bind. So the port is probed, the answer must be a Chrome DevTools
// endpoint, and the browser WebSocket URL it reports must point back at the
// same loopback port — otherwise brw would follow whatever address an
// unrelated listener chose to return.
func Discover(ctx context.Context, opts Options) (Endpoint, error) {
	dir := strings.TrimSpace(opts.UserDataDir)
	if dir == "" {
		return Endpoint{}, errors.New("chrome opt-in discovery needs a user data directory")
	}
	port, err := readActivePort(dir)
	if err != nil {
		return Endpoint{}, err
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	version, err := probeVersion(ctx, client, port)
	if err != nil {
		return Endpoint{}, err
	}
	endpoint := Endpoint{
		HTTPURL:      fmt.Sprintf("http://127.0.0.1:%d", port),
		Port:         port,
		BrowserWSURL: version.WebSocketDebuggerURL,
		Browser:      version.Browser,
		Major:        majorVersion(version.Browser),
		UserDataDir:  dir,
	}
	if err := validateBrowserWS(endpoint.BrowserWSURL, port); err != nil {
		return Endpoint{}, err
	}
	if endpoint.Major > 0 && endpoint.Major < MinimumChromeMajor {
		return Endpoint{}, fmt.Errorf("%w: %s is listening on port %d, but the opt-in lane needs Chrome %d or newer", ErrChromeTooOld, endpoint.Browser, port, MinimumChromeMajor)
	}
	return endpoint, nil
}

// maxActivePortBytes bounds the read of DevToolsActivePort. Chrome writes a
// port and a path; anything larger is not that file, and this is a path in a
// directory brw does not own, so the read is bounded rather than trusted.
const maxActivePortBytes = 4 << 10

// maxVersionBytes bounds the /json/version body. The port is read from a file
// any process could have replaced, so the listener answering may not be Chrome:
// without a cap it can stream for the client's whole timeout.
const maxVersionBytes = 64 << 10

// readActivePort parses the port Chrome recorded. A missing file, an empty one
// and an unparseable one all mean the same thing to the caller — no opt-in
// endpoint — so all three answer with ErrOptInOff and the action.
func readActivePort(dir string) (int, error) {
	path := filepath.Join(dir, activePortFile)
	// Lstat, not Stat: a symlink here would let whoever placed it choose the
	// file brw reads, and a fifo would let them choose how long the read takes.
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("%w: %s does not exist, so this Chrome has no debugging endpoint. %s", ErrOptInOff, path, UserAction)
		}
		return 0, fmt.Errorf("%w: cannot read %s: %v. %s", ErrOptInOff, path, err, UserAction)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%w: %s is not a regular file. %s", ErrOptInOff, path, UserAction)
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("%w: cannot read %s: %v. %s", ErrOptInOff, path, err, UserAction)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxActivePortBytes))
	if err != nil {
		return 0, fmt.Errorf("%w: cannot read %s: %v. %s", ErrOptInOff, path, err, UserAction)
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	port, convErr := strconv.Atoi(strings.TrimSpace(lines[0]))
	if convErr != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("%w: %s does not start with a port number. %s", ErrOptInOff, path, UserAction)
	}
	return port, nil
}

type versionInfo struct {
	Browser              string `json:"Browser"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// probeVersion confirms something is listening on the recorded port AND that it
// is a Chrome DevTools endpoint. A stale file whose port another process has
// since taken is the case this catches.
func probeVersion(ctx context.Context, client *http.Client, port int) (versionInfo, error) {
	target := fmt.Sprintf("http://127.0.0.1:%d/json/version", port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return versionInfo{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return versionInfo{}, fmt.Errorf("%w: nothing answered %s (%v), so the recorded port is stale. %s", ErrOptInOff, target, err, UserAction)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return versionInfo{}, fmt.Errorf("%w: %s answered %s rather than a DevTools version document. %s", ErrOptInOff, target, resp.Status, UserAction)
	}
	var out versionInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxVersionBytes)).Decode(&out); err != nil {
		return versionInfo{}, fmt.Errorf("%w: %s did not answer with a DevTools version document (%v). %s", ErrOptInOff, target, err, UserAction)
	}
	if strings.TrimSpace(out.WebSocketDebuggerURL) == "" {
		// The browser target is the whole point of this lane. An endpoint that
		// exposes only page targets cannot open an incognito context or read a
		// cookie at the browser level, so it is not this lane.
		return versionInfo{}, fmt.Errorf("%w: %s reports no browser target, so brw cannot reach browser-level CDP there. %s", ErrOptInOff, target, UserAction)
	}
	return out, nil
}

// validateBrowserWS refuses a browser WebSocket URL that does not point back at
// the loopback port discovery probed. Chrome reports this URL and brw would
// otherwise dial whatever it says: a stale file pointing at a port some other
// local process now holds is enough for that process to redirect brw's whole
// CDP session elsewhere.
func validateBrowserWS(raw string, port int) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: browser target URL %q is unparseable. %s", ErrOptInOff, raw, UserAction)
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return fmt.Errorf("%w: browser target URL %q is not a WebSocket URL. %s", ErrOptInOff, raw, UserAction)
	}
	host, wsPort, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return fmt.Errorf("%w: browser target URL %q has no host:port. %s", ErrOptInOff, raw, UserAction)
	}
	if !isLoopback(host) {
		return fmt.Errorf("%w: browser target URL %q points off this machine. %s", ErrOptInOff, raw, UserAction)
	}
	if wsPort != strconv.Itoa(port) {
		return fmt.Errorf("%w: browser target URL %q is on port %s, not the %d that %s named. %s", ErrOptInOff, raw, wsPort, port, activePortFile, UserAction)
	}
	return nil
}

// isLoopback accepts only an address that resolves to the loopback interface
// without a DNS lookup. "localhost" is deliberately included and every other
// name is not: a name brw would have to resolve can be pointed anywhere.
func isLoopback(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// majorVersion pulls the major out of "Chrome/144.0.7000.0". An unrecognised
// string yields 0, which Discover treats as "cannot tell" rather than as old:
// refusing a Chromium fork whose version string brw cannot parse would be a
// worse failure than attaching to one.
func majorVersion(browser string) int {
	_, version, ok := strings.Cut(browser, "/")
	if !ok {
		return 0
	}
	major, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(strings.TrimSpace(major))
	if err != nil {
		return 0
	}
	return n
}
