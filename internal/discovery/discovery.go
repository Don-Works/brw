// Package discovery is brw's single profile-daemon discovery path: it turns the profile policy into the set of configured bridge daemons and probes each daemon's /health.
package discovery

import (
	"context"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/profilepolicy"
)

// Record is one configured browser-profile bridge daemon, as emitted by `brwctl daemons`.
type Record struct {
	Name        string                `json:"name"`
	Kind        string                `json:"kind,omitempty"`
	Workspace   string                `json:"workspace,omitempty"`
	Profile     string                `json:"profile"`
	HTTPAddr    string                `json:"http_addr"`
	WSAddr      string                `json:"ws_addr"`
	ExtensionID string                `json:"extension_id,omitempty"`
	Reachable   bool                  `json:"reachable"`
	Identity    *brwidentity.Identity `json:"identity,omitempty"`
	Error       string                `json:"error,omitempty"`
}

// Candidates returns every extension-bridge profile in the policy at path (an empty path means the policy's normal discovery order).
func Candidates(policyPath string) ([]profilepolicy.Profile, error) {
	policy, err := profilepolicy.Load(policyPath)
	if err != nil {
		return nil, err
	}
	profiles := make([]profilepolicy.Profile, 0, len(policy.Profiles))
	for _, profile := range policy.Profiles {
		if !profile.ExtensionBridgeAllowed {
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

// Probe builds the discovery record for one bridge profile: it derives the daemon's loopback addresses and extension id from the profile, then GETs the daemon's /health to fill in reachability and live identity.
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
		WSAddr:      WSAddr(profile),
		ExtensionID: extID,
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
