package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func autoServer() *Server {
	return &Server{manager: fakeController{}, toolProfile: autoProfile}
}

func matchNames(matches []discoveryMatch) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.Name)
	}
	return out
}

func relevantNames(matches []discoveryMatch) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if m.Relevant {
			out = append(out, m.Name)
		}
	}
	return out
}

func TestAutoProfileStartsMinimalAndGrows(t *testing.T) {
	s := autoServer()

	start := s.advertisedToolNames()
	if !start[discoveryToolName] {
		t.Fatal("auto profile does not advertise brw_tools, so nothing can be discovered")
	}
	if start["brw_downloads"] {
		t.Fatal("auto profile advertises the long tail up front")
	}
	if len(start) != len(minimalToolNames)+1 {
		t.Fatalf("auto starts with %d tools, want the minimal set plus brw_tools", len(start))
	}

	result, err := s.discoverTools("download a file")
	if err != nil {
		t.Fatal(err)
	}
	if result.Unlocked == 0 {
		t.Fatalf("search unlocked nothing: %+v", result)
	}

	after := s.advertisedToolNames()
	if !after["brw_downloads"] {
		t.Fatalf("brw_downloads not advertised after discovery; matches were %v", matchNames(result.Matches))
	}
	if len(after) <= len(start) {
		t.Fatalf("catalogue did not grow: %d -> %d", len(start), len(after))
	}
}

// Substring matching scored on accidents — "file" sits inside "profile", so a
// file search matched brw_identity and unlocked a dozen unrelated tools,
// undoing the saving the mechanism exists for.
func TestSearchDoesNotMatchInsideLongerWords(t *testing.T) {
	terms := searchTerms("upload a file")
	for _, tool := range tools() {
		name, _ := tool["name"].(string)
		if name != "brw_identity" {
			continue
		}
		description, _ := tool["description"].(string)
		if !strings.Contains(strings.ToLower(description), "profile") {
			t.Skip("brw_identity no longer mentions profile; the regression this guards is moot")
		}
		if score := scoreTool(name, description, terms); score > 0 {
			t.Fatalf("brw_identity scored %d for a file query — \"file\" matched inside \"profile\"", score)
		}
	}
}

func TestSearchMatchesPluralAndParticipleForms(t *testing.T) {
	tokens := tokenSet("brw_downloads")
	for _, term := range []string{"download", "downloads"} {
		if !matchesToken(tokens, term) {
			t.Errorf("%q did not match brw_downloads", term)
		}
	}
	// A shared stem needs a real suffix, not any prefix relationship.
	if sharesStem("profile", "file") {
		t.Error("profile and file treated as the same stem")
	}
	if sharesStem("press", "pre") {
		t.Error("a three-letter prefix was treated as a stem")
	}
}

// A weak description hit is worth showing — it might be the tool the agent
// meant — but not worth carrying in the catalogue on every later turn.
func TestSearchShowsWeakHitsWithoutUnlockingThem(t *testing.T) {
	s := autoServer()
	result, err := s.discoverTools("download a file")
	if err != nil {
		t.Fatal(err)
	}
	if len(relevantNames(result.Matches)) >= len(result.Matches) {
		t.Skip("this query produced no weak hits to check")
	}
	if len(relevantNames(result.Matches)) > maxUnlockPerSearch {
		t.Fatalf("unlocked %d tools from one search, cap is %d", len(relevantNames(result.Matches)), maxUnlockPerSearch)
	}
}

func TestSearchCapsCatalogueGrowthPerQuery(t *testing.T) {
	s := autoServer()
	// A deliberately vague query matches many descriptions weakly.
	result, err := s.discoverTools("browser tab element state")
	if err != nil {
		t.Fatal(err)
	}
	if result.Unlocked > maxUnlockPerSearch {
		t.Fatalf("one vague query unlocked %d tools, cap is %d", result.Unlocked, maxUnlockPerSearch)
	}
}

// Disclosure narrows what is advertised, never what is permitted. A client that
// ignores list_changed must still be able to call anything.
func TestUndiscoveredToolsRemainCallable(t *testing.T) {
	s := autoServer()
	if s.advertisedToolNames()["brw_window_bounds"] {
		t.Skip("brw_window_bounds is advertised by default; pick another tool to prove the point")
	}
	result, rpcErr := s.callTool(context.Background(), "brw_window_bounds", json.RawMessage(`{}`))
	if rpcErr != nil {
		t.Fatalf("undiscovered tool rejected: %v", rpcErr)
	}
	if result == nil {
		t.Fatal("undiscovered tool returned nothing")
	}
}

func TestListChangedOnlyAdvertisedInAutoMode(t *testing.T) {
	if !(&Server{toolProfile: autoProfile}).supportsListChanged() {
		t.Error("auto mode does not advertise listChanged, but its catalogue does change")
	}
	for _, profile := range []string{"all", "core", "minimal", ""} {
		if (&Server{toolProfile: profile}).supportsListChanged() {
			t.Errorf("profile %q advertises listChanged but never sends one", profile)
		}
	}
}

func TestDiscoveryNotifiesOnlyWhenTheCatalogueMoved(t *testing.T) {
	s := autoServer()
	var mu sync.Mutex
	var methods []string
	s.setNotifier(func(method string, _ any) {
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()
	})

	call := func(query string) discoveryResult {
		t.Helper()
		result, err := s.discoverTools(query)
		if err != nil {
			t.Fatal(err)
		}
		if result.Unlocked > 0 && s.supportsListChanged() {
			s.announceToolsChanged()
		}
		return result
	}

	first := call("download a file")
	if first.Unlocked == 0 {
		t.Fatal("first search unlocked nothing")
	}
	second := call("download a file")
	if second.Unlocked != 0 {
		t.Fatalf("repeat search unlocked %d again", second.Unlocked)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 1 || methods[0] != "notifications/tools/list_changed" {
		t.Fatalf("notifications = %v, want exactly one list_changed", methods)
	}
}

func TestDiscoveryRejectsEmptyQuery(t *testing.T) {
	if _, err := autoServer().discoverTools("   "); err == nil {
		t.Fatal("empty query accepted")
	}
}

func TestDiscoveryMissExplainsTheFallback(t *testing.T) {
	result, err := autoServer().discoverTools("zzzzqqqq")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 0 {
		t.Fatalf("nonsense query matched %v", matchNames(result.Matches))
	}
	if result.Matches == nil {
		t.Fatal("empty matches is nil; it would marshal as null")
	}
	// A miss must not leave the agent stuck: every tool is callable regardless.
	if !strings.Contains(result.Note, "callable") {
		t.Fatalf("miss note does not mention the fallback: %q", result.Note)
	}
}

func TestFirstSentenceTrimsToTheOpeningClaim(t *testing.T) {
	got := firstSentence("Does one thing. Then explains at length why, for several more sentences.")
	if got != "Does one thing." {
		t.Fatalf("firstSentence = %q", got)
	}
	long := strings.Repeat("word ", 60)
	if trimmed := firstSentence(long); len(trimmed) > 170 {
		t.Fatalf("unpunctuated description not capped: %d chars", len(trimmed))
	}
	if firstSentence("  ") != "" {
		t.Fatal("blank description did not produce an empty summary")
	}
}

func TestUnlockedToolsIsConcurrencySafe(t *testing.T) {
	var unlocked unlockedTools
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlocked.unlock([]string{"brw_downloads", "brw_console"})
			_ = unlocked.has("brw_downloads")
			_ = unlocked.count()
		}()
	}
	wg.Wait()
	if unlocked.count() != 2 {
		t.Fatalf("count = %d, want 2 after concurrent unlocks", unlocked.count())
	}
}
