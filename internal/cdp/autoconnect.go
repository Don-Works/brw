package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// AutoEndpoint is the value --remote auto resolves to.
type AutoEndpoint struct {
	URL string
	// Port is the TCP port the endpoint is on.
	Port int
	// Browser is the /json/version Browser string, for example "Chrome/141.0.0.0".
	Browser string
	// Source is how it was found: SourceActivePortFile or SourcePortProbe.
	Source string
	// From is the user data directory whose DevToolsActivePort named it, when that is how it was found.
	From string
}

const (
	// SourceActivePortFile is the precise path: the browser itself wrote the port into its profile directory.
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

// DefaultProbePorts are the ports a Chromium is conventionally started on with --remote-debugging-port.
func DefaultProbePorts() []int {
	return []int{9222, 9223, 9224, 9229, 9333, 9922}
}

// ErrNoEndpoint is the refusal when nothing was found.
var ErrNoEndpoint = errors.New("no running Chrome DevTools endpoint found")

// AutoConnectOptions configures discovery.
type AutoConnectOptions struct {
	// UserDataDirs are searched for DevToolsActivePort, in order.
	UserDataDirs []string
	// Ports are tried in order after the files.
	Ports []int
	// Probe reports the browser at a port, or an error.
	Probe func(ctx context.Context, port int) (string, error)
}

// AutoConnect finds a running DevTools endpoint.
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

// ProbeEndpoint asks a loopback port whether it is a Chrome DevTools endpoint and returns its Browser string.
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

	if err := json.NewDecoder(io.LimitReader(response.Body, maxProbeBody)).Decode(&payload); err != nil {
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

const maxProbeBody = 1 << 20

const probeTimeout = 2 * time.Second
