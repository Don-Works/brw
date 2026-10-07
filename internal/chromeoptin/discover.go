// Package chromeoptin discovers the DevTools endpoint a Chrome 144+ user has turned on for themselves at chrome://inspect/#remote-debugging.
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

// MinimumChromeMajor is the first Chrome that offers the opt-in.
const MinimumChromeMajor = 144

const activePortFile = "DevToolsActivePort"

// ErrOptInOff reports that no opt-in endpoint is reachable for a user data directory.
var ErrOptInOff = errors.New("chrome remote debugging opt-in is off")

// ErrChromeTooOld reports a Chrome that predates the opt-in.
var ErrChromeTooOld = errors.New("this chrome predates the remote debugging opt-in")

// UserAction is what a person has to do, named the same way everywhere it is reported so the instruction does not drift between brwd's log line, doctor's fix and the tool error an agent sees.
const UserAction = "open chrome://inspect/#remote-debugging in the Chrome you want brw to drive and turn on remote debugging; brw will not and cannot turn it on for you"

// Endpoint is a discovered, live opt-in DevTools endpoint.
type Endpoint struct {
	// HTTPURL is the http://127.0.0.1:<port> base brw attaches to.
	HTTPURL string
	// Port is the dynamically allocated port Chrome recorded.
	Port int
	// BrowserWSURL is the browser target's WebSocket URL.
	BrowserWSURL string
	// Browser is Chrome's own version string, e.g.
	Browser string
	// Major is Browser's major version, or 0 when Browser is empty or unparseable.
	Major int
	// UserDataDir is the directory the endpoint was discovered from.
	UserDataDir string
}

// Options configure discovery.
type Options struct {
	// UserDataDir is the Chrome user data directory to look in.
	UserDataDir string
	// Client probes the endpoint.
	Client *http.Client
}

// Discover resolves the opt-in endpoint for a user data directory.
func Discover(ctx context.Context, opts Options) (Endpoint, error) {
	dir := strings.TrimSpace(opts.UserDataDir)
	if dir == "" {
		return Endpoint{}, errors.New("chrome opt-in discovery needs a user data directory")
	}
	port, wsPath, err := readActivePort(dir)
	if err != nil {
		return Endpoint{}, err
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	endpoint := Endpoint{
		HTTPURL:     fmt.Sprintf("http://127.0.0.1:%d", port),
		Port:        port,
		UserDataDir: dir,
	}
	version, probeErr := probeVersion(ctx, client, port)
	switch {
	case probeErr == nil:
		endpoint.BrowserWSURL = version.WebSocketDebuggerURL
		endpoint.Browser = version.Browser
		endpoint.Major = majorVersion(version.Browser)
	case errors.Is(probeErr, errNoDevToolsHTTP) && wsPath != "":
		endpoint.BrowserWSURL = fmt.Sprintf("ws://127.0.0.1:%d%s", port, wsPath)
	case errors.Is(probeErr, errNoDevToolsHTTP):

		return Endpoint{}, fmt.Errorf("%w: %v, and %s records no browser target on its second line. %s", ErrOptInOff, probeErr, filepath.Join(dir, activePortFile), UserAction)
	default:
		return Endpoint{}, probeErr
	}
	if err := validateBrowserWS(endpoint.BrowserWSURL, port); err != nil {
		return Endpoint{}, err
	}
	if endpoint.Major > 0 && endpoint.Major < MinimumChromeMajor {
		return Endpoint{}, fmt.Errorf("%w: %s is listening on port %d, but the opt-in lane needs Chrome %d or newer", ErrChromeTooOld, endpoint.Browser, port, MinimumChromeMajor)
	}
	return endpoint, nil
}

// BrowserLabel is Endpoint.Browser for a human, and says so rather than printing an empty pair of brackets when the opt-in served no version document.
func (e Endpoint) BrowserLabel() string {
	if strings.TrimSpace(e.Browser) != "" {
		return e.Browser
	}
	return "version not reported: this Chrome serves no DevTools HTTP endpoints"
}

const maxActivePortBytes = 4 << 10

const maxVersionBytes = 64 << 10

func readActivePort(dir string) (int, string, error) {
	path := filepath.Join(dir, activePortFile)

	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, "", fmt.Errorf("%w: %s does not exist, so this Chrome has no debugging endpoint. %s", ErrOptInOff, path, UserAction)
		}
		return 0, "", fmt.Errorf("%w: cannot read %s: %v. %s", ErrOptInOff, path, err, UserAction)
	}
	if !info.Mode().IsRegular() {
		return 0, "", fmt.Errorf("%w: %s is not a regular file. %s", ErrOptInOff, path, UserAction)
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, "", fmt.Errorf("%w: cannot read %s: %v. %s", ErrOptInOff, path, err, UserAction)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxActivePortBytes))
	if err != nil {
		return 0, "", fmt.Errorf("%w: cannot read %s: %v. %s", ErrOptInOff, path, err, UserAction)
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	port, convErr := strconv.Atoi(strings.TrimSpace(lines[0]))
	if convErr != nil || port <= 0 || port > 65535 {
		return 0, "", fmt.Errorf("%w: %s does not start with a port number. %s", ErrOptInOff, path, UserAction)
	}
	wsPath := ""
	if len(lines) > 1 {
		wsPath = browserTargetPath(lines[1])
	}
	return port, wsPath, nil
}

func browserTargetPath(line string) string {
	raw := strings.TrimSpace(line)
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.Opaque != "" || parsed.User != nil {
		return ""
	}
	return raw
}

type versionInfo struct {
	Browser              string `json:"Browser"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

var errNoDevToolsHTTP = errors.New("the port is bound but serves no DevTools HTTP endpoint")

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
	if resp.StatusCode == http.StatusNotFound {
		return versionInfo{}, fmt.Errorf("%w: %s answered %s", errNoDevToolsHTTP, target, resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return versionInfo{}, fmt.Errorf("%w: %s answered %s rather than a DevTools version document. %s", ErrOptInOff, target, resp.Status, UserAction)
	}
	var out versionInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxVersionBytes)).Decode(&out); err != nil {
		return versionInfo{}, fmt.Errorf("%w: %s did not answer with a DevTools version document (%v). %s", ErrOptInOff, target, err, UserAction)
	}
	if strings.TrimSpace(out.WebSocketDebuggerURL) == "" {

		return versionInfo{}, fmt.Errorf("%w: %s reports no browser target, so brw cannot reach browser-level CDP there. %s", ErrOptInOff, target, UserAction)
	}
	return out, nil
}

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

func isLoopback(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

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
