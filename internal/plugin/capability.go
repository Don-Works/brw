package plugin

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/Don-Works/brw/internal/credential"
)

// CapabilityCredentialRead lets a plugin answer one credential reference at a
// time.
const CapabilityCredentialRead = "credential.read"

// CapabilityBrowserProvider names the cloud/remote-browser backend capability.
// A plugin holding it answers with a CDP websocket URL, a session lifetime and
// a teardown hook; brw owns everything above that socket.
const CapabilityBrowserProvider = "browser.provider"

// grantable is a CLOSED allowlist, matched byte for byte. It, not the refusal
// table below, is what stops an attacker-supplied capability: a name that is
// not a key here is refused whether or not anyone anticipated it.
var grantable = map[string]bool{
	CapabilityCredentialRead:  true,
	CapabilityBrowserProvider: true,
}

// reserved names a capability with a defined interface shape that brw cannot
// grant yet. Separated from "never" so the error tells an operator to wait
// rather than to give up.
//
// It is empty today: browser.provider, the one entry it ever held, is granted
// now that a backend honours it. The map stays because the distinction is real
// and the next reserved name should land here rather than in "never".
var reserved = map[string]string{}

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

// BrowserProvider is what CapabilityBrowserProvider grants. A provider hands
// brw a CDP websocket URL for a browser it owns, a lifetime for that browser,
// and a way to give it back. Everything above the socket stays brw's: the
// navigation policy, the containment boundary, the site-consent gate and the
// identity guard are the same code on a remote target as on a local one.
type BrowserProvider interface {
	// OpenBrowserSession mints a session and returns it with the release
	// function the daemon calls when it is finished with the browser.
	OpenBrowserSession(ctx context.Context) (BrowserSession, func(context.Context) error, error)
}

// browserProvider is the internal shape of a granted browser.provider plugin.
// It is as narrow as credentialProvider on purpose: a provider is asked to open
// a session and told to release one, and has no way to ask brw for a page, a
// cookie, a policy, or anything else.
//
// The credential it needs is HANDED to it, already resolved. brw looks the
// reference up through the credential.read holder — the wave-3 mechanism — so
// a browser provider is not a second way to reach a secret store.
type browserProvider interface {
	open(ctx context.Context, secret credential.Secret) (BrowserSession, error)
	release(ctx context.Context, sessionID string, secret credential.Secret) error
}

// credentialProvider is the internal shape of a granted credential.read plugin.
// It is deliberately narrower than credential.Resolver's public surface: a
// provider is handed a reference name and returns a value, and has no way to
// ask brw for anything.
type credentialProvider interface {
	resolve(ctx context.Context, reference string) (credential.Secret, error)
}
