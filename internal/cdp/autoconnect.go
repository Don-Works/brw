package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Auto-connecting to a browser that is already running.
//
// `brwd --remote http://127.0.0.1:9222` needs a port the operator has to know,
// and for a Chrome that brw itself launched earlier that port is ephemeral —
// nobody knows it but the browser, which writes it into DevToolsActivePort in
// its own user data directory. So the common case, "attach to the browser that
// is already there", meant reading a file by hand or guessing.
//
// `--remote auto` does the two things a person would do: read that file for
// every user data directory brw knows about, then try the conventional ports.

// AutoEndpoint is the value --remote auto resolves to. Everything but URL is
// there so the daemon can say what it attached to rather than only that it did.
type AutoEndpoint struct {
	URL string
	// Port is the TCP port the endpoint is on.
	Port int
	// Browser is the /json/version Browser string, for example
	// "Chrome/141.0.0.0". It is what proves something answered as a browser.
	Browser string
	// Source is how it was found: SourceActivePortFile or SourcePortProbe.
	Source string
	// From is the user data directory whose DevToolsActivePort named it, when
	// that is how it was found.
	From string
}

const (
	// SourceActivePortFile is the precise path: the browser itself wrote the
	// port into its profile directory.
	SourceActivePortFile = "devtools-active-port"
	// SourcePortProbe is the guess: a conventional port answered.
	SourcePortProbe = "port-probe"
)

// AutoConnectValue is the --remote value that turns discovery on.
const AutoConnectValue = "auto"

// IsAutoConnect reports whether a --remote value asks for discovery.
func IsAutoConnect(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), AutoConnectValue)
}

// DefaultProbePorts are the ports a Chromium is conventionally started on with
// --remote-debugging-port. The list is short on purpose: every port on it is a
// port brw will connect a debugger to and drive somebody's browser through, so
// widening it trades an unlikely convenience for attaching to something the
// operator did not mean.
func DefaultProbePorts() []int {
	return []int{9222, 9223, 9224, 9229, 9333, 9922}
}

// ErrNoEndpoint is the refusal when nothing was found. It names where brw
// looked, because the fix is always to start a browser or to say which port.
var ErrNoEndpoint = errors.New("no running Chrome DevTools endpoint found")

// AutoConnectOptions configures discovery. Probe is injectable so a test can
// stand in for a browser; production leaves it nil and gets ProbeEndpoint.
type AutoConnectOptions struct {
	// UserDataDirs are searched for DevToolsActivePort, in order. The first
	// that names a port something answers on wins.
	UserDataDirs []string
	// Ports are tried in order after the files. Nil means DefaultProbePorts.
	Ports []int
	// Probe reports the browser at a port, or an error. Nil means
	// ProbeEndpoint.
	Probe func(ctx context.Context, port int) (string, error)
}

// AutoConnect finds a running DevTools endpoint.
//
// The files come first and the ports second, and that order is the whole point:
// a DevToolsActivePort file in a directory the operator named is a browser they
// meant, while a conventional port is whatever happens to be listening. Both are
// verified by asking /json/version, so a port with some other service on it is
// never attached to.
func AutoConnect(ctx context.Context, opts AutoConnectOptions) (AutoEndpoint, error) {
	probe := opts.Probe
	if probe == nil {
		probe = ProbeEndpoint
	}
	ports := opts.Ports
	if ports == nil {
		ports = DefaultProbePorts()
	}

	var looked []string
	for _, dir := range opts.UserDataDirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		path := filepath.Join(dir, "DevToolsActivePort")
		looked = append(looked, path)
		port, err := readActivePort(path)
		if err != nil {
			continue
		}
		browser, err := probe(ctx, port)
		if err != nil {
			// The file outlives the browser that wrote it, so a stale one is
			// ordinary rather than exceptional: keep looking.
			continue
		}
		return AutoEndpoint{
			URL: endpointURL(port), Port: port, Browser: browser,
			Source: SourceActivePortFile, From: dir,
		}, nil
	}

	for _, port := range ports {
		looked = append(looked, endpointURL(port))
		browser, err := probe(ctx, port)
		if err != nil {
			continue
		}
		return AutoEndpoint{
			URL: endpointURL(port), Port: port, Browser: browser, Source: SourcePortProbe,
		}, nil
	}
	return AutoEndpoint{}, fmt.Errorf("%w; looked at %s. Start Chrome with --remote-debugging-port, or pass --remote with the endpoint",
		ErrNoEndpoint, strings.Join(looked, ", "))
}

func endpointURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// readActivePort reads the port Chrome wrote into its profile. The file is two
// lines: the port, then the browser's WebSocket path. Only the port is used —
// the path is re-fetched from /json/version, which is also what proves the
// browser is still there.
func readActivePort(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	first, _, _ := strings.Cut(string(data), "\n")
	port, err := strconv.Atoi(strings.TrimSpace(first))
	if err != nil {
		return 0, fmt.Errorf("%s does not start with a port number: %w", path, err)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s names port %d, which is not a port", path, port)
	}
	return port, nil
}

// ProbeEndpoint asks a loopback port whether it is a Chrome DevTools endpoint
// and returns its Browser string.
//
// The check is what keeps discovery from attaching brw to whatever happens to
// be listening: a service on 9222 that is not a browser answers without a
// webSocketDebuggerUrl, or does not answer this path at all, and is skipped.
// Only 127.0.0.1 is ever probed — a DevTools endpoint is a full remote-control
// channel for a signed-in browser, and reaching for one across a network is
// never something brw should do by guessing.
func ProbeEndpoint(ctx context.Context, port int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL(port)+"/json/version", nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("port %d answered %s", port, response.Status)
	}
	var payload struct {
		Browser              string `json:"Browser"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("port %d did not answer /json/version with JSON: %w", port, err)
	}
	if payload.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("port %d answered /json/version without a webSocketDebuggerUrl, so it is not a DevTools endpoint", port)
	}
	if payload.Browser == "" {
		return "", fmt.Errorf("port %d did not name a browser", port)
	}
	return payload.Browser, nil
}

// probeTimeout bounds one probe. Discovery tries several ports in sequence and
// a closed port fails immediately; this only matters for a port that accepts
// the connection and then says nothing.
const probeTimeout = 2 * time.Second
