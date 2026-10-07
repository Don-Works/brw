package mcp

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/Don-Works/brw/internal/snapshot"
)

type diffBaseline struct {
	URL      string
	Title    string
	Elements map[string]snapshot.Element
	Text     string
}

type diffStore struct {
	mu        sync.Mutex
	baselines map[string]diffBaseline
}

func (d *diffStore) put(tabID string, baseline diffBaseline) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.baselines == nil {
		d.baselines = make(map[string]diffBaseline)
	}
	d.baselines[tabID] = baseline
}

func (d *diffStore) get(tabID string) (diffBaseline, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	baseline, ok := d.baselines[tabID]
	return baseline, ok
}

// ElementChange names one element that appeared, disappeared or changed between the marked baseline and now.
type ElementChange struct {
	Ref  string `json:"ref"`
	Role string `json:"role,omitempty"`
	Name string `json:"name,omitempty"`
	// From and To are set only for a changed element, and carry the value or accessible name that actually moved.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// DiffResult answers "did my action change the page, and how?" without re-reading the whole page into context.
type DiffResult struct {
	Action string `json:"action"`
	TabID  string `json:"tab_id,omitempty"`
	// Changed is false when the page is materially identical to the baseline, which is the answer an agent most often needs and the cheapest to act on.
	Changed    bool `json:"changed"`
	URLChanged bool `json:"url_changed,omitempty"`
	// TextChanged reports that the page's visible prose moved even when no element was added, removed or updated.
	TextChanged  bool            `json:"text_changed,omitempty"`
	URLFrom      string          `json:"url_from,omitempty"`
	URLTo        string          `json:"url_to,omitempty"`
	TitleFrom    string          `json:"title_from,omitempty"`
	TitleTo      string          `json:"title_to,omitempty"`
	Added        []ElementChange `json:"added,omitempty"`
	Removed      []ElementChange `json:"removed,omitempty"`
	Updated      []ElementChange `json:"updated,omitempty"`
	AddedCount   int             `json:"added_count"`
	RemovedCount int             `json:"removed_count"`
	UpdatedCount int             `json:"updated_count"`
	// Truncated reports that the lists were capped; the counts remain exact.
	Truncated bool `json:"truncated,omitempty"`
	// Summary is the one-line verdict ("unchanged", "+3 ~1", "url a -> b") an agent can branch on without reading the lists.
	Summary string `json:"summary,omitempty"`
	Note    string `json:"note,omitempty"`
}

const maxDiffEntries = 40

const textFingerprintExpression = `(function(){
  var t = document.body ? document.body.innerText : '';
  var h = 0;
  for (var i = 0; i < t.length; i++) { h = (h * 31 + t.charCodeAt(i)) | 0; }
  return t.length + ':' + h;
})()`

func baselineFrom(snap snapshot.PageSnapshot) diffBaseline {
	elements := make(map[string]snapshot.Element, len(snap.Elements))
	for _, el := range snap.Elements {
		if el.Ref == "" {
			continue
		}
		elements[el.Ref] = el
	}
	return diffBaseline{URL: snap.URL, Title: snap.Title, Elements: elements}
}

func diffSnapshots(baseline diffBaseline, snap snapshot.PageSnapshot, text string) DiffResult {
	current := baselineFrom(snap)
	current.Text = text
	result := DiffResult{
		Action:    "compare",
		URLFrom:   baseline.URL,
		URLTo:     snap.URL,
		TitleFrom: baseline.Title,
		TitleTo:   snap.Title,
	}
	result.URLChanged = baseline.URL != snap.URL

	for ref, el := range current.Elements {
		before, existed := baseline.Elements[ref]
		if !existed {
			result.AddedCount++
			result.Added = insertChange(result.Added, ElementChange{Ref: ref, Role: el.Role, Name: el.Name})
			continue
		}
		if from, to, moved := elementMoved(before, el); moved {
			result.UpdatedCount++
			result.Updated = insertChange(result.Updated, ElementChange{Ref: ref, Role: el.Role, Name: el.Name, From: from, To: to})
		}
	}
	for ref, el := range baseline.Elements {
		if _, stillThere := current.Elements[ref]; !stillThere {
			result.RemovedCount++
			result.Removed = insertChange(result.Removed, ElementChange{Ref: ref, Role: el.Role, Name: el.Name})
		}
	}

	result.Truncated = result.AddedCount > len(result.Added) ||
		result.RemovedCount > len(result.Removed) ||
		result.UpdatedCount > len(result.Updated)

	result.TextChanged = baseline.Text != "" && current.Text != "" && baseline.Text != current.Text
	result.Changed = result.URLChanged || result.TextChanged ||
		baseline.Title != snap.Title ||
		result.AddedCount > 0 || result.RemovedCount > 0 || result.UpdatedCount > 0
	if !result.Changed {
		result.Note = "the page is materially unchanged since the mark: same URL, title and elements"
	}
	result.Summary = diffSummary(result)
	return result
}

func elementMoved(before, after snapshot.Element) (from, to string, moved bool) {
	switch {
	case before.Value != after.Value:
		return before.Value, after.Value, true
	case before.Name != after.Name:
		return before.Name, after.Name, true
	case before.Role != after.Role:
		return before.Role, after.Role, true
	case before.Href != after.Href:
		return before.Href, after.Href, true
	}
	return "", "", false
}

func insertChange(changes []ElementChange, change ElementChange) []ElementChange {
	index, _ := slices.BinarySearchFunc(changes, change, func(a, b ElementChange) int {
		return cmp.Or(cmp.Compare(a.Role, b.Role), cmp.Compare(a.Ref, b.Ref))
	})
	if index >= maxDiffEntries {
		return changes
	}
	changes = slices.Insert(changes, index, change)
	return changes[:min(len(changes), maxDiffEntries)]
}

func diffSummary(result DiffResult) string {
	if !result.Changed {
		return "unchanged"
	}
	parts := make([]string, 0, 4)
	if result.URLChanged {
		parts = append(parts, fmt.Sprintf("url %s -> %s", result.URLFrom, result.URLTo))
	}
	if result.AddedCount > 0 {
		parts = append(parts, fmt.Sprintf("+%d", result.AddedCount))
	}
	if result.RemovedCount > 0 {
		parts = append(parts, fmt.Sprintf("-%d", result.RemovedCount))
	}
	if result.UpdatedCount > 0 {
		parts = append(parts, fmt.Sprintf("~%d", result.UpdatedCount))
	}
	if result.TextChanged && len(parts) == 0 {
		parts = append(parts, "text")
	}
	return strings.Join(parts, " ")
}

const activeDiffKey = "\x00active"
