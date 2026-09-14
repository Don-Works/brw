package browser

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/Don-Works/brw/internal/snapshot"
)

// FindActResolveLimit bounds the search a locate-and-act performs. It is the
// limit the request is BUILT with rather than one a caller may pass: a caller
// supplied limit of 1 would truncate every rival match away and turn the
// exactly-one rule into "act on the highest-ranked match", which is the failure
// the rule exists to prevent.
const FindActResolveLimit = 200

// ErrFindActTruncated is returned when the page holds more candidates than the
// search window. A truncated search answers a different question than the one
// that was asked, so it cannot be used to conclude that a match is unique.
var ErrFindActTruncated = errors.New("locate-and-act search was truncated; narrow it with role or a longer query before acting")

// findActActions are the verbs a locate-and-act step can run. Every one has a
// primitive on BOTH transports; the set is deliberately closed so a typo lands
// as a named error rather than as a silent no-op.
var findActActions = []string{"click", "fill", "type", "select", "hover"}

// findActNeedsValue names the verbs that write something.
func findActNeedsValue(action string) bool {
	return action == "fill" || action == "type" || action == "select"
}

// FindAct is one semantic search that must resolve to exactly one element,
// followed by one action on it. It is brw's answer to "locate and act in one
// call" without giving up what refs are for: the caller never learns a ref that
// could have been one of several elements, because several elements is an error.
type FindAct struct {
	Query         string `json:"query,omitempty"`
	Text          string `json:"text,omitempty"`
	Role          string `json:"role,omitempty"`
	Action        string `json:"action"`
	Value         string `json:"value,omitempty"`
	Exact         bool   `json:"exact,omitempty"`
	ViewportOnly  bool   `json:"viewport_only,omitempty"`
	IncludeHidden bool   `json:"include_hidden,omitempty"`
	TextContent   bool   `json:"text_content,omitempty"`
}

// Validate rejects a request that cannot be executed as written, by name.
func (f FindAct) Validate() error {
	action := strings.TrimSpace(f.Action)
	if action == "" {
		return errors.New("find action is required")
	}
	if !slices.Contains(findActActions, action) {
		return fmt.Errorf("unsupported find action %q; supported: %s", action, strings.Join(findActActions, ", "))
	}
	if f.searchTerm() == "" && strings.TrimSpace(f.Role) == "" {
		return errors.New("find with an action requires query, text or role to locate the element")
	}
	if findActNeedsValue(action) && f.Value == "" {
		return fmt.Errorf("find action %q requires value", action)
	}
	// Refusing rather than ignoring: a caller that passed a value to a click
	// meant something by it, and silently dropping it is how a flow ends up
	// believing it filled a field.
	if !findActNeedsValue(action) && f.Value != "" {
		return fmt.Errorf("find action %q takes no value", action)
	}
	if f.Exact && f.searchTerm() == "" {
		return errors.New("find exact matching requires query or text")
	}
	return nil
}

func (f FindAct) searchTerm() string {
	if term := strings.TrimSpace(f.Query); term != "" {
		return term
	}
	return strings.TrimSpace(f.Text)
}

// FindOptions builds the search this request runs. The limit is fixed here on
// purpose; see FindActResolveLimit.
func (f FindAct) FindOptions() snapshot.FindOptions {
	return snapshot.FindOptions{
		Query:         f.Query,
		Text:          f.Text,
		Role:          f.Role,
		Limit:         FindActResolveLimit,
		ViewportOnly:  f.ViewportOnly,
		IncludeHidden: f.IncludeHidden,
		TextContent:   f.TextContent,
	}
}

// normalizeFindText collapses whitespace and case the way an accessible name is
// compared, so "  Add  to cart " and "add to cart" are the same name.
func normalizeFindText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// ResolveFindActTarget returns the single element the request names, or an error
// that says why it could not. Zero matches and several matches are BOTH errors:
// acting on the highest-ranked of several is exactly the guess semantic refs
// exist to avoid, and the model cannot tell afterwards that it guessed.
func ResolveFindActTarget(result snapshot.FindResult, f FindAct) (snapshot.Element, error) {
	if truncated, _ := result.Metadata["truncated"].(bool); truncated {
		return snapshot.Element{}, ErrFindActTruncated
	}
	candidates := result.Elements
	if f.Exact {
		want := normalizeFindText(f.searchTerm())
		kept := make([]snapshot.Element, 0, len(candidates))
		for _, el := range candidates {
			if normalizeFindText(el.Name) == want || normalizeFindText(el.Value) == want {
				kept = append(kept, el)
			}
		}
		candidates = kept
	}
	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		return snapshot.Element{}, fmt.Errorf("no element matches %s", f.describeTarget())
	default:
		return snapshot.Element{}, fmt.Errorf("%s matches %d elements, refusing to guess which one to %s: %s — "+
			"narrow it with role or exact, or act on one ref with brw_click",
			f.describeTarget(), len(candidates), strings.TrimSpace(f.Action), describeCandidates(candidates))
	}
}

func (f FindAct) describeTarget() string {
	parts := make([]string, 0, 3)
	if term := f.searchTerm(); term != "" {
		parts = append(parts, fmt.Sprintf("%q", term))
	}
	if role := strings.TrimSpace(f.Role); role != "" {
		parts = append(parts, "role "+role)
	}
	if f.Exact {
		parts = append(parts, "exact")
	}
	if len(parts) == 0 {
		return "the requested element"
	}
	return strings.Join(parts, " ")
}

// describeCandidates lists enough of the rivals that the caller can pick one
// without a second round trip, and says how many it did not list.
func describeCandidates(elements []snapshot.Element) string {
	const listed = 8
	shown := elements
	if len(shown) > listed {
		shown = shown[:listed]
	}
	parts := make([]string, 0, len(shown)+1)
	for _, el := range shown {
		parts = append(parts, fmt.Sprintf("%s %s %q", el.Ref, el.Role, el.Name))
	}
	if len(elements) > listed {
		parts = append(parts, fmt.Sprintf("and %d more", len(elements)-listed))
	}
	return strings.Join(parts, ", ")
}

// FindActuator carries one transport's RAW (non-observing) element primitives so
// a locate-and-act batch or plan step keeps that runner's one-observation
// contract instead of paying for a post-action snapshot per step.
type FindActuator struct {
	Click  func(ctx context.Context, ref string) error
	Fill   func(ctx context.Context, ref, value string) error
	Type   func(ctx context.Context, ref, value string) error
	Select func(ctx context.Context, ref, value string) error
	Hover  func(ctx context.Context, ref string) error
}

// Actuate runs one validated action against a resolved ref.
func (a FindActuator) Actuate(ctx context.Context, action, ref, value string) error {
	var fn func() error
	switch action {
	case "click":
		if a.Click != nil {
			fn = func() error { return a.Click(ctx, ref) }
		}
	case "fill":
		if a.Fill != nil {
			fn = func() error { return a.Fill(ctx, ref, value) }
		}
	case "type":
		if a.Type != nil {
			fn = func() error { return a.Type(ctx, ref, value) }
		}
	case "select":
		if a.Select != nil {
			fn = func() error { return a.Select(ctx, ref, value) }
		}
	case "hover":
		if a.Hover != nil {
			fn = func() error { return a.Hover(ctx, ref) }
		}
	default:
		return fmt.Errorf("unsupported find action %q; supported: %s", action, strings.Join(findActActions, ", "))
	}
	if fn == nil {
		return fmt.Errorf("find action %q is not wired on this transport", action)
	}
	return fn()
}

// FindActFinder is the read half of a locate-and-act: only the search.
type FindActFinder interface {
	Find(context.Context, snapshot.FindOptions) (snapshot.FindResult, error)
}

// LiveFinder is a searcher that can bypass its transport's snapshot cache. A
// transport that may serve a cached element list implements it, and a
// locate-and-act always resolves through it: the extension's cache-validity
// probe only sees DOM MUTATIONS, and a fill, select or checkbox writes a DOM
// property that mutates no node, so a cached read after such a step returns the
// pre-action page. Deciding to actuate from that page means the exactly-one
// rule confirms a uniqueness the page may no longer have.
type LiveFinder interface {
	FindLive(context.Context, snapshot.FindOptions) (snapshot.FindResult, error)
}

// findActSearcher picks the searcher a locate-and-act resolves with. Choosing
// here rather than at each call site is what makes the rule transport-wide: the
// standalone tool, a batch step and a plan step all go through ResolveFindAct.
//
// A finder that does not implement LiveFinder is REFUSED rather than used as it
// is. Falling back to the plain Find looks harmless on a transport that has no
// cache and is silent on one that does: that fallback is how the upstream-HTTP
// proxy, whose Find is a GET the daemon served from the extension's snapshot
// cache, went on resolving a standalone locate-and-act from a stale element list
// after the bridge itself was fixed. Every transport now says whether its search
// is live (browser.Controller requires FindLive), so a new one that forgets
// fails loudly instead of quietly acting on the page as it used to be.
func findActSearcher(finder FindActFinder) (FindActFinder, error) {
	if live, ok := finder.(LiveFinder); ok {
		return findInTab(live.FindLive), nil
	}
	return nil, fmt.Errorf("%w: %T", ErrFinderNotLive, finder)
}

// ErrFinderNotLive is returned when a locate-and-act would have to resolve
// through a searcher that cannot promise a live read of the page.
var ErrFinderNotLive = errors.New("locate-and-act needs a live search of the page and this transport does not provide one; " +
	"acting on a cached element list would confirm a uniqueness the page may no longer have")

// ResolveFindAct validates the request, runs the search, and returns the one
// element it names.
func ResolveFindAct(ctx context.Context, finder FindActFinder, f FindAct) (snapshot.Element, error) {
	if err := f.Validate(); err != nil {
		return snapshot.Element{}, err
	}
	searcher, err := findActSearcher(finder)
	if err != nil {
		return snapshot.Element{}, err
	}
	result, err := searcher.Find(ctx, f.FindOptions())
	if err != nil {
		return snapshot.Element{}, err
	}
	return ResolveFindActTarget(result, f)
}

// RunFindActStep resolves the target and actuates it with a transport's raw
// primitives. It returns the ref it acted on so a runner can report it.
func RunFindActStep(ctx context.Context, finder FindActFinder, actuator FindActuator, f FindAct) (string, error) {
	element, err := ResolveFindAct(ctx, finder, f)
	if err != nil {
		return "", err
	}
	if err := actuator.Actuate(ctx, strings.TrimSpace(f.Action), element.Ref, f.Value); err != nil {
		return element.Ref, err
	}
	return element.Ref, nil
}

// FindActController is the observing half: the transport-agnostic Controller
// methods a standalone brw_find-with-action call uses, so it answers with the
// same post-action observation as the equivalent two calls would have.
type FindActController interface {
	FindActFinder
	Click(context.Context, string) (ActionResult, error)
	Fill(context.Context, snapshot.FillOptions) (ActionResult, error)
	Type(context.Context, string, string) (ActionResult, error)
	Select(context.Context, string, string) (ActionResult, error)
	Hover(context.Context, string) (ActionResult, error)
}

// FindActResult reports which element was chosen as well as what happened to it.
// The ref is included because the caller never saw the search result: without it
// a follow-up action would have to search again.
type FindActResult struct {
	Matched snapshot.Element `json:"matched"`
	Action  string           `json:"action"`
	Result  ActionResult     `json:"result"`
}

// RunFindAct is the standalone locate-and-act: resolve to exactly one element,
// then run the observing action tool against it.
func RunFindAct(ctx context.Context, c FindActController, f FindAct) (FindActResult, error) {
	element, err := ResolveFindAct(ctx, c, f)
	if err != nil {
		return FindActResult{}, err
	}
	action := strings.TrimSpace(f.Action)
	var result ActionResult
	switch action {
	case "click":
		result, err = c.Click(ctx, element.Ref)
	case "fill":
		result, err = c.Fill(ctx, snapshot.FillOptions{Ref: element.Ref, Text: f.Value, Replace: true})
	case "type":
		result, err = c.Type(ctx, element.Ref, f.Value)
	case "select":
		result, err = c.Select(ctx, element.Ref, f.Value)
	case "hover":
		result, err = c.Hover(ctx, element.Ref)
	default:
		return FindActResult{}, fmt.Errorf("unsupported find action %q; supported: %s", action, strings.Join(findActActions, ", "))
	}
	if err != nil {
		return FindActResult{}, err
	}
	return FindActResult{Matched: element, Action: action, Result: result}, nil
}

// FindActActions returns the supported verbs, sorted, for schemas and errors.
func FindActActions() []string {
	out := append([]string(nil), findActActions...)
	sort.Strings(out)
	return out
}
