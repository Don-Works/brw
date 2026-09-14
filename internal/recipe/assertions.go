package recipe

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Don-Works/brw/internal/browser"
)

// Assertion is the recipe spelling of a deterministic check. It is the same
// vocabulary brw_assert exposes, with one difference that is the whole point of
// a recipe: elements are named by a semantic Target, never by an observation
// ref, because a ref lives for one page state and a recipe outlives many.
//
// It is a step action rather than another wait_event kind because an assertion
// does not wait. wait_event polls to a timeout; an assertion reads once and says
// what it saw. Folding the two together would give every assertion an implicit
// sleep that no reviewer could see in the recipe.
type Assertion struct {
	Kind string `json:"kind"`
	Mode string `json:"mode,omitempty"`
	// Expected is the URL/prefix/regex for url, and the attribute value for
	// attribute.
	Expected        string  `json:"expected,omitempty"`
	IncludeFragment bool    `json:"include_fragment,omitempty"`
	Status          int     `json:"status,omitempty"`
	Target          *Target `json:"target,omitempty"`
	Count           *int    `json:"count,omitempty"`
	Min             *int    `json:"min,omitempty"`
	Max             *int    `json:"max,omitempty"`
	State           string  `json:"state,omitempty"`
	Negate          bool    `json:"negate,omitempty"`
	Attribute       string  `json:"attribute,omitempty"`
	// Filename selects the download. A recipe cannot name a download GUID: the
	// GUID is minted per download, so it can only be known after the run starts.
	Filename string `json:"filename,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	Bytes    *int64 `json:"bytes,omitempty"`
}

// Asserter is an optional Surface capability. It is optional so a Surface
// written against the previous ABI keeps compiling and reports the gap by name
// instead of failing to build.
type Asserter interface {
	Assert(context.Context, Assertion) error
}

var assertionKinds = []string{
	browser.AssertionURL,
	browser.AssertionHTTPStatus,
	browser.AssertionElementCount,
	browser.AssertionElementState,
	browser.AssertionAttribute,
	browser.AssertionDownload,
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

func validateAssertion(assertion Assertion, inputs map[string]Input) error {
	return validateAssertionValue(assertion, inputs, true)
}

// validateExpandedAssertion checks the same bounded shape after runtime
// interpolation without parsing the resulting user data as recipe syntax a
// second time. Expansion is deliberately single-pass, as it is for events.
func validateExpandedAssertion(assertion Assertion) error {
	return validateAssertionValue(assertion, nil, false)
}

func validateAssertionValue(assertion Assertion, inputs map[string]Input, checkTemplates bool) error {
	var problems []error
	if !slices.Contains(assertionKinds, assertion.Kind) {
		return fmt.Errorf("unsupported assertion kind %q", assertion.Kind)
	}
	targeted := slices.Contains([]string{browser.AssertionElementCount, browser.AssertionElementState, browser.AssertionAttribute}, assertion.Kind)
	if targeted {
		if assertion.Target == nil {
			problems = append(problems, errors.New("assertion requires a semantic target"))
		} else {
			if err := validateTarget(*assertion.Target); err != nil {
				problems = append(problems, err)
			} else if checkTemplates {
				if err := validateTargetTemplates(*assertion.Target, inputs); err != nil {
					problems = append(problems, err)
				}
			}
		}
	} else if assertion.Target != nil {
		problems = append(problems, errors.New("target is not valid for this assertion kind"))
	}

	counted := assertion.Kind == browser.AssertionElementCount
	if !counted && (assertion.Count != nil || assertion.Min != nil || assertion.Max != nil) {
		problems = append(problems, errors.New("count/min/max are only valid for element_count"))
	}
	if assertion.Kind != browser.AssertionElementState && (assertion.State != "" || assertion.Negate) {
		problems = append(problems, errors.New("state/negate are only valid for element_state"))
	}
	if assertion.Kind != browser.AssertionAttribute && assertion.Attribute != "" {
		problems = append(problems, errors.New("attribute is only valid for the attribute assertion"))
	}
	if assertion.Kind != browser.AssertionURL && assertion.IncludeFragment {
		problems = append(problems, errors.New("include_fragment is only valid for the url assertion"))
	}
	if assertion.Kind != browser.AssertionHTTPStatus && assertion.Status != 0 {
		problems = append(problems, errors.New("status is only valid for http_status"))
	}
	if assertion.Kind != browser.AssertionDownload && (assertion.Filename != "" || assertion.SHA256 != "" || assertion.Bytes != nil) {
		problems = append(problems, errors.New("filename/sha256/bytes are only valid for the download assertion"))
	}
	expectedKind := assertion.Kind == browser.AssertionURL || assertion.Kind == browser.AssertionAttribute
	if !expectedKind && assertion.Expected != "" {
		problems = append(problems, errors.New("expected is only valid for the url and attribute assertions"))
	}
	if !expectedKind && assertion.Mode != "" {
		problems = append(problems, errors.New("mode is only valid for the url and attribute assertions"))
	}

	switch assertion.Kind {
	case browser.AssertionURL:
		if strings.TrimSpace(assertion.Expected) == "" {
			problems = append(problems, errors.New("url assertion requires expected"))
		}
		if !slices.Contains([]string{"", browser.AssertModeExact, browser.AssertModePrefix, browser.AssertModeRegex}, assertion.Mode) {
			problems = append(problems, fmt.Errorf("url assertion mode must be exact, prefix or regex, got %q", assertion.Mode))
		}
	case browser.AssertionHTTPStatus:
		if assertion.Status < 100 || assertion.Status > 599 {
			problems = append(problems, errors.New("http_status assertion requires status between 100 and 599"))
		}
	case browser.AssertionElementCount:
		if assertion.Count == nil && assertion.Min == nil && assertion.Max == nil {
			problems = append(problems, errors.New("element_count assertion requires count, min or max"))
		}
	case browser.AssertionElementState:
		if !slices.Contains([]string{browser.AssertStateEnabled, browser.AssertStateEditable, browser.AssertStateChecked, browser.AssertStateFocused}, assertion.State) {
			problems = append(problems, fmt.Errorf("element_state assertion state must be enabled, editable, checked or focused, got %q", assertion.State))
		}
	case browser.AssertionAttribute:
		if strings.TrimSpace(assertion.Attribute) == "" {
			problems = append(problems, errors.New("attribute assertion requires attribute"))
		}
		if !slices.Contains([]string{"", browser.AssertModeExact, browser.AssertModeContains}, assertion.Mode) {
			problems = append(problems, fmt.Errorf("attribute assertion mode must be exact or contains, got %q", assertion.Mode))
		}
	case browser.AssertionDownload:
		if strings.TrimSpace(assertion.Filename) == "" {
			problems = append(problems, errors.New("download assertion requires filename"))
		}
		if strings.TrimSpace(assertion.SHA256) == "" && assertion.Bytes == nil {
			problems = append(problems, errors.New("download assertion requires sha256 or bytes"))
		}
		if assertion.SHA256 != "" && !inputTemplate.MatchString(assertion.SHA256) && !sha256Pattern.MatchString(assertion.SHA256) {
			problems = append(problems, errors.New("download assertion sha256 must be 64 hex characters"))
		}
	}

	for _, value := range []string{assertion.Expected, assertion.Attribute, assertion.Filename, assertion.SHA256} {
		if len(value) > 2000 {
			problems = append(problems, errors.New("assertion field is too long"))
		}
		if checkTemplates {
			if err := validateTemplates(value, inputs); err != nil {
				problems = append(problems, err)
			}
		}
	}
	// The whole request is revalidated against the browser ABI so recipe
	// authoring and the tool surface cannot drift apart silently. An assertion
	// still carrying templates is skipped here and checked again after
	// expansion: "${input:digest}" is not a sha256 until it is one.
	if len(problems) == 0 && !assertionHasTemplates(assertion) {
		if err := browser.ValidateAssertRequest(assertionRequest(assertion, "placeholder-ref")); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

func assertionHasTemplates(assertion Assertion) bool {
	values := assertionTemplateValues(nil, &assertion)
	for _, value := range values {
		if inputTemplate.MatchString(value) {
			return true
		}
	}
	return false
}

// assertionRequest renders the transport-level request. ref is supplied by the
// caller after semantic resolution; validation passes a placeholder because the
// shape, not the element, is what it is checking.
func assertionRequest(assertion Assertion, ref string) browser.AssertRequest {
	req := browser.AssertRequest{
		Assertion:       assertion.Kind,
		Mode:            assertion.Mode,
		Expected:        assertion.Expected,
		IncludeFragment: assertion.IncludeFragment,
		Status:          assertion.Status,
		Count:           assertion.Count,
		Min:             assertion.Min,
		Max:             assertion.Max,
		State:           assertion.State,
		Negate:          assertion.Negate,
		Attribute:       assertion.Attribute,
		Filename:        assertion.Filename,
		SHA256:          assertion.SHA256,
		Bytes:           assertion.Bytes,
	}
	switch assertion.Kind {
	case browser.AssertionElementState, browser.AssertionAttribute:
		req.Ref = ref
	case browser.AssertionElementCount:
		if assertion.Target != nil {
			req.Role = assertion.Target.Role
			req.Name = assertion.Target.Name
		}
	}
	return req
}

func expandAssertion(assertion Assertion, inputs map[string]string) (Assertion, error) {
	fields := []*string{&assertion.Expected, &assertion.Attribute, &assertion.Filename, &assertion.SHA256}
	for _, field := range fields {
		expanded, err := Expand(*field, inputs)
		if err != nil {
			return Assertion{}, err
		}
		*field = expanded
	}
	if assertion.Target != nil {
		target, err := expandTarget(*assertion.Target, inputs)
		if err != nil {
			return Assertion{}, err
		}
		assertion.Target = &target
	}
	if err := validateExpandedAssertion(assertion); err != nil {
		return Assertion{}, fmt.Errorf("expanded assertion is invalid: %w", err)
	}
	return assertion, nil
}

func assertionTemplateValues(values []string, assertion *Assertion) []string {
	if assertion == nil {
		return values
	}
	values = append(values, assertion.Expected, assertion.Attribute, assertion.Filename, assertion.SHA256)
	return appendTargetTemplateValues(values, assertion.Target)
}

// Assert resolves the assertion's semantic target, then evaluates the check on
// the browser host. Resolution happens here rather than in the browser package
// so a recipe never has to name an observation ref.
func (s *BrowserSurface) Assert(ctx context.Context, assertion Assertion) error {
	switch assertion.Kind {
	case browser.AssertionElementCount:
		matches, err := s.Resolve(ctx, *assertion.Target)
		if err != nil {
			return err
		}
		description := "role " + strconv.Quote(assertion.Target.Role)
		if name := targetNameForMessage(*assertion.Target); name != "" {
			description += " named " + strconv.Quote(name)
		}
		_, err = browser.AssertCount(len(matches), description, assertionRequest(assertion, ""))
		return err
	case browser.AssertionElementState, browser.AssertionAttribute:
		matches, err := s.Resolve(ctx, *assertion.Target)
		if err != nil {
			return err
		}
		if len(matches) != 1 {
			return fmt.Errorf("assertion target resolved to %d elements; refusing to guess", len(matches))
		}
		_, err = browser.Assert(ctx, s.Browser, assertionRequest(assertion, matches[0].Ref))
		return err
	case browser.AssertionDownload:
		// A recipe's download.completed postcondition already drained the entry
		// into this surface's per-tab cache, and Downloads() is delta-scoped for
		// recipes, so the ledger would no longer return it. Check the entry the
		// run actually observed; fall back to the ledger for a download that
		// completed without a postcondition.
		if entry, ok := s.peekCompletedDownload(ctx, assertion.Filename); ok {
			_, err := browser.AssertDownloadEntry(entry, assertionRequest(assertion, ""))
			return err
		}
	}
	_, err := browser.Assert(ctx, s.Browser, assertionRequest(assertion, ""))
	return err
}

func targetNameForMessage(target Target) string {
	if target.Name != "" {
		return target.Name
	}
	return target.NameContains
}
