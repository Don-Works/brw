package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
)

// Every lane brw can report has to get an answer out of skipReason, and the
// answer has to follow the lane's declared capabilities.
//
// The earlier spelling named lanes, so a scenario requiring "direct-cdp" was
// skipped against chrome-opt-in-cdp — a lane with the same browser-target CDP —
// and the conformance suite reported the whole lane as untested. A skip is not
// a failure, so nothing said so.
func TestSkipReasonAnswersForEveryTransport(t *testing.T) {
	transports := brwidentity.Transports()
	if len(transports) < 3 {
		t.Fatalf("brwidentity reports %d transports; this test exists to cover all of them", len(transports))
	}
	for _, transport := range transports {
		caps, known := brwidentity.Capabilities(transport)
		if !known {
			t.Fatalf("transport %q is unclassified", transport)
		}
		for requirement, rule := range transportRequirements {
			t.Run(transport+"/"+requirement, func(t *testing.T) {
				sc := scenario{ID: "fixture", Requires: []string{requirement}}
				reason := skipReason(sc, true, true, true, transport)
				if rule.has(caps) {
					if reason != "" {
						t.Fatalf("skipped a scenario the %s lane can run: %s", transport, reason)
					}
					return
				}
				if reason == "" {
					t.Fatalf("ran a %s scenario on %s, which does not have it", requirement, transport)
				}
				if !strings.Contains(reason, transport) {
					t.Fatalf("skip reason %q does not name the lane", reason)
				}
			})
		}
	}
}

// A requirement value nothing classifies must stop the run, not quietly skip
// the scenario: an unrecognised requirement is a coverage gap that looks like
// a clean result.
func TestSuiteRequirementsAreClosed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		requires []string
		refused  bool
	}{
		{name: "a capability property", requires: []string{"browser-target"}},
		{name: "a run flag", requires: []string{"network"}},
		{name: "a lane name, which is no longer a requirement", requires: []string{"direct-cdp"}, refused: true},
		{name: "a typo", requires: []string{"browser_target"}, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "suite.json")
			body, err := json.Marshal(map[string]any{
				"version": 1,
				"scenarios": []map[string]any{{
					"id":       "fixture",
					"name":     "fixture",
					"requires": tc.requires,
					"actions":  []map[string]any{},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = loadSuite(path)
			if tc.refused && err == nil {
				t.Fatalf("loadSuite accepted requires %v, so every scenario using it would silently skip", tc.requires)
			}
			if !tc.refused && err != nil {
				t.Fatalf("loadSuite refused requires %v: %v", tc.requires, err)
			}
		})
	}
}

// Every lane has to be exercised for a capability it actually has, not merely
// reached by the scenarios that ask for nothing.
//
// Counting every scenario was the earlier spelling and it proved nothing: a
// scenario with no `requires` runs on every lane, so the map was full before any
// capability-bearing scenario was consulted. Rewriting both `browser-target`
// and `cdp-session` in core.json to `extension-apis` — which leaves every
// CDP lane with no capability coverage at all — still passed it. Only the
// scenarios that name a transport capability are counted here, which is the
// population the claim is about.
func TestShippedSuitesExerciseEveryLane(t *testing.T) {
	covered := map[string]bool{}
	capabilityScenarios := 0
	for _, name := range []string{"core.json", "decathlon.json"} {
		suite, err := loadSuite(filepath.Join("..", "..", "tests", "scenarios", name))
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		for _, sc := range suite.Scenarios {
			if !declaresATransportCapability(sc) {
				continue
			}
			capabilityScenarios++
			for _, transport := range brwidentity.Transports() {
				if skipReason(sc, true, true, true, transport) == "" {
					covered[transport] = true
				}
			}
		}
	}
	if capabilityScenarios == 0 {
		t.Fatal("no shipped scenario declares a transport capability, so this test would pass whatever the lanes could do")
	}
	for _, transport := range brwidentity.Transports() {
		if !covered[transport] {
			t.Errorf("no shipped scenario exercises a capability the %s lane has; the suite would report that lane green while running nothing that needs it", transport)
		}
	}
}

// declaresATransportCapability reports whether a scenario asks for anything
// about the lane. A scenario that asks only for a run flag, or for nothing,
// runs everywhere and so says nothing about which lanes are covered.
func declaresATransportCapability(sc scenario) bool {
	for _, req := range sc.Requires {
		if _, ok := transportRequirements[req]; ok {
			return true
		}
	}
	return false
}
