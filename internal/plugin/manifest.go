// Package plugin is brw's only extension point: an operator-installed program
// or directory, described by a JSON manifest, that the daemon may call for one
// narrowly defined job.
//
// The capability set is a closed allowlist (see capability.go). brw does not
// sandbox a plugin — an exec provider runs as the daemon's own user — so the
// trust boundary is the permission on the plugin directory, and the loader
// enforces the parts of that boundary it can see. docs/plugins.md states the
// model in full, including what it deliberately does not promise.
package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/Don-Works/brw/internal/credential"
)

const ManifestSchemaVersion = 1

// MaxManifestBytes bounds one manifest file. A manifest is a dozen short
// fields; anything larger is a mistake or an attempt to make the parser work.
const MaxManifestBytes = 64 << 10

// CredentialKind values.
const (
	CredentialKindExec = "exec"
	CredentialKindFile = "file"
)

// CredentialKinds is the closed domain of provider backends, declared once so
// the validator, the loader and the test that enumerates them read from the
// same list. Every kind on it reaches the same capability, so every kind has to
// be classified by both switches over Kind and held to the same trust boundary;
// a sibling added to one of them and missed by the other is how a gate stops
// covering the thing it gates.
var CredentialKinds = []string{CredentialKindExec, CredentialKindFile}

// BrowserKind values.
const BrowserKindExec = "exec"

// BrowserKinds is the closed domain of browser-provider backends. One kind
// ships: a program the operator wrote that mints a session against whatever
// cloud service they use. Six named services would be six auth stories and six
// breakage surfaces inside brw; the capability carries them instead.
//
// Declared once, for the same reason CredentialKinds is: every kind on it
// reaches the same capability, so a sibling added to one switch over Kind and
// missed by another is how a gate stops covering what it gates. A test
// enumerates this list.
var BrowserKinds = []string{BrowserKindExec}

// ReferenceToken is the single argv placeholder an exec provider substitutes.
const ReferenceToken = "{reference}"

// SessionToken is the argv placeholder a browser provider's teardown command
// substitutes with the id of the session being released. Deliberately not
// ReferenceToken: a teardown argv is built from a value the PROVIDER printed,
// and reusing the credential token would make "which substitution is this?"
// a question the reader has to answer from context.
const SessionToken = "{session}"

type Manifest struct {
	SchemaVersion int      `json:"schema_version"`
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Version       string   `json:"version"`
	Description   string   `json:"description"`
	Capabilities  []string `json:"capabilities"`
	// Credential is required if and only if credential.read is declared.
	Credential *CredentialProviderSpec `json:"credential,omitempty"`
	// Browser is required if and only if browser.provider is declared.
	Browser *BrowserProviderSpec `json:"browser,omitempty"`
}

// BrowserProviderSpec configures the one browser-provider backend brw ships.
//
// Command mints a session and prints one JSON envelope (see ParseSessionEnvelope).
// Teardown releases it and must carry SessionToken exactly once, so the release
// is aimed at the session that was opened rather than at whatever the provider
// considers current. Credential names a reference resolved through the
// credential.read holder at the moment of the call and written to the program's
// STDIN — never its argv, which every process on the machine can read.
type BrowserProviderSpec struct {
	Kind       string   `json:"kind"`
	Command    []string `json:"command,omitempty"`
	Teardown   []string `json:"teardown,omitempty"`
	Credential string   `json:"credential,omitempty"`
	TimeoutMS  int      `json:"timeout_ms,omitempty"`
}

type CredentialProviderSpec struct {
	Kind      string   `json:"kind"`
	Command   []string `json:"command,omitempty"`
	Directory string   `json:"directory,omitempty"`
	TimeoutMS int      `json:"timeout_ms,omitempty"`
}

var (
	pluginIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)+$`)
	versionPattern  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][a-zA-Z0-9.-]+)?$`)
)

// ParseManifest decodes one manifest strictly. Unknown fields are refused so a
// misspelled setting is an error rather than a silently ignored one — the
// difference between "my timeout is not applying" and a plugin running with a
// default the operator never chose.
func ParseManifest(data []byte) (Manifest, error) {
	if len(data) > MaxManifestBytes {
		return Manifest{}, fmt.Errorf("plugin manifest exceeds %d bytes", MaxManifestBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var manifest Manifest
	if err := dec.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("plugin manifest is not valid JSON: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("plugin manifest contains trailing JSON")
	}
	if err := ValidateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func ValidateManifest(manifest Manifest) error {
	var problems []error
	if manifest.SchemaVersion != ManifestSchemaVersion {
		problems = append(problems, fmt.Errorf("schema_version must be %d", ManifestSchemaVersion))
	}
	if !pluginIDPattern.MatchString(manifest.ID) {
		problems = append(problems, errors.New("id must be a dotted lowercase identifier such as onepassword.cli"))
	}
	if !versionPattern.MatchString(manifest.Version) {
		problems = append(problems, errors.New("version must be semantic x.y.z"))
	}
	if strings.TrimSpace(manifest.Name) == "" || strings.TrimSpace(manifest.Description) == "" {
		problems = append(problems, errors.New("name and description are required"))
	}
	if len(manifest.Name) > 200 || len(manifest.Description) > 2000 {
		problems = append(problems, errors.New("name or description is too long"))
	}
	if len(manifest.Capabilities) == 0 {
		problems = append(problems, errors.New("a plugin must declare at least one capability"))
	}
	if len(manifest.Capabilities) > 16 {
		problems = append(problems, errors.New("at most 16 capabilities are allowed"))
	}
	seen := map[string]bool{}
	for _, capability := range manifest.Capabilities {
		if seen[capability] {
			problems = append(problems, fmt.Errorf("capability %q is declared twice", capability))
			continue
		}
		seen[capability] = true
		if err := CheckCapability(capability); err != nil {
			problems = append(problems, err)
		}
	}
	wantsCredential := slices.Contains(manifest.Capabilities, CapabilityCredentialRead)
	switch {
	case wantsCredential && manifest.Credential == nil:
		problems = append(problems, errors.New("credential.read requires a credential block naming the provider"))
	case !wantsCredential && manifest.Credential != nil:
		// A configured provider with no grant reads as working and is not. The
		// grant is the thing the operator reviews, so it is the thing required.
		problems = append(problems, errors.New("a credential block requires the credential.read capability"))
	case wantsCredential:
		if err := validateCredentialSpec(*manifest.Credential); err != nil {
			problems = append(problems, err)
		}
	}
	wantsBrowser := slices.Contains(manifest.Capabilities, CapabilityBrowserProvider)
	switch {
	case wantsBrowser && manifest.Browser == nil:
		problems = append(problems, errors.New("browser.provider requires a browser block naming the backend"))
	case !wantsBrowser && manifest.Browser != nil:
		// Same reason as the credential half: a configured backend with no grant
		// reads as working and is not, and the grant is what an operator reviews.
		problems = append(problems, errors.New("a browser block requires the browser.provider capability"))
	case wantsBrowser:
		if err := validateBrowserSpec(*manifest.Browser); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

func validateBrowserSpec(spec BrowserProviderSpec) error {
	var problems []error
	if spec.TimeoutMS < 0 || spec.TimeoutMS > maxBrowserTimeoutMS {
		problems = append(problems, fmt.Errorf("browser timeout_ms must be between 0 and %d", maxBrowserTimeoutMS))
	}
	if spec.Credential != "" {
		if err := credential.ValidateReference(spec.Credential); err != nil {
			problems = append(problems, err)
		}
	}
	switch spec.Kind {
	case BrowserKindExec:
		problems = append(problems, validateBrowserCommand("browser command", spec.Command, 0))
		// Exactly one, for the reason the credential command needs exactly one
		// reference: a teardown that never sees the session id releases whatever
		// the provider decides is current, which on a shared account is somebody
		// else's browser.
		problems = append(problems, validateBrowserCommand("browser teardown", spec.Teardown, 1))
	default:
		problems = append(problems, fmt.Errorf("browser kind must be one of %v", BrowserKinds))
	}
	return errors.Join(problems...)
}

// maxBrowserTimeoutMS bounds how long a mint or a teardown may take. A provider
// that can block for an hour blocks the daemon's startup behind it.
const maxBrowserTimeoutMS = 60_000

// validateBrowserCommand holds a browser provider's argv to the same rules the
// credential one lives under, and pins how many SessionToken placeholders it
// must carry: none for the mint, which has no session yet, and exactly one for
// the teardown, which is aimed at a specific one.
func validateBrowserCommand(what string, command []string, wantSessionTokens int) error {
	if len(command) == 0 {
		return fmt.Errorf("the exec browser kind requires a %s", what)
	}
	var problems []error
	if len(command) > 32 {
		problems = append(problems, fmt.Errorf("%s has too many arguments", what))
	}
	problems = append(problems, validateProgramPath(what+" program", command[0]))
	sessionTokens, referenceTokens, oversized := 0, 0, false
	for _, argument := range command {
		if len(argument) > 4096 {
			oversized = true
		}
		sessionTokens += strings.Count(argument, SessionToken)
		referenceTokens += strings.Count(argument, ReferenceToken)
	}
	if oversized {
		problems = append(problems, fmt.Errorf("%s argument is too long", what))
	}
	if sessionTokens != wantSessionTokens {
		problems = append(problems, fmt.Errorf("%s must contain the %s token exactly %d times, found %d", what, SessionToken, wantSessionTokens, sessionTokens))
	}
	if referenceTokens != 0 {
		// The credential reaches this provider on stdin, never in its argv. A
		// manifest that spells the credential token here is asking for a
		// substitution that will not happen, and shipping it would leave an
		// operator believing their key was passed.
		problems = append(problems, fmt.Errorf("%s must not contain the %s token; a browser provider receives its credential on stdin, not in its argv", what, ReferenceToken))
	}
	return errors.Join(problems...)
}

// maxCredentialTimeoutMS bounds the deadline an operator can hand a provider.
// A provider that can block for an hour blocks the recipe step behind it.
const maxCredentialTimeoutMS = 60_000

func validateCredentialSpec(spec CredentialProviderSpec) error {
	var problems []error
	if spec.TimeoutMS < 0 || spec.TimeoutMS > maxCredentialTimeoutMS {
		problems = append(problems, fmt.Errorf("credential timeout_ms must be between 0 and %d", maxCredentialTimeoutMS))
	}
	switch spec.Kind {
	case CredentialKindExec:
		if spec.Directory != "" {
			problems = append(problems, errors.New("credential directory is only valid for the file kind"))
		}
		problems = append(problems, validateCredentialCommand(spec.Command))
	case CredentialKindFile:
		if len(spec.Command) != 0 {
			problems = append(problems, errors.New("credential command is only valid for the exec kind"))
		}
		if strings.TrimSpace(spec.Directory) == "" {
			problems = append(problems, errors.New("the file credential kind requires a directory"))
		}
	default:
		problems = append(problems, fmt.Errorf("credential kind must be one of %v", CredentialKinds))
	}
	return errors.Join(problems...)
}

func validateCredentialCommand(command []string) error {
	if len(command) == 0 {
		return errors.New("the exec credential kind requires a command")
	}
	var problems []error
	if len(command) > 32 {
		problems = append(problems, errors.New("credential command has too many arguments"))
	}
	problems = append(problems, validateProgramPath("credential command program", command[0]))
	tokens, oversized := 0, false
	for _, argument := range command {
		if len(argument) > 4096 {
			oversized = true
		}
		tokens += strings.Count(argument, ReferenceToken)
	}
	if oversized {
		problems = append(problems, errors.New("credential command argument is too long"))
	}
	if tokens != 1 {
		// Zero is the dangerous one: a provider that never sees the reference
		// answers every request with the same secret, and the recipe that asked
		// for the staging password gets production's.
		problems = append(problems, fmt.Errorf("credential command must contain the %s token exactly once, found %d", ReferenceToken, tokens))
	}
	return errors.Join(problems...)
}

// validateProgramPath pins which binary a manifest names. Shared by every
// provider kind that execs, because the reason is the same for all of them and
// a second copy of this rule is a second place for it to go stale.
//
// A bare name such as "op" is resolved from the daemon's PATH at every call, so
// the manifest an operator reviewed does not decide what runs: whoever controls
// PATH, or can write an earlier directory on it, does. The path must also be
// already clean, because "/usr/bin/../../tmp/op" reads as a reviewed system
// binary and is not one, and it must carry NEITHER substitution token, which
// would let a reference name or a provider-printed session id spell a different
// program than the one the loader checked. The program's mode, owner and
// ancestors are checked separately at load, where the filesystem is available.
func validateProgramPath(what, program string) error {
	if strings.TrimSpace(program) == "" {
		return fmt.Errorf("%s is empty", what)
	}
	if !filepath.IsAbs(program) {
		return fmt.Errorf("%s %q must be an absolute path, so the reviewed manifest decides which binary runs rather than the daemon's PATH", what, program)
	}
	if filepath.Clean(program) != program {
		return fmt.Errorf("%s %q must already be a clean path, with no %q or %q segment", what, program, ".", "..")
	}
	for _, token := range []string{ReferenceToken, SessionToken} {
		if strings.Contains(program, token) {
			return fmt.Errorf("%s %q must not contain the %s token; the program is what the loader pins, so a substitution must not be able to name a different one", what, program, token)
		}
	}
	return nil
}

// substituteToken builds the argv for one call. It replaces the token in place
// rather than appending, so an argv like ["op","read","op://{reference}"] keeps
// its scheme prefix. No shell is involved at any point.
func substituteToken(command []string, token, value string) []string {
	argv := make([]string, len(command))
	for index, argument := range command {
		argv[index] = strings.ReplaceAll(argument, token, value)
	}
	return argv
}

func substituteReference(command []string, reference string) []string {
	return substituteToken(command, ReferenceToken, reference)
}
