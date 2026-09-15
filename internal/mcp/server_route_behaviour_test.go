package mcp

import (
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

// routeBehaviourEnum reads the behaviours brw_route advertises to an agent.
func routeBehaviourEnum(t *testing.T) []string {
	t.Helper()
	for _, entry := range tools() {
		if name, _ := entry["name"].(string); name != "brw_route" {
			continue
		}
		schema, ok := entry["inputSchema"].(map[string]any)
		if !ok {
			t.Fatal("brw_route has no input schema")
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatal("brw_route schema has no properties")
		}
		behaviour, ok := properties["behaviour"].(map[string]any)
		if !ok {
			t.Fatal("brw_route schema has no behaviour property")
		}
		values, ok := behaviour["enum"].([]string)
		if !ok {
			t.Fatalf("brw_route behaviour enum is %T, not a string list", behaviour["enum"])
		}
		return values
	}
	t.Fatal("brw_route is not a registered tool")
	return nil
}

// The advertised enum and the refusal table have to be disjoint.
//
// Either half alone is survivable; the combination is not. A behaviour offered
// in the enum and refused by the backends is a tool that documents a capability
// every call for it rejects, and one refused by the backends while the
// description tells an agent to use it is the same failure read from the other
// end. Enumerating the table rather than checking for "redirect" by hand is what
// makes this hold for the next entry as well.
func TestAdvertisedRouteBehavioursExcludeTheRefusedOnes(t *testing.T) {
	advertised := map[string]bool{}
	for _, value := range routeBehaviourEnum(t) {
		advertised[value] = true
	}
	if len(advertised) == 0 {
		t.Fatal("brw_route advertises no behaviours at all")
	}
	for behaviour := range browser.UnsupportedRouteBehaviours {
		if advertised[string(behaviour)] {
			t.Errorf("brw_route advertises behaviour %q, which both transports refuse by name", behaviour)
		}
	}
}
