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
// reaches brw. transports.go states what each one can do; a value added here
// without an entry there is not a transport brw can report.
const (
	// TransportDirectCDP is a browser brw started itself. It is the only lane
	// on which brw may move the browser's downloads, because it is the only one
	// where nobody else is downloading in the same browser.
	TransportDirectCDP = "direct-cdp"
	// TransportChromeOptIn is a Chrome 144+ instance whose user turned on
	// remote debugging at chrome://inspect/#remote-debugging. It is its own
	// lane and not a flavour of direct CDP: the browser is the one the user is
	// signed into, brw never started it, and brw must never try to turn the
	// opt-in on — it exists precisely because that switch is a human action.
	TransportChromeOptIn = "chrome-opt-in-cdp"
	// TransportRemoteCDP is `brwd --remote` at an endpoint on THIS machine: brw
	// attaches to a DevTools endpoint some other process opened. It was reported
	// as direct CDP, which says brw started the browser. The browser behind that
	// endpoint may be the one its user is signed into, up to and including the
	// port the Chrome opt-in publishes, and a lane reported as one brw started
	// routed and then deleted that person's downloads.
	//
	// The browser is still on this machine, so a filesystem path, an upload and
	// the clipboard all mean what the caller meant. An endpoint brw cannot prove
	// is on this machine is TransportOffHostCDP instead.
	TransportRemoteCDP = "remote-cdp"
	// TransportOffHostCDP is the CDP wire protocol over a socket to a machine
	// that is not this one: a browser a plugin holding browser.provider lent
	// brw, and equally a --remote endpoint that is not loopback.
	//
	// It names where the browser IS, not which flag asked for it, so a new way
	// of reaching a browser elsewhere lands here without an edit to the gates
	// that read it. That is also why it is separate from TransportRemoteCDP
	// rather than folded into it: --remote is pointed at a loopback endpoint in
	// every topology brw ships for, and there this machine's filesystem and
	// clipboard are the browser's too. Here they are not, so a path, an upload
	// or a clipboard read answers about the wrong machine rather than failing.
	//
	// It is separate from TransportDirectCDP because the difference is not
	// cosmetic — no local profile, no local filesystem, no extension bridge —
	// and a caller that cannot tell the two apart cannot avoid asking for those.
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
