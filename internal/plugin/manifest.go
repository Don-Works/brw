// Package plugin is brw's only extension point: an operator-installed program or directory, described by a JSON manifest, that the daemon may call for one narrowly defined job.
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

// MaxManifestBytes bounds one manifest file.
const MaxManifestBytes = 64 << 10

// CredentialKind values.
const (
	CredentialKindExec = "exec"
	CredentialKindFile = "file"
)

// CredentialKinds is the closed domain of provider backends, declared once so the validator, the loader and the test that enumerates them read from the same list.
var CredentialKinds = []string{CredentialKindExec, CredentialKindFile}

// BrowserKind values.
const BrowserKindExec = "exec"

// BrowserKinds is the closed domain of browser-provider backends.
var BrowserKinds = []string{BrowserKindExec}

// ReferenceToken is the single argv placeholder an exec provider substitutes.
const ReferenceToken = "{reference}"

// SessionToken is the argv placeholder a browser provider's teardown command substitutes with the id of the session being released.
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

// ParseManifest decodes one manifest strictly.
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

		problems = append(problems, validateBrowserCommand("browser teardown", spec.Teardown, 1))
	default:
		problems = append(problems, fmt.Errorf("browser kind must be one of %v", BrowserKinds))
	}
	return errors.Join(problems...)
}

const maxBrowserTimeoutMS = 60_000

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

		problems = append(problems, fmt.Errorf("%s must not contain the %s token; a browser provider receives its credential on stdin, not in its argv", what, ReferenceToken))
	}
	return errors.Join(problems...)
}

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

		problems = append(problems, fmt.Errorf("credential command must contain the %s token exactly once, found %d", ReferenceToken, tokens))
	}
	return errors.Join(problems...)
}

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
