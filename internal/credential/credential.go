// Package credential carries one resolved secret from a capability-gated
// provider to exactly one browser fill, and nowhere else.
//
// brw is not a secret store. Nothing here persists, caches, or indexes a value:
// a Secret is produced at dispatch, handed to one browser actuation, and wiped
// when that actuation returns. Every stringification path on Secret redacts, so
// a value that leaks into a struct someone later marshals still does not reach
// a transcript.
package credential

import (
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

// Scheme is the placeholder prefix a recipe uses to name a credential without
// containing one.
const Scheme = "secret://"

// Placeholder replaces a value wherever a Secret would otherwise be rendered.
const Placeholder = "[redacted credential]"

// MaxValueBytes bounds what a provider may return. A provider that prints a
// file, a help text, or a stack trace instead of a secret is a bug, and typing
// a megabyte into a form field is never the recovery.
const MaxValueBytes = 64 << 10

var (
	// ErrNoProvider is the fail-closed answer when a recipe names a credential
	// and nothing is granted to resolve it.
	ErrNoProvider = errors.New("no credential provider is configured: no loaded plugin holds the credential.read capability")
	// ErrProviderRevoked separates "revoked" from "never configured" so an
	// operator who just revoked a plugin recognises their own action.
	ErrProviderRevoked = errors.New("the credential provider was revoked")
	// ErrReferenceNotFound is the provider's answer for a name it does not hold.
	ErrReferenceNotFound = errors.New("credential reference not found")
	// ErrEmptyValue guards the case that reads as success and is not: a provider
	// that printed nothing would otherwise clear the field it was asked to fill.
	ErrEmptyValue = errors.New("credential provider returned an empty value")
)

// referencePattern is deliberately narrow. The name travels into an argv slot
// and, for the file provider, into a path, so it carries no spaces, no shell
// metacharacters, and no percent or backslash escapes.
var referencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(?:/[A-Za-z0-9._-]+)*$`)

// Reference reports whether value is EXACTLY a credential reference, and
// returns the name it carries.
//
// Exactly is the whole point. A value that merely starts with the scheme, or
// embeds it, is not a reference: it is text, and the caller types it literally.
// A prefix match here would let "secret://x and also ${input:y}" resolve.
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
// Validation uses it to refuse a near-miss loudly rather than typing it.
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
//
// It holds a byte slice rather than a string so Wipe can actually zero it.
// Copies share that backing array on purpose: wiping the value the runner holds
// wipes every copy of the Secret, which is the behaviour a defer at the end of
// a step needs.
type Secret struct {
	value []byte
}

// New copies value into a Secret. The caller's own string is untouched and, if
// it came from a string, cannot be wiped — see Reveal.
func New(value string) Secret {
	return Secret{value: []byte(value)}
}

// Reveal returns the value for the one caller allowed to use it: the browser
// fill path. The returned string is a fresh Go string and is NOT wipeable, so
// call it at the actuation and never store the result.
func (s Secret) Reveal() string { return string(s.value) }

func (s Secret) Empty() bool { return len(s.value) == 0 }

// Wipe zeroes the backing array. It is not a guarantee that no copy survives —
// Reveal makes an unwipeable one — but it removes brw's own retained copy,
// which is the copy a walk of the daemon's state would otherwise find.
func (s *Secret) Wipe() {
	for i := range s.value {
		s.value[i] = 0
	}
	s.value = nil
}

// The rendering paths below all redact. fmt routes %v, %s, %q, %x and %X
// through Stringer, so a Secret that reaches a log line, a
// formatted error, a JSON result or a debugger dump renders as the placeholder.
func (s Secret) String() string   { return Placeholder }
func (s Secret) GoString() string { return Placeholder }

func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"` + Placeholder + `"`), nil
}

// Validate applies the bounds every provider shares, so a new provider kind
// cannot forget one. It is called by the providers, not by their callers.
func Validate(raw []byte) (Secret, error) {
	if len(raw) > MaxValueBytes {
		return Secret{}, fmt.Errorf("credential provider returned more than %d bytes", MaxValueBytes)
	}
	trimmed := raw
	// One trailing line ending, so `op read` and a text file both work. Anything
	// further inside the value is refused below rather than silently joined.
	trimmed = trimSuffix(trimmed, '\n')
	trimmed = trimSuffix(trimmed, '\r')
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
	value := make([]byte, len(trimmed))
	copy(value, trimmed)
	return Secret{value: value}, nil
}

func trimSuffix(raw []byte, b byte) []byte {
	if len(raw) > 0 && raw[len(raw)-1] == b {
		return raw[:len(raw)-1]
	}
	return raw
}

// Resolver turns a reference name into a value. The daemon holds exactly one,
// supplied by the plugin registry; it is never reachable from an MCP tool.
type Resolver interface {
	Resolve(ctx context.Context, reference string) (Secret, error)
}

// Prober is an optional Resolver capability: reporting whether a resolution
// could succeed at all, WITHOUT resolving anything.
//
// It exists so a caller can fail a credential-bearing flow before its first
// side effect. Without it, a daemon whose provider was revoked discovers that
// at the password field, with the username already typed and a half-filled
// login form left on the page.
type Prober interface {
	ProbeProvider() error
}

// Probe reports whether resolver could answer. A resolver that cannot say is
// treated as able to answer: refusing on "I do not know" would break every
// third-party Resolver that has no probe to offer.
func Probe(resolver Resolver) error {
	if resolver == nil {
		return ErrNoProvider
	}
	if prober, ok := resolver.(Prober); ok {
		return prober.ProbeProvider()
	}
	return nil
}

// Scrub replaces every occurrence of the secret in an error's text with the
// placeholder. A browser transport that quotes the value it failed to type is
// the realistic leak path into a run result and a failure bundle, and this is
// where that path is cut.
//
// A scrubbed error deliberately does NOT wrap the original. The original's own
// Error() still holds the value, so a wrapped chain would keep a live copy in
// the daemon's heap for anything walking errors.Unwrap to find — which is the
// exact retention this package exists to avoid. An error whose text never
// contained the value is returned untouched and keeps its chain.
func Scrub(err error, secret Secret) error {
	if err == nil || secret.Empty() {
		return err
	}
	message := ScrubString(err.Error(), secret)
	if message == err.Error() {
		return err
	}
	return scrubbedError{message: message}
}

// ScrubString removes the value from message in every form a transport is
// likely to have written it in.
//
// A raw substring replace is not enough. The browser transports quote the text
// they failed to type: internal/snapshot marshals it into a JSON expression,
// and %q-formatted Go errors quote it too. A value containing a quote, a
// backslash or a character a URL escapes appears in those forms in an encoded
// shape that shares no substring with the raw one, and the raw replace walks
// straight past it.
//
// This is a net, not a proof. An encoding nothing here anticipates still gets
// through, which is why the value is also never given to anything that keeps it
// — scrubbing is the last line, not the boundary.
func ScrubString(message string, secret Secret) string {
	if secret.Empty() {
		return message
	}
	for _, form := range encodedForms(secret.Reveal()) {
		message = strings.ReplaceAll(message, form, Placeholder)
	}
	return message
}

// encodedForms returns the distinct renderings of value, longest first so a
// form that contains a shorter one is replaced before the placeholder breaks it
// up.
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

type scrubbedError struct {
	message string
}

func (e scrubbedError) Error() string { return e.message }
