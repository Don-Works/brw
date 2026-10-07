package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"

	"github.com/Don-Works/brw/internal/setup"
)

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

		d.add(checkWarn, name, title,
			"cannot read the installed extension copies under "+d.req.AppDir+": "+err.Error(), "")
		return
	}

	usable, unusable := loopbackBridgeDefaults(packaged)

	live, source, reporter := d.reportedBridgeConfig()
	if live != "" {
		safe, ok := loopbackStatusURL(live)
		if !ok {

			d.add(checkFail, name, title,
				"the endpoint named by "+reporter+" is not http:// on a loopback port; it was neither contacted nor printed",
				d.bridgeSettingsCommand(addr))
			return
		}
		live = safe
	}
	if live == "" {
		d.checkPackagedBridgeConfig(name, title, addr, usable, unusable)
		return
	}

	detail := fmt.Sprintf("the extension is using %s (%s, reported by %s)", live, configSourcePhrase(source), reporter)
	if d.bridge != nil && d.bridge.Connected {

		if drift := packagedDisagreeing(usable, unusable, live); drift != "" {
			detail += "; " + drift + ", and is overridden"
		}
		d.add(checkOK, name, title, detail, "")
		return
	}

	if sameEndpoint(live, statusURLFor(addr)) {
		d.add(checkOK, name, title, detail+"; that is this profile's bridge, so the endpoint is not the fault", "")
		return
	}
	if _, err := probeStatusURL(d.client, live); err != nil {
		d.add(checkFail, name, title, detail+" — "+endpointFailureDetail(err, live), d.bridgeSettingsCommand(addr))
		return
	}
	d.add(checkFail, name, title,
		fmt.Sprintf("%s — it answers, but it is not this profile's bridge at %s", detail, statusURLFor(addr)),
		d.bridgeSettingsCommand(addr))
}

func (d *doctorRun) checkPackagedBridgeConfig(name, title, addr string, packaged []setup.InstalledBridgeDefault, unusable []string) {
	const unreported = "no extension has reported which endpoint it is using"
	if len(unusable) > 0 {

		d.add(checkFail, name, title,
			unreported+", and "+unusable[0]+" names an endpoint that is not http:// on a loopback port; it was neither contacted nor printed",
			d.bridgeSettingsCommand(addr))
		return
	}
	var named []setup.InstalledBridgeDefault
	for _, file := range packaged {
		if file.StatusURL != "" {
			named = append(named, file)
		}
	}
	if len(named) == 0 {

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
			detail = fmt.Sprintf("%s; %s names %s — %s", unreported, file.Path, file.StatusURL, endpointFailureDetail(err, file.StatusURL))
		}
		d.add(checkFail, name, title, detail, d.bridgeSettingsCommand(addr))
		return
	}
	d.add(checkOK, name, title,
		unreported+"; the installed bridge-defaults.json names this bridge", "")
}

func (d *doctorRun) reportedBridgeConfig() (statusURL, source, reporter string) {
	if d.bridge == nil {
		return "", "", ""
	}
	if d.bridge.Connected {

		return d.bridge.Hello.StatusURL, d.bridge.Hello.ConfigSource, "the connected extension"
	}
	if d.bridge.LastHandshake.StatusURL != "" {
		return d.bridge.LastHandshake.StatusURL, d.bridge.LastHandshake.ConfigSource, "the extension's refused handshake"
	}
	return "", "", ""
}

func packagedDisagreeing(packaged []setup.InstalledBridgeDefault, unusable []string, live string) string {
	if len(unusable) > 0 {
		return unusable[0] + " names an endpoint that is not http:// on a loopback port"
	}
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

var errNotLoopbackEndpoint = errors.New("not an http:// status URL on a loopback port")

func loopbackStatusURL(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "http" || parsed.Port() == "" {
		return "", false
	}
	host := parsed.Hostname()
	if host != "localhost" {

		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return "", false
		}
	}

	switch parsed.EscapedPath() {
	case "", "/", "/status":
	default:
		return "", false
	}
	parsed.Path = "/status"
	parsed.RawPath = ""
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String(), true
}

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

func endpointFailureDetail(err error, contacted string) string {
	switch {
	case errors.Is(err, errNotLoopbackEndpoint):
		return "it is not http:// on a loopback port, so it was not contacted"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "nothing is listening there"
	case isTimeout(err):
		return "the connection was accepted but /status did not answer in time"
	case errors.Is(err, errUnexpectedResponse):
		return "something other than a brw bridge answered: " + gatedErrorText(err, contacted)
	default:
		return "it is unreachable: " + gatedErrorText(err, contacted)
	}
}

func gatedErrorText(err error, contacted string) string {
	const withheld = "the failure named an endpoint this check did not contact, so it is not printed"
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.URL != contacted {
		return withheld
	}
	message := err.Error()
	for _, found := range urlInErrorText.FindAllString(message, -1) {
		if strings.TrimRight(found, `.,;:)]}"'`) != contacted {
			return withheld
		}
	}
	return message
}

var urlInErrorText = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://[^\s"']+`)

func probeStatusURL(client *http.Client, statusURL string) (bridgeStatus, error) {

	target, ok := loopbackStatusURL(statusURL)
	if !ok {
		return bridgeStatus{}, errNotLoopbackEndpoint
	}

	var status bridgeStatus
	err := fetchJSON(withoutRedirects(client), target, &status)
	return status, err
}
