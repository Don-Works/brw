package plugin

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/Don-Works/brw/internal/credential"
)

// CapabilityCredentialRead lets a plugin answer one credential reference at a time.
const CapabilityCredentialRead = "credential.read"

// CapabilityBrowserProvider names the cloud/remote-browser backend capability.
const CapabilityBrowserProvider = "browser.provider"

var grantable = map[string]bool{
	CapabilityCredentialRead:  true,
	CapabilityBrowserProvider: true,
}

var refused = map[string]string{
	"cookies.export":            "brw performs no bulk cookie export on the signed-in extension transport; a plugin that could read them would turn brw into an exfiltration tool against the human's own profile",
	"storage.export":            "brw performs no bulk storage export on the signed-in extension transport, for the same reason it performs no cookie export",
	"command.run":               "brw runs no caller-supplied command; a plugin's argv is fixed in a manifest the operator wrote, and a capability that let anything else choose it would make every other guard irrelevant",
	"launch.mutate":             "Chrome's launch flags carry brw's sandbox and site-isolation settings; a plugin that could edit them could disable web security for every later page",
	"captcha.solve":             "solving means shipping page content to a third party, and brw sends page bytes nowhere the operator did not point it",
	"navigation.policy.disable": "the navigation allow/block policy is a guardrail an operator sets on the daemon; nothing loaded from a file in a directory removes it",
	"secret.store":              "brw stores no secret; a plugin that could write one back into brw would be the vault this design exists to avoid",
}

// ErrCapabilityNotGrantable is the single sentinel for every refused capability, so a caller can branch on the class without string matching.
var ErrCapabilityNotGrantable = errors.New("capability is not grantable")

// CheckCapability reports whether brw will grant a capability name.
func CheckCapability(name string) error {
	if grantable[name] {
		return nil
	}
	if why, ok := refused[name]; ok {
		return fmt.Errorf("%w: %q would widen a brw security default: %s", ErrCapabilityNotGrantable, name, why)
	}
	return fmt.Errorf("%w: %q is not a capability brw defines; the grantable set is %v", ErrCapabilityNotGrantable, name, GrantableCapabilities())
}

// GrantableCapabilities is the allowlist, for error text and for the docs test.
func GrantableCapabilities() []string {
	return slices.Sorted(maps.Keys(grantable))
}

// BrowserProvider is what CapabilityBrowserProvider grants.
type BrowserProvider interface {
	// OpenBrowserSession mints a session and returns it with the release function the daemon calls when it is finished with the browser.
	OpenBrowserSession(ctx context.Context) (BrowserSession, func(context.Context) error, error)
}

type browserProvider interface {
	open(ctx context.Context, secret credential.Secret) (BrowserSession, error)
	release(ctx context.Context, sessionID string, secret credential.Secret) error
}

type credentialProvider interface {
	resolve(ctx context.Context, reference string) (credential.Secret, error)
}
