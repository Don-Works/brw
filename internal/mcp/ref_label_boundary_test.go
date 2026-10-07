package mcp

import (
	"fmt"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
)

func TestRefLabelsStayWithinTheirReportedTab(t *testing.T) {
	var labels refLabelStore
	labels.record("other", []snapshot.Element{{Ref: "e1", Name: "Pay invoice"}})
	for _, tab := range []string{"selected", ""} {
		if got := labels.label(tab, "e1"); got != "" {
			t.Fatalf("tab=%q borrowed another tab's risk label %q", tab, got)
		}
	}
	labels.record("", []snapshot.Element{{Ref: "e1", Name: "Untargeted button"}})
	if got := labels.label("", "e1"); got != "Untargeted button" {
		t.Fatalf("lost untargeted observation: %q", got)
	}
}

func TestRefLabelsCanRefreshAfterReachingTheCap(t *testing.T) {
	var labels refLabelStore
	for i := range maxRefLabelsPerTab {
		labels.record("selected", []snapshot.Element{{Ref: fmt.Sprintf("e%d", i), Name: "Continue"}})
	}
	labels.record("selected", []snapshot.Element{{Ref: "new", Name: "Ignored"}, {Ref: "e1", Name: "Pay invoice"}})
	if got := labels.label("selected", "e1"); got != "Pay invoice" {
		t.Fatalf("risk label stayed stale at capacity: %q", got)
	}
	if got := labels.label("selected", "new"); got != "" {
		t.Fatalf("label cap exceeded: %q", got)
	}
}
