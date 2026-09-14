package credential

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// fixtureValue is deliberately low-entropy and obviously fake. Every assertion
// below searches for this literal, so a real-looking string here would be a
// secret-shaped token committed to a public repository for no benefit.
const fixtureValue = "fixture-login-value-one"

func TestReferenceAcceptsOnlyAnExactWholeValue(t *testing.T) {
	for name, test := range map[string]struct {
		value string
		want  string
		ok    bool
	}{
		"plain name":              {"secret://login", "login", true},
		"slash segments":          {"secret://work/login/field", "work/login/field", true},
		"dots and dashes":         {"secret://work.vault/my-login_1", "work.vault/my-login_1", true},
		"not a reference":         {"fixture-typed-text", "", false},
		"scheme only":             {"secret://", "", false},
		"leading text":            {"please type secret://login", "", false},
		"trailing text":           {"secret://login and more", "", false},
		"embedded space":          {"secret://work login", "", false},
		"parent segment":          {"secret://../../etc/passwd", "", false},
		"dot segment":             {"secret://work/./login", "", false},
		"double slash":            {"secret://work//login", "", false},
		"trailing slash":          {"secret://work/", "", false},
		"leading slash":           {"secret:///work", "", false},
		"uppercase scheme":        {"SECRET://login", "", false},
		"shell metacharacter":     {"secret://login;id", "", false},
		"query appended":          {"secret://login?x=1", "", false},
		"template appended":       {"secret://login${input:x}", "", false},
		"second scheme appended":  {"secret://login secret://other", "", false},
		"percent escape":          {"secret://log%2Fin", "", false},
		"backslash escape":        {`secret://work\login`, "", false},
		"newline inside the name": {"secret://login\nother", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := Reference(test.value)
			if got != test.want || ok != test.ok {
				t.Fatalf("Reference(%q) = %q,%v want %q,%v", test.value, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestValidateBoundsWhatAProviderMayReturn(t *testing.T) {
	for name, test := range map[string]struct {
		raw     string
		want    string
		wantErr string
	}{
		"plain":                 {fixtureValue, fixtureValue, ""},
		"one trailing newline":  {fixtureValue + "\n", fixtureValue, ""},
		"trailing crlf":         {fixtureValue + "\r\n", fixtureValue, ""},
		"interior newline":      {"one\ntwo", "", "line break"},
		"leading newline":       {"\n" + fixtureValue, "", "line break"},
		"nul byte":              {"one\x00two", "", "line break"},
		"empty":                 {"", "", "empty value"},
		"newline only":          {"\n", "", "empty value"},
		"invalid utf8":          {"one\xffbad", "", "UTF-8"},
		"over the size ceiling": {strings.Repeat("x", MaxValueBytes+1), "", "more than"},
	} {
		t.Run(name, func(t *testing.T) {
			secret, err := Validate([]byte(test.raw))
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Validate error = %v, want one containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if secret.Reveal() != test.want {
				t.Fatalf("Validate value = %q want %q", secret.Reveal(), test.want)
			}
		})
	}
}

// A Secret that ends up rendered anywhere must render as the placeholder, not
// as the value and not as its bytes. Each case asserts the placeholder is what
// comes out: asserting only "the value is absent" would pass even with every
// redaction removed, because the unexported field renders as bytes either way.
func TestSecretRendersAsThePlaceholderOnEveryPath(t *testing.T) {
	secret := New(fixtureValue)
	for name, got := range map[string]string{
		"%v":       fmt.Sprintf("%v", secret),
		"%s":       fmt.Sprintf("value is %s", secret),
		"%q":       fmt.Sprintf("%q", secret),
		"%#v":      fmt.Sprintf("%#v", secret),
		"String":   secret.String(),
		"in error": fmt.Errorf("could not type %v", secret).Error(),
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(got, Placeholder) {
				t.Fatalf("rendered %q, want it to contain %q", got, Placeholder)
			}
			if strings.Contains(got, fixtureValue) {
				t.Fatalf("rendered %q, which carries the value", got)
			}
		})
	}

	encoded, err := json.Marshal(struct {
		Field Secret `json:"field"`
	}{Field: secret})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), Placeholder) || strings.Contains(string(encoded), fixtureValue) {
		t.Fatalf("marshalled %s, want the placeholder and not the value", encoded)
	}
}

func TestWipeClearsTheValueForEveryCopyOfTheSecret(t *testing.T) {
	secret := New(fixtureValue)
	copied := secret
	if secret.Reveal() != fixtureValue || copied.Reveal() != fixtureValue {
		t.Fatal("the secret did not carry the value before the wipe")
	}
	secret.Wipe()
	if !secret.Empty() || secret.Reveal() != "" {
		t.Fatalf("wiped secret still reveals %q", secret.Reveal())
	}
	// The copy shares the backing array on purpose: a deferred wipe in the
	// runner has to clear the value every holder can still read.
	if copied.Reveal() != strings.Repeat("\x00", len(fixtureValue)) {
		t.Fatalf("a copy of the wiped secret still reveals %q", copied.Reveal())
	}
}

func TestScrubRemovesTheValueAndKeepsNoWrappedCopy(t *testing.T) {
	secret := New(fixtureValue)
	chatty := fmt.Errorf("could not set field to %q", fixtureValue)

	scrubbed := Scrub(chatty, secret)
	if strings.Contains(scrubbed.Error(), fixtureValue) {
		t.Fatalf("scrubbed error still reads %q", scrubbed.Error())
	}
	if !strings.Contains(scrubbed.Error(), Placeholder) {
		t.Fatalf("scrubbed error %q does not name the redaction", scrubbed.Error())
	}
	// Not wrapped: the wrapped error's own Error() still holds the value, so a
	// chain would keep a live copy for anything walking Unwrap to find.
	if unwrapped := errors.Unwrap(scrubbed); unwrapped != nil {
		t.Fatalf("scrubbed error unwraps to %v, which still carries the value", unwrapped)
	}

	// An error that never mentioned the value keeps its chain, so errors.Is
	// still works for every ordinary failure.
	sentinel := errors.New("bridge is not connected")
	wrapped := fmt.Errorf("fill: %w", sentinel)
	if got := Scrub(wrapped, secret); !errors.Is(got, sentinel) {
		t.Fatalf("Scrub broke the chain of an error that never held the value: %v", got)
	}
	if got := Scrub(nil, secret); got != nil {
		t.Fatalf("Scrub(nil) = %v", got)
	}
}

// fixtureAwkwardValue is low-entropy and obviously fabricated, and it carries
// the characters that change under escaping. A fixture made only of letters and
// dashes renders identically raw, quoted, JSON-encoded and percent-encoded, so
// it cannot tell a scrubber that handles one form from a scrubber that handles
// all of them.
const fixtureAwkwardValue = `fixture-"quote\back one`

// A transport does not have to write the value out raw. The snapshot scripts
// marshal the text they are told to type into a JSON expression, and a %q-
// formatted Go error quotes it — both of which leave a value containing a quote
// or a backslash looking nothing like itself.
func TestScrubRemovesTheValueInEveryFormATransportWritesIt(t *testing.T) {
	secret := New(fixtureAwkwardValue)
	for name, test := range map[string]struct {
		message string
		form    string
	}{
		"raw": {
			message: "could not set field to " + fixtureAwkwardValue,
			form:    fixtureAwkwardValue,
		},
		"go quoted": {
			message: fmt.Sprintf("could not set field to %q", fixtureAwkwardValue),
			form:    strconv.Quote(fixtureAwkwardValue)[1 : len(strconv.Quote(fixtureAwkwardValue))-1],
		},
		"json encoded": {
			message: `evaluate failed: {"text":` + mustJSON(t, fixtureAwkwardValue) + `}`,
			form:    mustJSON(t, fixtureAwkwardValue)[1 : len(mustJSON(t, fixtureAwkwardValue))-1],
		},
		"query escaped": {
			message: "POST /fill?text=" + url.QueryEscape(fixtureAwkwardValue) + " failed",
			form:    url.QueryEscape(fixtureAwkwardValue),
		},
		"path escaped": {
			message: "GET /fill/" + url.PathEscape(fixtureAwkwardValue) + " failed",
			form:    url.PathEscape(fixtureAwkwardValue),
		},
	} {
		t.Run(name, func(t *testing.T) {
			// Without this the case proves nothing: a form that is not in the
			// message cannot be scrubbed out of it.
			if !strings.Contains(test.message, test.form) {
				t.Fatalf("the %s message %q does not contain the form under test", name, test.message)
			}
			got := ScrubString(test.message, secret)
			if strings.Contains(got, test.form) {
				t.Fatalf("scrubbed message still carries the %s form: %q", name, got)
			}
			if strings.Contains(got, fixtureAwkwardValue) {
				t.Fatalf("scrubbed message still carries the raw value: %q", got)
			}
			if !strings.Contains(got, Placeholder) {
				t.Fatalf("scrubbed message %q does not name the redaction", got)
			}
		})
	}
	// Scrub goes through ScrubString, so the error path gets the same net.
	chatty := fmt.Errorf("could not set field to %q", fixtureAwkwardValue)
	if got := Scrub(chatty, secret); strings.Contains(got.Error(), `fixture-\"quote`) {
		t.Fatalf("Scrub left the quoted value in %q", got)
	}
}

func mustJSON(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestProbeTreatsAMissingResolverAsNoProvider(t *testing.T) {
	if err := Probe(nil); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("Probe(nil) = %v, want ErrNoProvider", err)
	}
}
