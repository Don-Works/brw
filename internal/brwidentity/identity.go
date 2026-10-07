package brwidentity

import "fmt"

// Identity is the runtime identity exposed by a brw HTTP daemon.
type Identity struct {
	Workspace        string `json:"workspace,omitempty"`
	Profile          string `json:"profile,omitempty"`
	UserDataDir      string `json:"user_data_dir,omitempty"`
	ProfileDirectory string `json:"profile_directory,omitempty"`
	Mode             string `json:"mode,omitempty"`
	// Transport is how this daemon reaches the browser, independent of how the caller reaches this daemon.
	Transport string `json:"transport,omitempty"`
	// Headless reports whether the browser this daemon drives has no visible window.
	Headless bool `json:"headless,omitempty"`
	// IgnoreHTTPSErrors reports that this daemon launched Chrome with certificate validation off.
	IgnoreHTTPSErrors bool `json:"ignore_https_errors,omitempty"`
}

// Transport values.
const (
	// TransportDirectCDP is a browser brw started itself.
	TransportDirectCDP = "direct-cdp"
	// TransportChromeOptIn is a Chrome 144+ instance whose user turned on remote debugging at chrome://inspect/#remote-debugging.
	TransportChromeOptIn = "chrome-opt-in-cdp"
	// TransportRemoteCDP is `brwd --remote` at an endpoint on THIS machine: brw attaches to a DevTools endpoint some other process opened.
	TransportRemoteCDP = "remote-cdp"
	// TransportOffHostCDP is the CDP wire protocol over a socket to a machine that is not this one: a browser a plugin holding browser.provider lent brw, and equally a --remote endpoint that is not loopback.
	TransportOffHostCDP      = "off-host-cdp"
	TransportExtensionBridge = "extension-bridge"
)

func (i Identity) Empty() bool {
	return i.Workspace == "" &&
		i.Profile == "" &&
		i.UserDataDir == "" &&
		i.ProfileDirectory == "" &&
		i.Mode == "" &&
		i.Transport == "" &&
		!i.Headless &&
		!i.IgnoreHTTPSErrors
}

// Mismatches compares non-empty expected fields.
func (i Identity) Mismatches(expected Identity) []string {
	var out []string
	check := func(field, got, want string) {
		if want != "" && got != want {
			out = append(out, fmt.Sprintf("%s got %q want %q", field, got, want))
		}
	}
	check("workspace", i.Workspace, expected.Workspace)
	check("profile", i.Profile, expected.Profile)
	check("user_data_dir", i.UserDataDir, expected.UserDataDir)
	check("profile_directory", i.ProfileDirectory, expected.ProfileDirectory)
	check("mode", i.Mode, expected.Mode)
	return out
}
