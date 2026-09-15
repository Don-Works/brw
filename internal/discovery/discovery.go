// Package discovery is brw's single profile-daemon discovery path: it turns the
// profile policy into the set of configured bridge daemons and probes each
// daemon's /health. `brwctl daemons` emits its records verbatim and the brw CLI
// resolves the daemon it acts against through the same functions, so neither can
// drift from the discovery contract a gateway consumes.
package discovery

import (
	"context"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/profilepolicy"
)

// Record is one configured browser-profile bridge daemon, as emitted by
// `brwctl daemons`. It is the discovery contract a gateway (e.g. mcplexer)
// consumes to register one namespace per brw profile-daemon. http_addr/ws_addr
// are the daemon's loopback control + extension-bridge addresses; identity is the
// live /health identity when the daemon is reachable.
type Record struct {
	Name        string                `json:"name"`
	Kind        string                `json:"kind,omitempty"`
	Workspace   string                `json:"workspace,omitempty"`
	Profile     string                `json:"profile"`
	HTTPAddr    string                `json:"http_addr"`
	WSAddr      string                `json:"ws_addr,omitempty"`
	ExtensionID string                `json:"extension_id,omitempty"`
	Transport   string                `json:"transport,omitempty"`
	Reachable   bool                  `json:"reachable"`
	Identity    *brwidentity.Identity `json:"identity,omitempty"`
	Error       string                `json:"error,omitempty"`
}

// Candidates returns every profile that exposes a control HTTP addr — extension
// bridge or direct-CDP. An empty path means the policy's normal discovery
// order. It iterates policy.Profiles directly, not ResolveProfile, because the
// point is to list ALL configured daemons rather than resolve one for a workspace.
func Candidates(policyPath string) ([]profilepolicy.Profile, error) {
	policy, err := profilepolicy.Load(policyPath)
	if err != nil {
		return nil, err
	}
	profiles := make([]profilepolicy.Profile, 0, len(policy.Profiles))
	for _, profile := range policy.Profiles {
		if !profile.ExtensionBridgeAllowed && !profile.DirectCDPAllowed {
			continue
		}
		profiles = append(profiles, profile)
	}
	return profiles, nil
}

// List probes every configured bridge daemon and returns one record each.
func List(policyPath string, timeout time.Duration) ([]Record, error) {
	profiles, err := Candidates(policyPath)
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(profiles))
	for _, profile := range profiles {
		records = append(records, Probe(profile, timeout))
	}
	return records, nil
}

// Probe builds the discovery record for one bridge profile: it derives the
// daemon's loopback addresses and extension id from the profile, then GETs the
// daemon's /health to fill in reachability and live identity. A probe failure is
// recorded (reachable=false + error), never fatal — an offline daemon must still
// appear in the listing so a consumer can decide whether to register it.
func Probe(profile profilepolicy.Profile, timeout time.Duration) Record {
	extID := profile.BridgeExtensionID
	if extID == "" {
		extID = profilepolicy.DefaultBridgeExtensionID
	}
	httpURL := HTTPURL(profile)
	rec := Record{
		Name:        profile.Name,
		Kind:        profile.Kind,
		Profile:     profile.Name,
		HTTPAddr:    httpURL,
		ExtensionID: extID,
		Transport:   transportOf(profile),
	}
	if profile.ExtensionBridgeAllowed {
		rec.WSAddr = WSAddr(profile)
	}
	ctrl, cerr := httpclient.New(httpURL, timeout)
	if cerr != nil {
		rec.Error = cerr.Error()
		return rec
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	health, herr := ctrl.Health(ctx)
	if herr != nil {
		rec.Error = herr.Error()
		return rec
	}
	rec.Reachable = true
	if !health.Identity.Empty() {
		id := health.Identity
		rec.Identity = &id
		rec.Workspace = health.Identity.Workspace
	}
	return rec
}

// WSAddr is the profile's extension-bridge WebSocket address.
func WSAddr(profile profilepolicy.Profile) string {
	if strings.TrimSpace(profile.BridgeWSAddr) != "" {
		return strings.TrimSpace(profile.BridgeWSAddr)
	}
	return "127.0.0.1:17311"
}

// HTTPURL is the profile's daemon control URL, scheme included.
func HTTPURL(profile profilepolicy.Profile) string {
	addr := strings.TrimSpace(profile.BridgeHTTPAddr)
	if addr == "" {
		addr = "127.0.0.1:17310"
	}
	if strings.Contains(addr, "://") {
		return addr
	}
	return "http://" + addr
}

func transportOf(profile profilepolicy.Profile) string {
	if profile.DirectCDPAllowed {
		return "direct-cdp"
	}
	if profile.ExtensionBridgeAllowed {
		return "extension-bridge"
	}
	return ""
}
