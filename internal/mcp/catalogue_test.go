package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func toolByName(t *testing.T, name string) map[string]any {
	t.Helper()
	for _, tl := range tools() {
		if got, _ := tl["name"].(string); got == name {
			return tl
		}
	}
	t.Fatalf("tool %q is not in the catalogue", name)
	return nil
}

func toolProperties(t *testing.T, name string) map[string]any {
	t.Helper()
	schema, _ := toolByName(t, name)["inputSchema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		t.Fatalf("tool %q has no schema properties", name)
	}
	return props
}

// A parameter the handler reads but the schema never mentions is a parameter no
// agent will ever pass. This locks every filtering and paging option added for
// token efficiency to its advertised schema.
func TestAdvertisedSchemasCoverHandlerParameters(t *testing.T) {
	want := map[string][]string{
		"brw_read":             {"include", "max_chars", "offset", "max_links", "max_headings", "section"},
		"brw_console":          {"only_errors", "level", "pattern", "limit", "clear"},
		"brw_network_requests": {"filter", "pattern", "limit"},
		"brw_network_capture":  {"filter", "pattern", "limit"},
		"brw_press":            {"repeat"},
		"brw_scroll":           {"repeat"},
		"brw_trace":            {"format", "guards", "include_failed"},
		"brw_window_resize":    {"width", "height", "left", "top", "state"},
	}
	for tool, params := range want {
		props := toolProperties(t, tool)
		for _, param := range params {
			if _, ok := props[param]; !ok {
				t.Errorf("%s does not advertise %q, so an agent cannot use it", tool, param)
			}
		}
	}
}

func TestMinimalProfileIsTheSmallestSurface(t *testing.T) {
	full := (&Server{toolProfile: "all"}).advertisedTools()
	core := (&Server{toolProfile: "core"}).advertisedTools()
	minimal := (&Server{toolProfile: "minimal"}).advertisedTools()

	if !(len(minimal) < len(core) && len(core) < len(full)) {
		t.Fatalf("profiles are not strictly nested: minimal=%d core=%d all=%d",
			len(minimal), len(core), len(full))
	}
	if len(minimal) != len(minimalToolNames) {
		t.Fatalf("minimal advertised %d tools, want %d — every entry must name a real tool",
			len(minimal), len(minimalToolNames))
	}

	// The minimal surface still has to complete ordinary web work: reach a page,
	// see its controls, act, and confirm.
	got := map[string]bool{}
	for _, tl := range minimal {
		name, _ := tl["name"].(string)
		got[name] = true
	}
	for _, essential := range []string{"brw_open", "brw_snapshot", "brw_click", "brw_fill", "brw_observe", "brw_batch"} {
		if !got[essential] {
			t.Errorf("minimal profile is missing %q, which ordinary web work needs", essential)
		}
	}
}

// Narrowing the profile has to actually shrink the catalogue an agent pays for
// on every request, not merely list fewer names.
func TestNarrowerProfilesCostFewerBytes(t *testing.T) {
	size := func(profile string) int {
		encoded, err := json.Marshal((&Server{toolProfile: profile}).advertisedTools())
		if err != nil {
			t.Fatalf("marshal %s catalogue: %v", profile, err)
		}
		return len(encoded)
	}
	all, core, minimal := size("all"), size("core"), size("minimal")
	if !(minimal < core && core < all) {
		t.Fatalf("catalogue bytes are not strictly ordered: minimal=%d core=%d all=%d", minimal, core, all)
	}
	// The minimal profile exists to be dramatically cheaper; a marginal saving
	// would not justify a separate profile.
	if minimal*2 > all {
		t.Fatalf("minimal catalogue is %d bytes vs all at %d — less than half the size was the point", minimal, all)
	}
}

func TestUnknownProfileFallsBackToFullSurface(t *testing.T) {
	full := (&Server{toolProfile: "all"}).advertisedTools()
	// A typo in a client config must degrade to the full surface, never to a
	// server that advertises nothing.
	if got := (&Server{toolProfile: "minimial"}).advertisedTools(); len(got) != len(full) {
		t.Fatalf("unknown profile advertised %d tools, want the full %d", len(got), len(full))
	}
	if ValidToolProfile("minimial") {
		t.Fatal("ValidToolProfile accepted a typo, so nothing would warn the operator")
	}
	for _, name := range []string{"all", "core", "minimal", "auto"} {
		if !ValidToolProfile(name) {
			t.Errorf("ValidToolProfile(%q) = false", name)
		}
	}
	if got := strings.Join(ToolProfileNames(), ","); got != "all,auto,core,minimal" {
		t.Fatalf("ToolProfileNames() = %q, want a sorted all,auto,core,minimal", got)
	}
}

// The descriptions are the agent's only instruction manual. The trim pass cut
// exposition, not the sentences that decide which tool gets called.
func TestSteeringGuidanceSurvivedTheTrim(t *testing.T) {
	steering := map[string]string{
		"brw_screenshot":     "you almost never need this",
		"brw_batch":          "PREFERRED for multi-step flows",
		"brw_replay_request": "BLOCKED",
		"brw_open":           "never close pre-existing tabs",
		"brw_observe":        "INSTEAD",
	}
	for tool, phrase := range steering {
		desc, _ := toolByName(t, tool)["description"].(string)
		if !strings.Contains(desc, phrase) {
			t.Errorf("%s lost its steering guidance %q:\n%s", tool, phrase, desc)
		}
	}
}
