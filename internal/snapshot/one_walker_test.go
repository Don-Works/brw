package snapshot

import (
	"os"

	"github.com/chromedp/chromedp"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoFile reads a file relative to the repository root.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// elementVocabulary is the part of the walker's output that only something
// DECIDING what a control is has any reason to produce. "button" and "link" are
// deliberately absent: they are ordinary English and appear all over a service
// worker for other reasons, while these are ARIA role names a classifier emits.
var elementVocabulary = []string{
	"textbox",
	"combobox",
	"searchbox",
	"spinbutton",
	"menuitem",
	"listbox",
	"aria-haspopup",
	"data-brw-ref",
}

// TestTheExtensionDecidesNothingAboutElements is the structural half of "one
// implementation of the ranking rules".
//
// The extension used to carry FRAME_EXTRACT_SCRIPT: its own selector list, its
// own role mapping, its own accessible-name rules and its own visibility test,
// used to read cross-origin iframes. Two extractors that agree today disagree
// after the next change to either, and only one of them is covered by
// ref_stability_test — so the refs an agent got from a frame were minted by
// rules nothing guarded. The extension now relays an expression the daemon
// builds and interprets none of it.
//
// The test enumerates the vocabulary rather than looking for the old constant by
// name, so re-introducing the fork under any other name fails too.
func TestTheExtensionDecidesNothingAboutElements(t *testing.T) {
	worker := repoFile(t, filepath.Join("extension", "service_worker.js"))
	for _, token := range elementVocabulary {
		if strings.Contains(worker, token) {
			t.Errorf("extension/service_worker.js mentions %q: element vocabulary belongs to the one walker in internal/snapshot, not to a second extractor in the extension", token)
		}
	}
	// Anchor the vocabulary to the walker, so a rename there cannot leave this
	// test looking for words nothing produces any more.
	for _, token := range elementVocabulary {
		if !strings.Contains(SnapshotFunctionScript, token) {
			t.Errorf("the walker no longer produces %q; update elementVocabulary so this guard keeps matching what a fork would have to reproduce", token)
		}
	}
}

var (
	refScriptPattern   = regexp.MustCompile(`(?m)^const ([A-Za-z]+Script) = ` + "`" + `\(function\(ref[,)]`)
	findByRefPattern   = regexp.MustCompile(`function findByRef\(ref\)\s*\{([^}]*)\}`)
	selectorLookupWord = "data-brw-ref="
)

// TestEveryRefScriptResolvesThroughTheSharedLookup keeps the cross-origin refusal
// (and frame/shadow traversal, and ref recovery) holding at the PROPERTY rather
// than at whichever scripts someone remembered.
//
// Every in-page script that takes a ref resolves it through __abFindDeep. That is
// the single function that knows the frame tree, and it is where a ref inside a
// cross-origin iframe is named for what it is instead of coming back as a bare
// "not found". Eighteen scripts used to carry their own copy of the selector
// loop; a nineteenth copy would route around the guard silently, so the test
// enumerates the scripts and fails on one that looks up a ref for itself.
func TestEveryRefScriptResolvesThroughTheSharedLookup(t *testing.T) {
	source := repoFile(t, filepath.Join("internal", "snapshot", "scripts.go"))

	names := refScriptPattern.FindAllStringSubmatch(source, -1)
	if len(names) < 10 {
		t.Fatalf("found only %d ref-taking walker scripts; the pattern has stopped matching the file", len(names))
	}

	for _, match := range findByRefPattern.FindAllStringSubmatch(source, -1) {
		body := match[1]
		if !strings.Contains(body, "__abFindDeep(") {
			t.Errorf("a findByRef in scripts.go does not use the shared lookup: %s", strings.TrimSpace(body))
		}
	}

	// __abFindDeep is DEFINED by FrameWalkHelpers, so a script that calls it
	// without prepending them throws ReferenceError at the first ref it is given.
	// Three mouse scripts did exactly that: they carried a private shadow-only
	// root walk and no helpers, which is also why a mouse action could never
	// target a ref inside an iframe.
	for _, block := range strings.Split(source, "\nconst ")[1:] {
		name := strings.SplitN(block, " ", 2)[0]
		body := strings.SplitN(block, "\n\nconst ", 2)[0]
		header := strings.SplitN(body, "\n", 2)[0]
		if strings.Contains(body, "__abFindDeep(") && !strings.Contains(header, "FrameWalkHelpers") {
			t.Errorf("%s calls __abFindDeep but does not prepend FrameWalkHelpers, which defines it", name)
		}
	}

	// Nothing outside the shared helper may query the ref attribute directly:
	// that is how a private lookup gets written in the first place.
	for _, file := range []string{
		filepath.Join("internal", "snapshot", "scripts.go"),
		filepath.Join("internal", "snapshot", "annotate.go"),
		filepath.Join("internal", "extensionbridge", "bridge.go"),
	} {
		body := repoFile(t, file)
		if count := strings.Count(body, selectorLookupWord); count > 0 {
			t.Errorf("%s builds a %q selector itself (%d times); resolve refs through __abFindDeep so frame traversal and the cross-origin refusal apply", file, selectorLookupWord, count)
		}
	}
}

// TestTheInstallingCallAndTheInstalledCallAgreeOnRefs covers the seam the
// install-once change introduces on both transports.
//
// A document's FIRST snapshot runs the expression that installs the walker and
// calls it; every later snapshot of that document runs the call alone. The two
// are built by one function from one source, and this is what holds them to
// producing the same element list — a divergence would renumber every ref
// between an agent's first snapshot of a page and its second.
func TestTheInstallingCallAndTheInstalledCallAgreeOnRefs(t *testing.T) {
	ctx, _ := crossOriginFixture(t, innerFrameDoc)
	opts := SnapshotOptions{Mode: "all"}
	hot, cold := SnapshotCallExpressions(opts)

	var installing PageSnapshot
	if err := chromedp.Run(ctx, chromedp.Evaluate(cold, &installing)); err != nil {
		t.Fatalf("install-and-call: %v", err)
	}
	var installed PageSnapshot
	if err := chromedp.Run(ctx, chromedp.Evaluate(hot, &installed)); err != nil {
		t.Fatalf("call the installed walker: %v", err)
	}
	if len(installing.Elements) == 0 {
		t.Fatal("the install-and-call form returned no elements")
	}
	if len(installing.Elements) != len(installed.Elements) {
		t.Fatalf("install-and-call read %d elements, the call alone read %d",
			len(installing.Elements), len(installed.Elements))
	}
	for i := range installing.Elements {
		first, second := installing.Elements[i], installed.Elements[i]
		if first.Ref != second.Ref || first.Role != second.Role || first.Name != second.Name {
			t.Fatalf("element %d differs between the two forms: %+v vs %+v", i, first, second)
		}
	}
}

// TestCrossOriginRefIsNamedByTheSharedLookup pins the message __abFindDeep
// throws. The Go guards refuse a cross-origin ref before a script runs, but a ref
// can still reach the page through a path that does not go via the Controller
// interface, and "not found — re-run brw_snapshot" is advice that can never help
// for this ref.
func TestCrossOriginRefIsNamedByTheSharedLookup(t *testing.T) {
	if !strings.Contains(FrameWalkHelpers, "cross-origin iframe") {
		t.Fatal("__abFindDeep no longer names a cross-origin frame ref; it would report it as an ordinary missing ref")
	}
	for _, script := range []string{ResolveBoxScript, FocusElementScript, FillElementScript, HoverElementScript, CommitFieldScript, AssertVisibleScript} {
		if !strings.Contains(script, "__abFindDeep(") {
			t.Error("a ref-taking script resolves without the shared lookup, so the cross-origin ref would not be named there")
		}
	}
}
