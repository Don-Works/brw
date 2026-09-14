package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
)

func findFixtureElement(ref, role, name string) snapshot.Element {
	return snapshot.Element{Ref: ref, Role: role, Name: name, Visible: true, InViewport: true, Source: []string{"dom"}}
}

// brw_find with an action locates and acts in one call. Without an action it is
// still the read-only search it has always been.
func TestFindWithAnActionLocatesAndActs(t *testing.T) {
	tests := []struct {
		name      string
		elements  []snapshot.Element
		args      string
		wantActed []string
		wantErr   string
	}{
		{
			name:      "no action stays a read-only find",
			elements:  []snapshot.Element{findFixtureElement("e4", "button", "Add to cart")},
			args:      `{"query":"Add"}`,
			wantActed: []string{"find:Add/"},
		},
		{
			name:      "click the single match",
			elements:  []snapshot.Element{findFixtureElement("e4", "button", "Add to cart")},
			args:      `{"query":"Add to cart","role":"button","action":"click"}`,
			wantActed: []string{"find:Add to cart/button", "click:e4"},
		},
		{
			name:      "fill carries the value",
			elements:  []snapshot.Element{findFixtureElement("e7", "textbox", "Email")},
			args:      `{"query":"Email","action":"fill","value":"fixture-user"}`,
			wantActed: []string{"find:Email/", "fill:e7=fixture-user"},
		},
		{
			// The safety property: several matches must stop the action, list the
			// rivals, and actuate nothing.
			name: "several matches refuse and actuate nothing",
			elements: []snapshot.Element{
				findFixtureElement("e4", "button", "Add to cart"),
				findFixtureElement("e9", "button", "Add to wishlist"),
			},
			args:      `{"query":"Add","action":"click"}`,
			wantActed: []string{"find:Add/"},
			wantErr:   "refusing to guess",
		},
		{
			name: "no match is an error, not an empty success",
			// Empty rather than nil: nil means "this test does not care which
			// elements the page has" and gets the default fixture list.
			elements:  []snapshot.Element{},
			args:      `{"query":"Checkout","action":"click"}`,
			wantActed: []string{"find:Checkout/"},
			wantErr:   "no element matches",
		},
		{
			// A caller cannot narrow its way past the rule: the locate-and-act
			// search is built with its own limit, so limit:1 never turns two
			// rivals into a unique match.
			name: "a caller-supplied limit cannot manufacture uniqueness",
			elements: []snapshot.Element{
				findFixtureElement("e4", "button", "Add to cart"),
				findFixtureElement("e9", "button", "Add to wishlist"),
			},
			args:      `{"query":"Add","action":"click","limit":1}`,
			wantActed: []string{"find:Add/"},
			wantErr:   "refusing to guess",
		},
		{
			name:     "exact resolves the ambiguity",
			elements: []snapshot.Element{findFixtureElement("e4", "button", "Add to cart"), findFixtureElement("e9", "button", "Add to wishlist")},
			args:     `{"query":"Add to cart","action":"click","exact":true}`,
			// The search itself is unchanged; exact narrows what came back.
			wantActed: []string{"find:Add to cart/", "click:e4"},
		},
		{
			name:      "an unsupported verb is named",
			elements:  []snapshot.Element{findFixtureElement("e4", "button", "Add to cart")},
			args:      `{"query":"Add","action":"submit"}`,
			wantActed: nil,
			wantErr:   "unsupported find action",
		},
		{
			name:      "fill without a value is refused before the search",
			elements:  []snapshot.Element{findFixtureElement("e7", "textbox", "Email")},
			args:      `{"query":"Email","action":"fill"}`,
			wantActed: nil,
			wantErr:   "requires value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := &observeController{findElements: tt.elements}
			raw, rpcErr := New(controller).callTool(context.Background(), "brw_find", json.RawMessage(tt.args))

			if tt.wantErr != "" {
				message := ""
				if rpcErr != nil {
					message = rpcErr.Message
				} else if result, ok := raw.(map[string]any); ok {
					message = toolText(t, result)
				}
				if !strings.Contains(message, tt.wantErr) {
					t.Fatalf("response %q does not contain %q", message, tt.wantErr)
				}
			} else if rpcErr != nil {
				t.Fatalf("brw_find %s: %v", tt.args, rpcErr)
			}

			if strings.Join(controller.acted, "|") != strings.Join(tt.wantActed, "|") {
				t.Fatalf("controller calls = %v, want %v", controller.acted, tt.wantActed)
			}
		})
	}
}

// The locate-and-act response has to name the element it picked: the caller
// never saw the search result, so without it a follow-up would have to search
// again — which is the round trip this feature exists to remove.
func TestFindWithAnActionReportsTheMatchedElement(t *testing.T) {
	controller := &observeController{findElements: []snapshot.Element{findFixtureElement("e4", "button", "Add to cart")}}
	response := toolText(t, observeCallTool(t, controller, "brw_find", `{"query":"Add to cart","action":"click"}`))

	var decoded struct {
		Matched snapshot.Element `json:"matched"`
		Action  string           `json:"action"`
		Result  struct {
			OK       bool               `json:"ok"`
			Elements []snapshot.Element `json:"elements"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(response), &decoded); err != nil {
		t.Fatalf("decode %s: %v", response, err)
	}
	if decoded.Matched.Ref != "e4" || decoded.Matched.Name != "Add to cart" || decoded.Action != "click" {
		t.Fatalf("matched element = %+v action = %q", decoded.Matched, decoded.Action)
	}
	if !decoded.Result.OK || len(decoded.Result.Elements) == 0 {
		t.Fatalf("locate-and-act did not carry the post-action observation: %s", response)
	}

	// observe reaches the observation half without touching the match.
	trimmed := toolText(t, observeCallTool(t, controller, "brw_find", `{"query":"Add to cart","action":"click","observe":"none"}`))
	if !strings.Contains(trimmed, `"ref":"e4"`) {
		t.Fatalf("observe=none dropped the matched element: %s", trimmed)
	}
	if strings.Contains(trimmed, `"elements"`) {
		t.Fatalf("observe=none kept the observation element list: %s", trimmed)
	}
}
