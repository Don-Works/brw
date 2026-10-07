package mcp

import (
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
)

const autoProfile = "auto"

const discoveryToolName = "brw_tools"

const maxDiscoveryResults = 12

const maxUnlockPerSearch = 4

type unlockedTools struct {
	mu    sync.RWMutex
	names map[string]bool
}

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

func (u *unlockedTools) snapshot() map[string]bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return maps.Clone(u.names)
}

type discoveryMatch struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
	// Advertised reports that the tool was already in the catalogue, so an agent can tell a genuine unlock from a restatement of what it already had.
	Advertised bool `json:"already_advertised,omitempty"`
	// Relevant marks a hit strong enough to add to the catalogue.
	Relevant bool `json:"relevant"`
}

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

const nameMatchScore = 10

func tokenSet(text string) map[string]bool {
	out := map[string]bool{}
	for _, token := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !('a' <= r && r <= 'z' || '0' <= r && r <= '9')
	}) {
		out[token] = true
	}
	return out
}

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

	advertised := s.advertisedToolNames()
	names := make([]string, 0, maxUnlockPerSearch)
	for i := range matches {
		matches[i].Advertised = advertised[matches[i].Name]
		if !matches[i].Relevant {
			continue
		}
		if matches[i].Advertised {
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
		notes = append(notes, "no new tools added to the catalogue")
	}
	if result.Truncated {
		notes = append(notes, fmt.Sprintf("only the top %d matches are shown; narrow the query for the rest", maxDiscoveryResults))
	}
	notes = append(notes, "any brw tool can be called whether or not it is advertised")
	return strings.Join(notes, "; ")
}

func (s *Server) advertisedToolNames() map[string]bool {
	out := map[string]bool{}
	for _, tl := range s.advertisedTools() {
		if name, _ := tl["name"].(string); name != "" {
			out[name] = true
		}
	}
	return out
}

func discoveryTool() map[string]any {
	return tool(discoveryToolName,
		"Find a brw tool by describing what you want to do, and add it to your tool catalogue. This session starts with the small set that covers ordinary web work; everything else — downloads, network capture, device emulation, incognito contexts, assertions, tab groups, WebMCP page tools — is discovered through here so you are not paying for the whole surface on every turn. Search with plain words (\"record network requests\", \"upload a file\", \"read the console\"); matches are added to your catalogue and their full definitions arrive on your next tools/list. Every brw tool is callable whether or not it has been discovered, so this is about what you are shown, never about what you are allowed to do.",
		object(map[string]any{
			"query": stringSchema("What you want to do, in plain words."),
		}, []string{"query"}))
}
