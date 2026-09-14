package browser

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
)

func TestFindActValidateRejectsUnrunnableRequests(t *testing.T) {
	tests := []struct {
		name    string
		request FindAct
		wantErr string
	}{
		{name: "click by role", request: FindAct{Role: "button", Action: "click"}},
		{name: "click by query", request: FindAct{Query: "Add to cart", Action: "click"}},
		{name: "fill with value", request: FindAct{Query: "Email", Action: "fill", Value: "fixture-user"}},
		{name: "hover by query", request: FindAct{Query: "Menu", Action: "hover"}},
		{name: "no action", request: FindAct{Query: "Add"}, wantErr: "find action is required"},
		{name: "unknown action", request: FindAct{Query: "Add", Action: "submit"}, wantErr: "unsupported find action"},
		{name: "no target", request: FindAct{Action: "click"}, wantErr: "requires query, text or role"},
		{name: "fill without value", request: FindAct{Query: "Email", Action: "fill"}, wantErr: "requires value"},
		{name: "type without value", request: FindAct{Query: "Email", Action: "type"}, wantErr: "requires value"},
		{name: "select without value", request: FindAct{Query: "Size", Action: "select"}, wantErr: "requires value"},
		// A value on a click is not silently dropped: the caller meant something
		// by it, and a flow that believes it filled a field is worse than one
		// that is told it asked for something incoherent.
		{name: "click with value", request: FindAct{Query: "Add", Action: "click", Value: "x"}, wantErr: "takes no value"},
		{name: "exact with only a role", request: FindAct{Role: "button", Action: "click", Exact: true}, wantErr: "requires query or text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.request.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// The limit a locate-and-act searches with is fixed by the request, not taken
// from the caller. A caller-supplied limit of 1 would hand the exactly-one rule
// a set of one that the limit created.
func TestFindActSearchesWithItsOwnLimit(t *testing.T) {
	opts := FindAct{Query: "Add", Role: "button", TextContent: true, ViewportOnly: true, Action: "click"}.FindOptions()
	if opts.Limit != FindActResolveLimit {
		t.Fatalf("limit = %d, want %d", opts.Limit, FindActResolveLimit)
	}
	if opts.Query != "Add" || opts.Role != "button" || !opts.TextContent || !opts.ViewportOnly {
		t.Fatalf("search options lost a filter: %+v", opts)
	}
}

func element(ref, role, name string) snapshot.Element {
	return snapshot.Element{Ref: ref, Role: role, Name: name}
}

func TestResolveFindActTargetRefusesToGuess(t *testing.T) {
	three := snapshot.FindResult{Elements: []snapshot.Element{
		element("e4", "button", "Add to cart"),
		element("e9", "button", "Add to wishlist"),
		element("e12", "link", "Add a review"),
	}}

	tests := []struct {
		name     string
		result   snapshot.FindResult
		request  FindAct
		wantRef  string
		wantErrs []string
	}{
		{
			name:    "one match",
			result:  snapshot.FindResult{Elements: []snapshot.Element{element("e4", "button", "Add to cart")}},
			request: FindAct{Query: "Add to cart", Action: "click"},
			wantRef: "e4",
		},
		{
			name:     "several matches name the rivals",
			result:   three,
			request:  FindAct{Query: "Add", Action: "click"},
			wantErrs: []string{"matches 3 elements", "refusing to guess", `e4 button "Add to cart"`, `e9 button "Add to wishlist"`},
		},
		{
			name:     "no match",
			result:   snapshot.FindResult{},
			request:  FindAct{Query: "Checkout", Action: "click"},
			wantErrs: []string{"no element matches", `"Checkout"`},
		},
		{
			// exact is the escape hatch from ambiguity: it compares the accessible
			// name for equality after collapsing case and whitespace.
			name:    "exact narrows to one",
			result:  three,
			request: FindAct{Query: "  add TO cart ", Action: "click", Exact: true},
			wantRef: "e4",
		},
		{
			name:     "exact that matches nothing is not a silent fallback",
			result:   three,
			request:  FindAct{Query: "Add", Action: "click", Exact: true},
			wantErrs: []string{"no element matches"},
		},
		{
			name: "exact also compares the field value",
			result: snapshot.FindResult{Elements: []snapshot.Element{
				{Ref: "e2", Role: "textbox", Name: "Search", Value: "shoes"},
				{Ref: "e3", Role: "textbox", Name: "Filter", Value: "socks"},
			}},
			request: FindAct{Query: "shoes", Action: "fill", Value: "boots", Exact: true},
			wantRef: "e2",
		},
		{
			// A truncated search cannot be used to conclude uniqueness: the rival
			// that would have made it ambiguous may be the one that was cut.
			name: "truncated search refuses even with one element",
			result: snapshot.FindResult{
				Elements: []snapshot.Element{element("e4", "button", "Add to cart")},
				Metadata: map[string]any{"truncated": true},
			},
			request:  FindAct{Query: "Add", Action: "click"},
			wantErrs: []string{"truncated"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveFindActTarget(tt.result, tt.request)
			if len(tt.wantErrs) == 0 {
				if err != nil {
					t.Fatalf("ResolveFindActTarget() = %v, want ref %s", err, tt.wantRef)
				}
				if got.Ref != tt.wantRef {
					t.Fatalf("ref = %q, want %q", got.Ref, tt.wantRef)
				}
				return
			}
			if err == nil {
				t.Fatalf("ResolveFindActTarget() resolved to %q, want an error", got.Ref)
			}
			for _, want := range tt.wantErrs {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// A page with more rivals than the error lists must still say how many there
// were, or a caller shown eight of forty thinks it is nearly unambiguous.
func TestResolveFindActTargetCountsTheRivalsItDoesNotList(t *testing.T) {
	elements := make([]snapshot.Element, 0, 12)
	for i := 0; i < 12; i++ {
		elements = append(elements, element("e"+string(rune('a'+i)), "button", "Add"))
	}
	_, err := ResolveFindActTarget(snapshot.FindResult{Elements: elements}, FindAct{Query: "Add", Action: "click"})
	if err == nil {
		t.Fatal("twelve matches resolved without an error")
	}
	for _, want := range []string{"matches 12 elements", "and 4 more"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

type stubFinder struct {
	result snapshot.FindResult
	err    error
	opts   snapshot.FindOptions
}

func (s *stubFinder) Find(_ context.Context, opts snapshot.FindOptions) (snapshot.FindResult, error) {
	s.opts = opts
	return s.result, s.err
}

func TestRunFindActStepActuatesOnlyTheResolvedRef(t *testing.T) {
	tests := []struct {
		name      string
		elements  []snapshot.Element
		request   FindAct
		wantVerb  string
		wantRef   string
		wantValue string
		wantErr   string
	}{
		{
			name:     "click",
			elements: []snapshot.Element{element("e4", "button", "Add to cart")},
			request:  FindAct{Query: "Add to cart", Action: "click"},
			wantVerb: "click",
			wantRef:  "e4",
		},
		{
			name:      "fill carries the value",
			elements:  []snapshot.Element{element("e7", "textbox", "Email")},
			request:   FindAct{Query: "Email", Action: "fill", Value: "fixture-user"},
			wantVerb:  "fill",
			wantRef:   "e7",
			wantValue: "fixture-user",
		},
		{
			name:      "select carries the value",
			elements:  []snapshot.Element{element("e8", "combobox", "Size")},
			request:   FindAct{Query: "Size", Action: "select", Value: "large"},
			wantVerb:  "select",
			wantRef:   "e8",
			wantValue: "large",
		},
		{
			name:     "hover",
			elements: []snapshot.Element{element("e9", "button", "Menu")},
			request:  FindAct{Query: "Menu", Action: "hover"},
			wantVerb: "hover",
			wantRef:  "e9",
		},
		{
			// The point of the whole exercise: ambiguity must stop the ACTION, not
			// just annotate it.
			name: "ambiguity actuates nothing",
			elements: []snapshot.Element{
				element("e4", "button", "Add to cart"),
				element("e9", "button", "Add to wishlist"),
			},
			request: FindAct{Query: "Add", Action: "click"},
			wantErr: "refusing to guess",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotVerb, gotRef, gotValue string
			record := func(verb string) func(context.Context, string) error {
				return func(_ context.Context, ref string) error {
					gotVerb, gotRef = verb, ref
					return nil
				}
			}
			recordValue := func(verb string) func(context.Context, string, string) error {
				return func(_ context.Context, ref, value string) error {
					gotVerb, gotRef, gotValue = verb, ref, value
					return nil
				}
			}
			actuator := FindActuator{
				Click:  record("click"),
				Hover:  record("hover"),
				Fill:   recordValue("fill"),
				Type:   recordValue("type"),
				Select: recordValue("select"),
			}
			finder := &stubFinder{result: snapshot.FindResult{Elements: tt.elements}}
			ref, err := RunFindActStep(context.Background(), finder, actuator, tt.request)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("RunFindActStep() = (%q, %v), want an error containing %q", ref, err, tt.wantErr)
				}
				if gotVerb != "" {
					t.Fatalf("an ambiguous search still actuated %s on %s", gotVerb, gotRef)
				}
				return
			}
			if err != nil {
				t.Fatalf("RunFindActStep() = %v", err)
			}
			if ref != tt.wantRef || gotRef != tt.wantRef || gotVerb != tt.wantVerb || gotValue != tt.wantValue {
				t.Fatalf("actuated %s(%q, %q) returning %q; want %s(%q, %q)",
					gotVerb, gotRef, gotValue, ref, tt.wantVerb, tt.wantRef, tt.wantValue)
			}
			if finder.opts.Limit != FindActResolveLimit {
				t.Fatalf("searched with limit %d, want %d", finder.opts.Limit, FindActResolveLimit)
			}
		})
	}
}

func TestRunFindActStepSurfacesTheSearchError(t *testing.T) {
	want := errors.New("fixture search failure")
	_, err := RunFindActStep(context.Background(), &stubFinder{err: want}, FindActuator{
		Click: func(context.Context, string) error {
			t.Fatal("a failed search must not actuate anything")
			return nil
		},
	}, FindAct{Query: "Add", Action: "click"})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// An action with no primitive wired on a transport must say so by name rather
// than report a success it did not perform.
func TestFindActuatorNamesAMissingPrimitive(t *testing.T) {
	err := FindActuator{}.Actuate(context.Background(), "click", "e1", "")
	if err == nil || !strings.Contains(err.Error(), "not wired on this transport") {
		t.Fatalf("err = %v, want a named capability error", err)
	}
	if err := (FindActuator{}).Actuate(context.Background(), "submit", "e1", ""); err == nil ||
		!strings.Contains(err.Error(), "unsupported find action") {
		t.Fatalf("err = %v, want an unsupported-action error", err)
	}
}
