package brwidentity

import "fmt"

// Identity is the runtime identity exposed by a brw HTTP daemon. MCP wrappers
// use it to prove that an upstream loopback daemon is the workspace/profile they
// were launched for, rather than trusting a port number or namespace label.
type Identity struct {
	Workspace        string `json:"workspace,omitempty"`
	Profile          string `json:"profile,omitempty"`
	UserDataDir      string `json:"user_data_dir,omitempty"`
	ProfileDirectory string `json:"profile_directory,omitempty"`
	Mode             string `json:"mode,omitempty"`
	// Transport is how this daemon reaches the browser, independent of how
	// the caller reaches this daemon. Mode cannot answer that: a disposable
	// --upstream-http MCP proxy reports Mode "upstream-http" whether the
	// daemon behind it drives Chrome over direct CDP or the extension
	// bridge, so an agent choosing between incognito (direct only) and tab
	// groups (an extension API) had no way to tell but to grep ps for the
	// upstream's flags. The proxy adopts its upstream's Transport, so this
	// field means the same thing at every hop.
	Transport string `json:"transport,omitempty"`
	// Headless reports whether the browser this daemon drives has no visible
	// window. Propagated through a proxy the same way Transport is.
	Headless bool `json:"headless,omitempty"`
	// IgnoreHTTPSErrors reports that this daemon launched Chrome with
	// certificate validation off. Every https page it loads may be
	// intercepted or served by an impostor and the browser will not say so,
	// so a caller reading a page over this daemon has to be able to find out
	// from the daemon itself. Propagated through a proxy like Transport.
	IgnoreHTTPSErrors bool `json:"ignore_https_errors,omitempty"`
}

// Transport values. These name how brw reaches the browser, not how a client
// reaches brw.
const (
	TransportDirectCDP       = "direct-cdp"
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

// Mismatches compares non-empty expected fields. Empty expected fields are
// treated as "do not care" so an upstream proxy can require workspace/profile
// identity without pinning the daemon's transport mode.
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
