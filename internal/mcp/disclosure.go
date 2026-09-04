package mcp

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Progressive tool disclosure.
//
// An MCP client re-sends the whole tool catalogue on every request, so the
// catalogue is a fixed cost on every turn rather than a one-off. brw's full
// surface is over 13k approximate tokens; a browser agent uses a dozen tools in a
// normal session and pays for all 54 every time it thinks.
//
// The "auto" profile advertises the minimal working set plus brw_tools. When an
// agent needs something outside that set it searches for it, the server unlocks
// the matches for the rest of the session and emits notifications/tools/
// list_changed, and the client refetches to get the full definitions. The
// catalogue grows to fit the task instead of being paid for up front.
//
// Every tool stays callable under every profile, so a client that ignores
// listChanged is never blocked — it can call an unadvertised tool directly and
// it works. Disclosure narrows what is advertised, never what is permitted.

// autoProfile is the profile name that turns on progressive disclosure.
const autoProfile = "auto"

// discoveryToolName is advertised in auto mode as the way into everything else.
const discoveryToolName = "brw_tools"

// maxDiscoveryResults bounds one search. A query that matches half the surface
// is a sign the agent should search for what it actually needs next, not a
// reason to hand back the whole catalogue it was trying to avoid.
const maxDiscoveryResults = 12

// maxUnlockPerSearch bounds how far the catalogue can grow from one query.
// Search again for the next thing; that is cheaper than carrying tools the
// agent never calls on every turn for the rest of the session.
const maxUnlockPerSearch = 4

// unlockedTools tracks which tools this session has disclosed. It only ever
// grows: a tool an agent needed once will very likely be needed again, and
// re-hiding it would churn the client's catalogue for no saving.
type unlockedTools struct {
	mu    sync.RWMutex
	names map[string]bool
}

func (u *unlockedTools) has(name string) bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.names[name]
}

// unlock adds names and reports how many were not already disclosed, so the
// caller can skip the list_changed notification when nothing actually changed.
func (u *unlockedTools) unlock(names []string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.names == nil {
		u.names = map[string]bool{}
	}
	added := 0
	for _, name := range names {
		if !u.names[name] {
			u.names[name] = true
			added++
		}
	}
	return added
}

func (u *unlockedTools) count() int {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return len(u.names)
}

// snapshot copies the unlocked set under one lock, so a catalogue built from it
// reflects a single consistent moment rather than a set that moved mid-scan.
func (u *unlockedTools) snapshot() map[string]bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	out := make(map[string]bool, len(u.names))
	for name := range u.names {
		out[name] = true
	}
	return out
}

// discoveryMatch is one search hit: enough to decide whether this is the tool,
// without paying for the full definition until it is unlocked.
type discoveryMatch struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
	// Advertised reports that the tool was already in the catalogue, so an agent
	// can tell a genuine unlock from a restatement of what it already had.
	Advertised bool `json:"already_advertised,omitempty"`
	// Relevant marks a hit strong enough to add to the catalogue. Weaker hits
	// are still shown — one may be what was meant — but are not disclosed.
	Relevant bool `json:"relevant"`
}

// discoveryResult is what brw_tools returns.
type discoveryResult struct {
	Query   string           `json:"query"`
	Matches []discoveryMatch `json:"matches"`
	// Unlocked is how many of the matches were newly disclosed by this call.
	Unlocked int `json:"unlocked"`
	// Advertised is the catalogue size after this call.
	Advertised int    `json:"advertised_tools"`
	Truncated  bool   `json:"truncated,omitempty"`
	Note       string `json:"note"`
}

// searchTools ranks the full surface against a query. Matching is substring
// based on purpose: an agent searching "download" should find brw_downloads
// whether it phrased the need as a noun, a verb, or a whole sentence.
func searchTools(query string) []discoveryMatch {
	terms := searchTerms(query)
	type scored struct {
		match discoveryMatch
		score int
	}

	ranked := make([]scored, 0, 8)
	for _, tl := range tools() {
		name, _ := tl["name"].(string)
		description, _ := tl["description"].(string)
		if name == discoveryToolName {
			continue
		}
		score := scoreTool(name, description, terms)
		if score == 0 {
			continue
		}
		ranked = append(ranked, scored{
			match: discoveryMatch{Name: name, Summary: firstSentence(description)},
			score: score,
		})
	}

	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].match.Name < ranked[j].match.Name
	})

	// Only strong hits are unlocked; the rest are shown but not disclosed, since
	// every advertised tool costs tokens on every later turn.
	//
	// The bar is relative to the best hit, not absolute. When some tool matched
	// by name, require name strength so a passing mention in a description does
	// not get disclosed. When nothing matched by name — "run some javascript",
	// where the tool is brw_evaluate — the best available signal IS the
	// description, and an absolute bar would unlock nothing while still ranking
	// the right tool first.
	best := 0
	if len(ranked) > 0 {
		best = ranked[0].score
	}
	bar := best / 2
	if bar < 1 {
		bar = 1
	}
	if best >= nameMatchScore && bar < nameMatchScore {
		bar = nameMatchScore
	}

	out := make([]discoveryMatch, 0, len(ranked))
	for _, item := range ranked {
		item.match.Relevant = item.score >= bar
		out = append(out, item.match)
	}
	return out
}

// searchTerms splits a query into lowercase words, dropping the filler that a
// natural-language request carries ("how do I read the console messages").
func searchTerms(query string) []string {
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !('a' <= r && r <= 'z' || '0' <= r && r <= '9')
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if len(field) < 2 || searchStopWords[field] {
			continue
		}
		out = append(out, field)
	}
	return out
}

var searchStopWords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "for": true,
	"to": true, "of": true, "in": true, "on": true, "how": true, "do": true,
	"can": true, "i": true, "it": true, "is": true, "with": true, "that": true,
	"brw": true, "tool": true, "tools": true, "browser": true, "page": true,
}

// scoreTool weights a name match far above a description match: a tool whose
// name carries the term is far more likely to be the one being looked for than
// one that merely mentions it in passing.
//
// Both sides are tokenised rather than substring-matched. Raw substrings score
// on accidents — "file" is inside "profile", so a search for a file tool
// matched brw_identity — which then unlocked a dozen unrelated tools and undid
// the saving the whole mechanism exists for.
func scoreTool(name, description string, terms []string) int {
	if len(terms) == 0 {
		return 0
	}
	nameTokens := tokenSet(name)
	descTokens := tokenSet(description)

	score := 0
	for _, term := range terms {
		switch {
		case matchesToken(nameTokens, term):
			score += nameMatchScore
		case matchesToken(descTokens, term):
			score++
		}
	}
	return score
}

// nameMatchScore is what one term matching a tool's name is worth. It doubles
// as the unlock bar: a single name hit is a strong enough signal to disclose.
const nameMatchScore = 10

// tokenSet splits text into lowercase word tokens, so brw_upload_file yields
// {brw, upload, file}.
func tokenSet(text string) map[string]bool {
	out := map[string]bool{}
	for _, token := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !('a' <= r && r <= 'z' || '0' <= r && r <= '9')
	}) {
		out[token] = true
	}
	return out
}

// matchesToken accepts an exact token or a shared stem, so "download" matches
// "downloads" and "assert" matches "asserts", without "file" matching
// "profile". It is deliberately not a real stemmer: unrelated word forms
// ("navigate" vs "navigation") are left to the description match rather than
// guessed at, because a wrong stem unlocks the wrong tool.
func matchesToken(tokens map[string]bool, term string) bool {
	if tokens[term] {
		return true
	}
	for token := range tokens {
		if sharesStem(token, term) {
			return true
		}
	}
	return false
}

// sharesStem reports whether one token is the other plus a short suffix, which
// covers the plural and participle forms a query naturally uses. The prefix
// must be at least four characters so short words do not collide.
func sharesStem(a, b string) bool {
	if len(a) < len(b) {
		a, b = b, a
	}
	if len(b) < 4 || !strings.HasPrefix(a, b) {
		return false
	}
	switch a[len(b):] {
	case "s", "es", "d", "ed", "ing", "tion", "ion":
		return true
	default:
		return false
	}
}

// firstSentence trims a description to its opening claim, which is what a
// search result needs. The full definition arrives when the tool is unlocked.
func firstSentence(description string) string {
	description = strings.TrimSpace(description)
	if description == "" {
		return ""
	}
	if idx := strings.Index(description, ". "); idx > 0 {
		return description[:idx+1]
	}
	const cap = 160
	if len(description) > cap {
		trimmed := description[:cap]
		if space := strings.LastIndex(trimmed, " "); space > 0 {
			trimmed = trimmed[:space]
		}
		return trimmed + "…"
	}
	return description
}

// discoverTools runs one brw_tools search and unlocks what it found.
func (s *Server) discoverTools(query string) (discoveryResult, error) {
	if strings.TrimSpace(query) == "" {
		return discoveryResult{}, fmt.Errorf("query is required, for example %q", "download a file")
	}

	matches := searchTools(query)
	result := discoveryResult{Query: query}
	if len(matches) == 0 {
		result.Matches = []discoveryMatch{}
		result.Advertised = len(s.advertisedTools())
		result.Note = "no tool matched; try a different word for what you want to do, or call the tool directly — every brw tool is callable whether or not it is advertised"
		return result, nil
	}

	if len(matches) > maxDiscoveryResults {
		matches = matches[:maxDiscoveryResults]
		result.Truncated = true
	}

	// Cap what one search can disclose. A vague query ("which browser profile is
	// this") matches many descriptions weakly, and the relative bar lets them all
	// through; showing them is useful, but adding a dozen tools to the catalogue
	// for one question is the cost this mechanism exists to avoid. The agent
	// asked one thing — give it the best answers, not the long tail.
	advertised := s.advertisedToolNames()
	names := make([]string, 0, maxUnlockPerSearch)
	for i := range matches {
		matches[i].Advertised = advertised[matches[i].Name]
		if !matches[i].Relevant {
			continue
		}
		if len(names) >= maxUnlockPerSearch {
			matches[i].Relevant = false
			continue
		}
		names = append(names, matches[i].Name)
	}

	result.Matches = matches
	result.Unlocked = s.unlocked.unlock(names)
	result.Advertised = len(s.advertisedTools())
	result.Note = discoveryNote(result)
	return result, nil
}

func discoveryNote(result discoveryResult) string {
	notes := make([]string, 0, 3)
	if result.Unlocked > 0 {
		notes = append(notes, fmt.Sprintf("%d tool(s) added to the catalogue; their full definitions arrive on the next tools/list", result.Unlocked))
	} else {
		notes = append(notes, "every match was already advertised")
	}
	if result.Truncated {
		notes = append(notes, fmt.Sprintf("only the top %d matches are shown; narrow the query for the rest", maxDiscoveryResults))
	}
	notes = append(notes, "any brw tool can be called whether or not it is advertised")
	return strings.Join(notes, "; ")
}

// advertisedToolNames is the set currently in the catalogue.
func (s *Server) advertisedToolNames() map[string]bool {
	out := map[string]bool{}
	for _, tl := range s.advertisedTools() {
		if name, _ := tl["name"].(string); name != "" {
			out[name] = true
		}
	}
	return out
}

// discoveryTool is the definition advertised in auto mode.
func discoveryTool() map[string]any {
	return tool(discoveryToolName,
		"Find a brw tool by describing what you want to do, and add it to your tool catalogue. This session starts with the small set that covers ordinary web work; everything else — downloads, network capture, device emulation, incognito contexts, assertions, tab groups, WebMCP page tools — is discovered through here so you are not paying for the whole surface on every turn. Search with plain words (\"record network requests\", \"upload a file\", \"read the console\"); matches are added to your catalogue and their full definitions arrive on your next tools/list. Every brw tool is callable whether or not it has been discovered, so this is about what you are shown, never about what you are allowed to do.",
		object(map[string]any{
			"query": stringSchema("What you want to do, in plain words."),
		}, []string{"query"}))
}
