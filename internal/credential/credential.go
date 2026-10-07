// Package credential carries one resolved secret from a capability-gated provider to exactly one browser fill, and nowhere else.
package credential

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Scheme is the placeholder prefix a recipe uses to name a credential without containing one.
const Scheme = "secret://"

// Placeholder replaces a value wherever a Secret would otherwise be rendered.
const Placeholder = "[redacted credential]"

// MaxValueBytes bounds what a provider may return.
const MaxValueBytes = 64 << 10

var (
	// ErrNoProvider is the fail-closed answer when a recipe names a credential and nothing is granted to resolve it.
	ErrNoProvider = errors.New("no credential provider is configured: no loaded plugin holds the credential.read capability")
	// ErrProviderRevoked separates "revoked" from "never configured" so an operator who just revoked a plugin recognises their own action.
	ErrProviderRevoked = errors.New("the credential provider was revoked")
	// ErrReferenceNotFound is the provider's answer for a name it does not hold.
	ErrReferenceNotFound = errors.New("credential reference not found")
	// ErrEmptyValue guards the case that reads as success and is not: a provider that printed nothing would otherwise clear the field it was asked to fill.
	ErrEmptyValue = errors.New("credential provider returned an empty value")
)

var referencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(?:/[A-Za-z0-9._-]+)*$`)

// Reference reports whether value is EXACTLY a credential reference, and returns the name it carries.
func Reference(value string) (string, bool) {
	if !strings.HasPrefix(value, Scheme) {
		return "", false
	}
	name := value[len(Scheme):]
	if ValidateReference(name) != nil {
		return "", false
	}
	return name, true
}

// HasSchemePrefix reports whether value contains the credential scheme anywhere.
func HasSchemePrefix(value string) bool { return strings.Contains(value, Scheme) }

func ValidateReference(name string) error {
	if name == "" {
		return errors.New("credential reference name is empty")
	}
	if len(name) > 192 {
		return errors.New("credential reference name is too long")
	}
	if !referencePattern.MatchString(name) {
		return fmt.Errorf("credential reference %q must be slash-separated segments of letters, digits, dot, dash or underscore", name)
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("credential reference %q may not contain a relative path segment", name)
		}
	}
	return nil
}

// Secret is a resolved credential value.
type Secret struct {
	value []byte
}

// New copies value into a Secret.
func New(value string) Secret {
	return Secret{value: []byte(value)}
}

// Reveal returns the value for the one caller allowed to use it: the browser fill path.
func (s Secret) Reveal() string { return string(s.value) }

func (s Secret) Empty() bool { return len(s.value) == 0 }

// Wipe zeroes the backing array.
func (s *Secret) Wipe() {
	clear(s.value)
	s.value = nil
}

// The rendering paths below all redact.
func (s Secret) String() string   { return Placeholder }
func (s Secret) GoString() string { return Placeholder }

func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"` + Placeholder + `"`), nil
}

// Validate applies the bounds every provider shares, so a new provider kind cannot forget one.
func Validate(raw []byte) (Secret, error) {
	if len(raw) > MaxValueBytes {
		return Secret{}, fmt.Errorf("credential provider returned more than %d bytes", MaxValueBytes)
	}
	trimmed := raw

	trimmed = bytes.TrimSuffix(trimmed, []byte("\n"))
	trimmed = bytes.TrimSuffix(trimmed, []byte("\r"))
	if len(trimmed) == 0 {
		return Secret{}, ErrEmptyValue
	}
	if !utf8.Valid(trimmed) {
		return Secret{}, errors.New("credential provider returned invalid UTF-8")
	}
	for _, b := range trimmed {
		if b == 0 || b == '\n' || b == '\r' {
			return Secret{}, errors.New("credential value contains a line break or NUL; a provider must print one single-line value")
		}
	}
	return Secret{value: bytes.Clone(trimmed)}, nil
}

// Resolver turns a reference name into a value.
type Resolver interface {
	Resolve(ctx context.Context, reference string) (Secret, error)
}

// Prober is an optional Resolver capability: reporting whether a resolution could succeed at all, WITHOUT resolving anything.
type Prober interface {
	ProbeProvider() error
}

// Probe reports whether resolver could answer.
func Probe(resolver Resolver) error {
	if resolver == nil {
		return ErrNoProvider
	}
	if prober, ok := resolver.(Prober); ok {
		return prober.ProbeProvider()
	}
	return nil
}

// Scrub replaces every occurrence of the secret in an error's text with the placeholder.
func Scrub(err error, secret Secret) error {
	if err == nil || secret.Empty() {
		return err
	}
	message := ScrubString(err.Error(), secret)
	if message == err.Error() {
		return err
	}
	return errors.New(message)
}

// ScrubString removes the value from message in every form a transport is likely to have written it in.
func ScrubString(message string, secret Secret) string {
	if secret.Empty() {
		return message
	}
	for _, form := range encodedForms(secret.Reveal()) {
		message = strings.ReplaceAll(message, form, Placeholder)
	}
	return message
}

func encodedForms(value string) []string {
	quoted := strconv.Quote(value)
	candidates := []string{value, quoted[1 : len(quoted)-1], url.QueryEscape(value), url.PathEscape(value)}
	if encoded, err := json.Marshal(value); err == nil && len(encoded) >= 2 {
		candidates = append(candidates, string(encoded[1:len(encoded)-1]))
	}
	forms := make([]string, 0, len(candidates))
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		forms = append(forms, candidate)
	}
	sort.SliceStable(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	return forms
}
