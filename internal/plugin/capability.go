package plugin

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/Don-Works/brw/internal/credential"
)

// CapabilityCredentialRead lets a plugin answer one credential reference at a
// time. It is the only capability brw grants.
const CapabilityCredentialRead = "credential.read"

// CapabilityBrowserProvider names the cloud-browser backend capability. The Go
// interface shape below exists for the work that will implement it; the loader
// refuses the name today, because advertising a capability nothing honours is
// a claim brw cannot back.
const CapabilityBrowserProvider = "browser.provider"

// grantable is a CLOSED allowlist, matched byte for byte. It, not the refusal
// table below, is what stops an attacker-supplied capability: a name that is
// not a key here is refused whether or not anyone anticipated it.
var grantable = map[string]bool{
	CapabilityCredentialRead: true,
}

// reserved names a capability with a defined interface shape that brw cannot
// grant yet. Separated from "never" so the error tells an operator to wait
// rather than to give up.
var reserved = map[string]string{
	CapabilityBrowserProvider: "brw has no plugin-supplied browser backend yet; the interface shape exists but nothing honours the grant",
}

// refused maps a capability brw will never grant to the security default it
// would widen. This table only improves the message — an unknown capability is
// refused identically by the allowlist above.
var refused = map[string]string{
	"cookies.export":            "brw performs no bulk cookie export on the signed-in extension transport; a plugin that could read them would turn brw into an exfiltration tool against the human's own profile",
	"storage.export":            "brw performs no bulk storage export on the signed-in extension transport, for the same reason it performs no cookie export",
	"command.run":               "brw runs no caller-supplied command; a plugin's argv is fixed in a manifest the operator wrote, and a capability that let anything else choose it would make every other guard irrelevant",
	"launch.mutate":             "Chrome's launch flags carry brw's sandbox and site-isolation settings; a plugin that could edit them could disable web security for every later page",
	"captcha.solve":             "solving means shipping page content to a third party, and brw sends page bytes nowhere the operator did not point it",
	"navigation.policy.disable": "the navigation allow/block policy is a guardrail an operator sets on the daemon; nothing loaded from a file in a directory removes it",
	"secret.store":              "brw stores no secret; a plugin that could write one back into brw would be the vault this design exists to avoid",
}

// ErrCapabilityNotGrantable is the single sentinel for every refused
// capability, so a caller can branch on the class without string matching.
var ErrCapabilityNotGrantable = errors.New("capability is not grantable")

// CheckCapability reports whether brw will grant a capability name.
//
// The comparison is exact on purpose. No trimming, no case folding, no prefix
// or suffix matching: "credential.read " and "credential.readonly" are
// different names and both unknown. A normalisation step here is exactly how an
// allowlist stops being one.
func CheckCapability(name string) error {
	if grantable[name] {
		return nil
	}
	if why, ok := reserved[name]; ok {
		return fmt.Errorf("%w: %q is reserved: %s", ErrCapabilityNotGrantable, name, why)
	}
	if why, ok := refused[name]; ok {
		return fmt.Errorf("%w: %q would widen a brw security default: %s", ErrCapabilityNotGrantable, name, why)
	}
	return fmt.Errorf("%w: %q is not a capability brw defines; the grantable set is %v", ErrCapabilityNotGrantable, name, GrantableCapabilities())
}

// GrantableCapabilities is the allowlist, for error text and for the docs test.
func GrantableCapabilities() []string {
	names := make([]string, 0, len(grantable))
	for name := range grantable {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// BrowserProvider is the interface shape the cloud-browser work will implement
// behind CapabilityBrowserProvider. It is declared here so that task has a
// contract to build against; the loader refuses the capability, so nothing can
// be reached through it today.
type BrowserProvider interface {
	// Endpoint returns a CDP websocket URL for a browser the plugin owns, plus
	// a release function the daemon calls when it is finished with it.
	Endpoint(ctx context.Context) (wsURL string, release func(context.Context) error, err error)
}

// credentialProvider is the internal shape of a granted credential.read plugin.
// It is deliberately narrower than credential.Resolver's public surface: a
// provider is handed a reference name and returns a value, and has no way to
// ask brw for anything.
type credentialProvider interface {
	resolve(ctx context.Context, reference string) (credential.Secret, error)
}
