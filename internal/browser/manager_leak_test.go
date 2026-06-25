package browser

import "testing"

// bareManager builds a Manager with just the per-tab/per-context bookkeeping
// maps initialized — enough to exercise the leak-cleanup helpers without a live
// Chrome (the helpers do no browser I/O).
func bareManager() *Manager {
	return &Manager{
		netCaptureTabs:    map[string]bool{},
		shadowPierceTabs:  map[string]bool{},
		webmcpTabs:        map[string]bool{},
		emulationStates:   map[string]deviceEmulationState{},
		incognitoContexts: map[string]bool{},
	}
}

// TestForgetTabCachesClearsEmulationState is the regression for the leaked
// emulationStates entry: CloseTab pruned the other per-tab maps but not the
// device-emulation map, so it grew unbounded across open/close churn.
func TestForgetTabCachesClearsAllPerTabMaps(t *testing.T) {
	m := bareManager()
	const id = "tab-1"
	m.netCaptureTabs[id] = true
	m.shadowPierceTabs[id] = true
	m.webmcpTabs[id] = true
	m.emulationStates[id] = deviceEmulationState{}

	m.forgetTabCaches(id)

	if n := len(m.netCaptureTabs) + len(m.shadowPierceTabs) + len(m.webmcpTabs) + len(m.emulationStates); n != 0 {
		t.Fatalf("forgetTabCaches left %d per-tab entries behind (emul=%d) — a leak on every closed tab",
			n, len(m.emulationStates))
	}
}

// TestIncognitoBookkeeping covers the tracking that lets Manager.Close dispose
// contexts a caller forgot to close: track adds, untrack (CloseContext) removes,
// and take (Close) drains the set.
func TestIncognitoBookkeeping(t *testing.T) {
	m := bareManager()
	m.trackIncognito("a")
	m.trackIncognito("b")
	m.trackIncognito("c")
	m.untrackIncognito("b") // models CloseContext("b")

	taken := m.takeIncognitoContexts()
	if len(taken) != 2 {
		t.Fatalf("takeIncognitoContexts returned %d ids, want 2 (a,c)", len(taken))
	}
	got := map[string]bool{}
	for _, id := range taken {
		got[id] = true
	}
	if !got["a"] || !got["c"] || got["b"] {
		t.Fatalf("taken = %v, want exactly {a, c}", taken)
	}
	if len(m.incognitoContexts) != 0 {
		t.Fatalf("takeIncognitoContexts must clear the set; %d entries remain", len(m.incognitoContexts))
	}
	// Idempotent: taking again on an empty set is safe and returns nothing.
	if again := m.takeIncognitoContexts(); len(again) != 0 {
		t.Fatalf("second take returned %d, want 0", len(again))
	}
}
