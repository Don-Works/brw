package setup

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// InstalledBridgeDefault is one extension copy's bridge-defaults.json: the
// packaged endpoint the extension falls back to when its own stored config sets
// none.
//
// StatusURL is empty when the file names no endpoint at all, which is different
// from "no file": a file that configures only a label leaves the endpoint to the
// built-in default and is not drift.
type InstalledBridgeDefault struct {
	Path      string
	StatusURL string
	BridgeURL string
}

// InstalledBridgeDefaults reads every bridge-defaults.json under appDir — the
// shared extension payload and each per-profile copy.
//
// The file is install state, not payload: no release archive contains one, and
// both scripts/install.sh and RefreshExtensionPayloads copy each profile's own
// copy across an upgrade. So a machine that was once pointed at a port keeps
// pointing at it, long after the daemon moved, and nothing in a release ever
// corrects it.
//
// A copy that cannot be read or parsed is skipped rather than failing the lot:
// this is diagnostic input, and one unreadable profile must not hide the rest.
func InstalledBridgeDefaults(appDir string) ([]InstalledBridgeDefault, error) {
	dirs := []string{filepath.Join(appDir, "extension")}
	perProfile, err := PerProfileExtensionDirs(appDir)
	if err != nil {
		return nil, err
	}
	dirs = append(dirs, perProfile...)

	var found []InstalledBridgeDefault
	for _, dir := range dirs {
		path := filepath.Join(dir, BridgeDefaultsFile)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var file struct {
			BridgeURL  string `json:"bridgeUrl"`
			URL        string `json:"url"`
			StatusURL  string `json:"statusUrl"`
			BridgePort any    `json:"bridgePort"`
		}
		if err := json.Unmarshal(data, &file); err != nil {
			continue
		}
		bridgeURL := firstNonEmpty(file.BridgeURL, file.URL, bridgeURLFromPort(file.BridgePort))
		found = append(found, InstalledBridgeDefault{
			Path:      path,
			BridgeURL: bridgeURL,
			StatusURL: firstNonEmpty(file.StatusURL, statusURLFromBridgeURL(bridgeURL)),
		})
	}
	return found, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// bridgeURLFromPort mirrors the extension's bridgePort shorthand. JSON numbers
// decode as float64, so the port is accepted in either spelling.
func bridgeURLFromPort(value any) string {
	var port int
	switch typed := value.(type) {
	case float64:
		port = int(typed)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return ""
		}
		port = parsed
	default:
		return ""
	}
	if port < 1 || port > 65535 {
		return ""
	}
	return "ws://127.0.0.1:" + strconv.Itoa(port) + "/extension"
}

// statusURLFromBridgeURL mirrors the extension's deriveStatusURL: same host and
// port, http scheme, /status path.
func statusURLFromBridgeURL(bridgeURL string) string {
	if bridgeURL == "" {
		return ""
	}
	parsed, err := url.Parse(bridgeURL)
	if err != nil || parsed.Host == "" {
		return ""
	}
	parsed.Scheme = "http"
	parsed.Path = "/status"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}
