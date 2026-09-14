package snapshot_test

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

// scriptCaseLabels extracts the case labels of GetScript's switch(what).
var scriptCaseLabels = regexp.MustCompile(`case '([a-z_]+)':`)

// TestGetKindsMatchScript compares the accepted vocabulary against the script
// that answers it, in BOTH directions.
//
// The two are separate literals — a Go map and a JavaScript switch — and nothing
// but this test keeps them together. A kind accepted by Validate that the switch
// does not answer throws "unknown get target" after the browser has already been
// driven; a case added to the switch and not to the map is refused before the
// page is touched, while the MCP schema (built from the same map) never offers
// it. Comparing the sets is what makes either drift a test failure rather than a
// surprise on merge.
func TestGetKindsMatchScript(t *testing.T) {
	inScript := map[string]bool{}
	for _, match := range scriptCaseLabels.FindAllStringSubmatch(snapshot.GetScript, -1) {
		inScript[match[1]] = true
	}
	if len(inScript) == 0 {
		t.Fatal("found no case labels in GetScript: the switch(what) shape this test reads has changed")
	}

	accepted := map[string]bool{}
	for _, name := range snapshot.GetKindNames() {
		accepted[name] = true
	}

	var acceptedButUnanswered, answeredButUnaccepted []string
	for name := range accepted {
		if !inScript[name] {
			acceptedButUnanswered = append(acceptedButUnanswered, name)
		}
	}
	for name := range inScript {
		if !accepted[name] {
			answeredButUnaccepted = append(answeredButUnaccepted, name)
		}
	}
	sort.Strings(acceptedButUnanswered)
	sort.Strings(answeredButUnaccepted)
	if len(acceptedButUnanswered) > 0 {
		t.Errorf("getKinds accepts %v, which GetScript's switch(what) does not answer: brw_get would drive the browser and then throw", acceptedButUnanswered)
	}
	if len(answeredButUnaccepted) > 0 {
		t.Errorf("GetScript answers %v, which getKinds rejects: the question is refused before the page is touched and the tool schema never offers it", answeredButUnaccepted)
	}
}

// TestGetScriptAnswersEveryAcceptedKind is the half the set comparison cannot
// do: it runs each accepted kind in real Chrome and proves the switch reaches a
// case, plus that the default arm still throws for anything outside the table.
// Without the negative case a switch whose default silently returned undefined
// would pass everything.
func TestGetScriptAnswersEveryAcceptedKind(t *testing.T) {
	ctx, _ := openFixture(t, getSurfaceFixture)

	// Targets that make each element-scoped kind answerable on the fixture.
	targets := map[string]string{
		"value":    "#name",
		"attr":     "#name",
		"count":    ".row",
		"box":      "#heading",
		"styles":   "#heading",
		"visible":  "#heading",
		"hidden":   "#gone",
		"enabled":  "#name",
		"disabled": "#go",
		"checked":  "#agree",
	}

	for _, kind := range snapshot.GetKindNames() {
		t.Run(kind, func(t *testing.T) {
			name := ""
			if kind == "attr" {
				name = "data-kind"
			}
			var out map[string]any
			expr := snapshot.BuildGetExpression(kind, targets[kind], name)
			if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &out)); err != nil {
				t.Fatalf("get %s: %v", kind, err)
			}
			if _, ok := out["value"]; !ok {
				t.Fatalf("get %s returned %#v, want a {value:...} answer", kind, out)
			}
		})
	}

	t.Run("outside the table", func(t *testing.T) {
		var out map[string]any
		err := chromedp.Run(ctx, chromedp.Evaluate(snapshot.BuildGetExpression("colour", "#heading", ""), &out))
		if err == nil {
			t.Fatalf("get colour returned %#v, want the script to throw", out)
		}
		if !strings.Contains(err.Error(), "unknown get target") {
			t.Fatalf("get colour failed with %q, want it to name the unknown target", err)
		}
	})
}
