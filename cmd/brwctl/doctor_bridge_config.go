package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"

	"github.com/Don-Works/brw/internal/setup"
)

// checkBridgeConfig answers one question: which bridge endpoint is the extension
// actually using, and is anything listening on it.
//
// It cannot be answered from disk. The extension's chrome.storage.local config
// silently overrides the packaged bridge-defaults.json, and nothing outside the
// browser can read chrome.storage.local — so a machine can hold a file naming a
// port nothing listens on AND a perfectly working bridge. Reading the file alone
// invents a fault; ignoring it reports green on a machine that is dead. The
// extension is therefore asked: a live hello reports the endpoint in use, and so
// does a REFUSED hello, which is the shape this drift produces now that the
// handshake token is mandatory (the extension could not fetch a token from a
// dead status URL, so it presented none and was turned away).
//
// A refusal needs the websocket URL to still reach this daemon. Drift that moves
// the websocket URL too — a stored bridgeUrl or bridgePort, from which the
// status URL is derived — reaches no daemon at all, so nothing is recorded
// anywhere and this check cannot name it. bridge_connected is the check that
// fails on that machine; the skip detail below points at it rather than reading
// as a clean bill.
//
// The packaged file is consulted only as a fallback, when no extension has
// reported anything at all, and the detail says so.
//
// Every endpoint this check reads comes from outside the process: a handshake
// report arrives on an unauthenticated websocket, and bridge-defaults.json is a
// file no release ever rewrites. probeStatusURL is what keeps either from
// choosing a URL this machine resolves and fetches.
func (d *doctorRun) checkBridgeConfig() {
	const name, title = "bridge_config", "bridge endpoint"
	if !d.resolved {
		d.add(checkSkip, name, title, "no profile resolved", "")
		return
	}
	if !d.profile.ExtensionBridgeAllowed {
		d.add(checkSkip, name, title, "direct-CDP profile: no extension config is involved", "")
		return
	}
	if d.req.SkipLiveChecks {
		d.add(checkSkip, name, title, "live checks skipped; probe with: brwctl doctor"+d.workspaceFlag(), "")
		return
	}

	addr := d.result.BridgeWSAddr
	packaged, err := setup.InstalledBridgeDefaults(d.req.AppDir)
	if err != nil {
		// An unreadable app directory returns the same empty list as a machine
		// with no file, and that reads as "nothing to correct". Name it instead.
		d.add(checkWarn, name, title,
			"cannot read the installed extension copies under "+d.req.AppDir+": "+err.Error(), "")
		return
	}
	usable, unusable := loopbackBridgeDefaults(packaged)
	if len(unusable) > 0 {
		d.add(checkFail, name, title,
			unusable[0]+" names an endpoint that is not http:// on a loopback port; it was neither contacted nor printed",
			d.bridgeSettingsCommand(addr))
		return
	}

	live, source, reporter := d.reportedBridgeConfig()
	if live != "" {
		safe, ok := loopbackStatusURL(live)
		if !ok {
			// Refuse the whole report rather than the URL alone: echoing it would
			// put a string of the reporter's choosing in an operator's terminal,
			// and a report is only evidence at all while it names somewhere the
			// extension could actually have been talking to.
			d.add(checkFail, name, title,
				"the endpoint named by "+reporter+" is not http:// on a loopback port; it was neither contacted nor printed",
				d.bridgeSettingsCommand(addr))
			return
		}
		live = safe
	}
	if live == "" {
		d.checkPackagedBridgeConfig(name, title, addr, usable)
		return
	}

	detail := fmt.Sprintf("the extension is using %s (%s, reported by %s)", live, configSourcePhrase(source), reporter)
	if d.bridge != nil && d.bridge.Connected {
		// The extension is connected to THIS bridge, which means it reached this
		// status URL and was served a token this daemon minted. Whatever a file on
		// disk says is not what is running.
		if drift := packagedDisagreeing(usable, live); drift != "" {
			detail += "; " + drift + ", and is overridden"
		}
		d.add(checkOK, name, title, detail, "")
		return
	}

	// Not connected, and the extension has named the endpoint it is trying.
	if sameEndpoint(live, statusURLFor(addr)) {
		d.add(checkOK, name, title, detail+"; that is this profile's bridge, so the endpoint is not the fault", "")
		return
	}
	if _, err := probeStatusURL(d.client, live); err != nil {
		d.add(checkFail, name, title, detail+" — "+endpointFailureDetail(err), d.bridgeSettingsCommand(addr))
		return
	}
	d.add(checkFail, name, title,
		fmt.Sprintf("%s — it answers, but it is not this profile's bridge at %s", detail, statusURLFor(addr)),
		d.bridgeSettingsCommand(addr))
}

// checkPackagedBridgeConfig is the fallback for a machine where no extension has
// reported a config: never loaded, never reloaded since the daemon last started,
// or older than the build that reports one. The packaged file is then the only
// evidence there is, and the detail says it is a guess about the live config
// rather than a reading of it.
func (d *doctorRun) checkPackagedBridgeConfig(name, title, addr string, packaged []setup.InstalledBridgeDefault) {
	const unreported = "no extension has reported which endpoint it is using"
	var named []setup.InstalledBridgeDefault
	for _, file := range packaged {
		if file.StatusURL != "" {
			named = append(named, file)
		}
	}
	if len(named) == 0 {
		// Not a clean bill: an extension whose stored bridgeUrl points at a dead
		// port never reaches this daemon, so it reports nothing from here either.
		d.add(checkSkip, name, title,
			unreported+", and no installed bridge-defaults.json names one — if the extension is pointed at a dead port, bridge_connected is the check that says so", "")
		return
	}
	for _, file := range named {
		if sameEndpoint(file.StatusURL, statusURLFor(addr)) {
			continue
		}
		detail := fmt.Sprintf("%s; %s names %s — it answers, but it is not this profile's bridge at %s",
			unreported, file.Path, file.StatusURL, statusURLFor(addr))
		if _, err := probeStatusURL(d.client, file.StatusURL); err != nil {
			detail = fmt.Sprintf("%s; %s names %s — %s", unreported, file.Path, file.StatusURL, endpointFailureDetail(err))
		}
		d.add(checkFail, name, title, detail, d.bridgeSettingsCommand(addr))
		return
	}
	d.add(checkOK, name, title,
		unreported+"; the installed bridge-defaults.json names this bridge", "")
}

// reportedBridgeConfig returns the endpoint the extension itself last named, the
// config layer it came from, and which report it came off. A live hello is
// preferred over a refused one: the refused one may predate the reload that
// fixed it.
func (d *doctorRun) reportedBridgeConfig() (statusURL, source, reporter string) {
	if d.bridge == nil {
		return "", "", ""
	}
	if d.bridge.Connected {
		// A connected extension that names no endpoint has not reported one. The
		// stale refusal below would still be printed as "the extension is using",
		// which asserts the wrong URL on a green line.
		return d.bridge.Hello.StatusURL, d.bridge.Hello.ConfigSource, "the connected extension"
	}
	if d.bridge.LastHandshake.StatusURL != "" {
		return d.bridge.LastHandshake.StatusURL, d.bridge.LastHandshake.ConfigSource, "the extension's refused handshake"
	}
	return "", "", ""
}

// packagedDisagreeing names an installed bridge-defaults.json that points
// somewhere other than the endpoint actually in use. It is drift worth printing
// — the next profile that loses its stored config falls back to it — but it is
// not a fault while something else is overriding it.
func packagedDisagreeing(packaged []setup.InstalledBridgeDefault, live string) string {
	for _, file := range packaged {
		if file.StatusURL != "" && !sameEndpoint(file.StatusURL, live) {
			return fmt.Sprintf("%s still names %s", file.Path, file.StatusURL)
		}
	}
	return ""
}

func statusURLFor(wsAddr string) string {
	return "http://" + wsAddr + "/status"
}

// errNotLoopbackEndpoint marks a status URL this process will not contact.
var errNotLoopbackEndpoint = errors.New("not an http:// status URL on a loopback port")

// loopbackStatusURL is the one gate every endpoint from outside this process
// passes. It accepts what the extension itself would accept — http, a loopback
// host, an explicit port, the /status path (extension/service_worker.js
// normalizeStatusURL) — and returns the URL stripped of userinfo, query and
// fragment so what is fetched is also what is printed.
//
// Without it, one forged handshake on the bridge's unauthenticated websocket
// picks a hostname the operator's machine resolves and a URL it GETs, which is
// network egress the extension bridge's own boundary (docs/auth-model.md) says
// belongs to brw and not to the local process on the other end of that socket.
func loopbackStatusURL(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "http" || parsed.Port() == "" {
		return "", false
	}
	host := parsed.Hostname()
	if host != "localhost" {
		// A name is never resolved to decide this: a hostname that resolves to a
		// loopback address today is an attacker's DNS record tomorrow.
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return "", false
		}
	}
	switch parsed.Path {
	case "", "/":
		parsed.Path = "/status"
	case "/status":
	default:
		return "", false
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String(), true
}

// loopbackBridgeDefaults splits the installed files into the ones naming an
// endpoint this check may contact and the paths of the ones naming something
// else. A file that names no endpoint at all is not drift and stays.
func loopbackBridgeDefaults(files []setup.InstalledBridgeDefault) ([]setup.InstalledBridgeDefault, []string) {
	var usable []setup.InstalledBridgeDefault
	var unusable []string
	for _, file := range files {
		if file.StatusURL == "" {
			usable = append(usable, file)
			continue
		}
		safe, ok := loopbackStatusURL(file.StatusURL)
		if !ok {
			unusable = append(unusable, file.Path)
			continue
		}
		file.StatusURL = safe
		usable = append(usable, file)
	}
	return usable, unusable
}

// sameEndpoint compares two status URLs by host and port only. "localhost" and
// "127.0.0.1" are the same daemon and the extension accepts either spelling, so
// a textual comparison would report a fault on a machine that is working.
func sameEndpoint(left, right string) bool {
	leftHost, leftOK := endpointHostPort(left)
	rightHost, rightOK := endpointHostPort(right)
	return leftOK && rightOK && leftHost == rightHost
}

func endpointHostPort(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return "", false
	}
	host := parsed.Hostname()
	if host == "localhost" {
		host = "127.0.0.1"
	}
	port := parsed.Port()
	if port == "" {
		return "", false
	}
	return host + ":" + port, true
}

// configSourcePhrase renders the extension's config_source for an operator. An
// unreported source is named as unreported rather than guessed at: which layer
// holds the endpoint is the difference between editing a file and editing the
// extension's options page.
func configSourcePhrase(source string) string {
	switch source {
	case "stored":
		return "from the extension's stored config, set on its options page"
	case "packaged":
		return "from the packaged bridge-defaults.json"
	case "built-in":
		return "from the extension's built-in default"
	default:
		return "from a config layer this extension build does not report"
	}
}

// endpointFailureDetail separates the ways a status URL fails to answer. They
// take different fixes, and "unreachable" alone sends an operator to restart a
// daemon that is already running on another port.
func endpointFailureDetail(err error) string {
	switch {
	case errors.Is(err, errNotLoopbackEndpoint):
		return "it is not http:// on a loopback port, so it was not contacted"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "nothing is listening there"
	case isTimeout(err):
		return "the connection was accepted but /status did not answer in time"
	case errors.Is(err, errUnexpectedResponse):
		return "something other than a brw bridge answered: " + err.Error()
	default:
		return "it is unreachable: " + err.Error()
	}
}

// probeStatusURL asks a full status URL whether a brw bridge is behind it. It
// takes the URL as the extension holds it, rather than a host:port, so the thing
// doctor reports on is the thing it tested.
func probeStatusURL(client *http.Client, statusURL string) (bridgeStatus, error) {
	// The gate lives here rather than only at the call sites so that a later
	// caller cannot reach the network with an endpoint it was handed.
	target, ok := loopbackStatusURL(statusURL)
	if !ok {
		return bridgeStatus{}, errNotLoopbackEndpoint
	}
	var status bridgeStatus
	err := fetchJSON(client, target, &status)
	return status, err
}
