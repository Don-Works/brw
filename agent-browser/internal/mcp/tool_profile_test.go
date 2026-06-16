package mcp

import "testing"

func TestToolProfileFiltersAdvertisedTools(t *testing.T) {
	full := (&Server{toolProfile: "all"}).advertisedTools()
	core := (&Server{toolProfile: "core"}).advertisedTools()

	if len(core) >= len(full) {
		t.Fatalf("core profile (%d) should advertise fewer tools than all (%d)", len(core), len(full))
	}
	if len(core) != len(coreToolNames) {
		t.Fatalf("core profile advertised %d tools, want %d (every coreToolNames entry must be a real tool)", len(core), len(coreToolNames))
	}

	// Every advertised core tool must be in the core set; a few essentials must
	// always be present.
	got := map[string]bool{}
	for _, tl := range core {
		name, _ := tl["name"].(string)
		if !coreToolNames[name] {
			t.Errorf("core profile advertised non-core tool %q", name)
		}
		got[name] = true
	}
	for _, essential := range []string{"browser_open", "browser_snapshot", "browser_click", "browser_type", "browser_press", "browser_batch"} {
		if !got[essential] {
			t.Errorf("core profile missing essential tool %q", essential)
		}
	}

	// An empty/unknown profile must behave as "all".
	if def := (&Server{toolProfile: ""}).advertisedTools(); len(def) != len(full) {
		t.Fatalf("empty profile advertised %d tools, want full %d", len(def), len(full))
	}
}
