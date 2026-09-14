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

// ReferenceToken is the single argv placeholder an exec provider substitutes.
const ReferenceToken = "{reference}"

type Manifest struct {
	SchemaVersion int      `json:"schema_version"`
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Version       string   `json:"version"`
	Description   string   `json:"description"`
	Capabilities  []string `json:"capabilities"`
	// Credential is required if and only if credential.read is declared.
	Credential *CredentialProviderSpec `json:"credential,omitempty"`
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
	problems = append(problems, validateCredentialProgram(command[0]))
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

// validateCredentialProgram pins which binary a manifest names.
//
// A bare name such as "op" is resolved from the daemon's PATH at every call, so
// the manifest an operator reviewed does not decide what runs: whoever controls
// PATH, or can write an earlier directory on it, does. The path must also be
// already clean, because "/usr/bin/../../tmp/op" reads as a reviewed system
// binary and is not one, and it must not carry the reference token, which would
// let the caller's reference name spell a different program than the one the
// loader checked. The program's mode, owner and ancestors are checked
// separately at load, where the filesystem is available.
func validateCredentialProgram(program string) error {
	if strings.TrimSpace(program) == "" {
		return errors.New("credential command program is empty")
	}
	if !filepath.IsAbs(program) {
		return fmt.Errorf("credential command program %q must be an absolute path, so the reviewed manifest decides which binary runs rather than the daemon's PATH", program)
	}
	if filepath.Clean(program) != program {
		return fmt.Errorf("credential command program %q must already be a clean path, with no %q or %q segment", program, ".", "..")
	}
	if strings.Contains(program, ReferenceToken) {
		return fmt.Errorf("credential command program %q must not contain the %s token; the program is what the loader pins, so a reference must not be able to name a different one", program, ReferenceToken)
	}
	return nil
}

// substituteReference builds the argv for one resolve. It replaces the token in
// place rather than appending, so an argv like ["op","read","op://{reference}"]
// keeps its scheme prefix. No shell is involved at any point.
func substituteReference(command []string, reference string) []string {
	argv := make([]string, len(command))
	for index, argument := range command {
		argv[index] = strings.ReplaceAll(argument, ReferenceToken, reference)
	}
	return argv
}
