package devtools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/devtools/axe"
)

// TestEmbeddedAxeMatchesItsDeclaredVersion is what stops the vendored bundle
// and the constant beside it drifting: the engine announces its own version in
// the banner comment the minifier keeps, so the two can be compared without
// running it. README.md points at this test.
func TestEmbeddedAxeMatchesItsDeclaredVersion(t *testing.T) {
	if len(axe.Source) < 100000 {
		t.Fatalf("the embedded engine is %d bytes; that is not an axe-core bundle", len(axe.Source))
	}
	banner := axe.Source
	if index := strings.IndexByte(banner, '\n'); index > 0 {
		banner = banner[:index]
	}
	if !strings.Contains(banner, "axe v"+axe.Version) {
		t.Fatalf("bundle banner %q does not carry the declared version %q", banner, axe.Version)
	}
	if !strings.Contains(axe.Source, "Mozilla Public") {
		t.Error("the bundle no longer carries its licence header, which has to travel with it")
	}
	if !strings.Contains(AxeInstallExpression(), axe.Source) {
		t.Error("the install expression does not carry the embedded bundle, so an audit would have nothing to run")
	}
}

// axeDocument builds an axe-shaped result for the summarizer. It is written out
// as JSON rather than as structs because the summarizer's job is to read what
// the engine actually emits.
func axeDocument(violations string) json.RawMessage {
	return json.RawMessage(`{"violations":[` + violations + `],"incomplete":[],"passes":[{"id":"region"}],"inapplicable":[{"id":"video-caption"},{"id":"audio-caption"}]}`)
}

func rule(id, impact string, nodes string) string {
	return `{"id":"` + id + `","impact":"` + impact + `","help":"h","helpUrl":"https://example.com/` + id + `","tags":["wcag2aa"],"nodes":[` + nodes + `]}`
}

func TestSummarizeAudit(t *testing.T) {
	tests := []struct {
		name        string
		document    json.RawMessage
		opts        AuditOptions
		wantOrder   []string
		wantNodes   int
		wantImpact  map[string]int
		wantRefs    []string
		wantTargets []string
		wantTrunc   bool
	}{
		{
			name: "worst impact first, then most nodes",
			document: axeDocument(strings.Join([]string{
				rule("minor-thing", "minor", `{"target":["#a"],"brw_ref":"e1"}`),
				rule("critical-thing", "critical", `{"target":["#b"],"brw_ref":"e2"}`),
				rule("serious-many", "serious", `{"target":["#c"],"brw_ref":"e3"},{"target":["#d"],"brw_ref":"e4"}`),
				rule("serious-one", "serious", `{"target":["#e"],"brw_ref":"e5"}`),
			}, ",")),
			wantOrder:  []string{"critical-thing", "serious-many", "serious-one", "minor-thing"},
			wantNodes:  5,
			wantImpact: map[string]int{"critical": 1, "serious": 3, "minor": 1},
		},
		{
			name: "max_rules cuts the list but never the totals",
			document: axeDocument(strings.Join([]string{
				rule("critical-thing", "critical", `{"target":["#b"],"brw_ref":"e2"}`),
				rule("minor-thing", "minor", `{"target":["#a"],"brw_ref":"e1"}`),
			}, ",")),
			opts:       AuditOptions{MaxRules: 1},
			wantOrder:  []string{"critical-thing"},
			wantNodes:  2,
			wantImpact: map[string]int{"critical": 1, "minor": 1},
			wantTrunc:  true,
		},
		{
			name:      "max_refs bounds the refs per rule",
			document:  axeDocument(rule("many", "serious", `{"target":["#a"],"brw_ref":"e1"},{"target":["#b"],"brw_ref":"e2"},{"target":["#c"],"brw_ref":"e3"}`)),
			opts:      AuditOptions{MaxRefs: 2},
			wantOrder: []string{"many"},
			wantNodes: 3,
			wantRefs:  []string{"e1", "e2"},
		},
		{
			// The rule is ranked by the worst node so it sorts first, but
			// by_impact counts elements: one minor element and one critical
			// element is one of each. Bucketing both as critical would tell a
			// caller sizing the work there are two critical elements to fix.
			name: "a rule with no impact of its own takes the worst its nodes carry",
			document: axeDocument(
				`{"id":"mixed","impact":"","help":"h","helpUrl":"","tags":[],"nodes":[{"target":["#a"],"impact":"minor","brw_ref":"e1"},{"target":["#b"],"impact":"critical","brw_ref":"e2"}]}`),
			wantOrder:  []string{"mixed"},
			wantNodes:  2,
			wantImpact: map[string]int{"critical": 1, "minor": 1},
		},
		{
			// Same rule for a rule that does carry an impact: axe labels nodes
			// individually and the per-element count follows the node.
			name: "nodes are counted at their own impact under a labelled rule",
			document: axeDocument(
				`{"id":"labelled","impact":"serious","help":"h","helpUrl":"","tags":[],"nodes":[{"target":["#a"],"impact":"moderate","brw_ref":"e1"},{"target":["#b"],"impact":"serious","brw_ref":"e2"},{"target":["#c"],"brw_ref":"e3"}]}`),
			wantOrder:  []string{"labelled"},
			wantNodes:  3,
			wantImpact: map[string]int{"moderate": 1, "serious": 2},
		},
		{
			name: "a frame and shadow target chain reads as one selector",
			document: axeDocument(
				`{"id":"framed","impact":"serious","help":"h","helpUrl":"","tags":[],"nodes":[{"target":["#frame","#inner"],"brw_ref":""},{"target":[["#host","#shadow"]],"brw_ref":""}]}`),
			wantOrder:   []string{"framed"},
			wantNodes:   2,
			wantImpact:  map[string]int{"serious": 2},
			wantRefs:    []string{"", ""},
			wantTargets: []string{"#frame >> #inner", "#host >> #shadow"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SummarizeAudit(RawAudit{
				OK:     true,
				Ours:   true,
				Engine: "axe-core 4.10.2",
				URL:    "https://example.com/page",
				Title:  "Page",
				Report: tt.document,
			}, tt.opts, time.Unix(0, 0))
			if err != nil {
				t.Fatalf("summarize: %v", err)
			}
			var order []string
			for _, rule := range got.Rules {
				order = append(order, rule.ID)
			}
			if strings.Join(order, ",") != strings.Join(tt.wantOrder, ",") {
				t.Fatalf("rules = %v, want %v", order, tt.wantOrder)
			}
			if got.ViolationNodes != tt.wantNodes {
				t.Errorf("violation_nodes = %d, want %d", got.ViolationNodes, tt.wantNodes)
			}
			if got.Truncated != tt.wantTrunc {
				t.Errorf("truncated = %v, want %v", got.Truncated, tt.wantTrunc)
			}
			for impact, want := range tt.wantImpact {
				if got.ByImpact[impact] != want {
					t.Errorf("by_impact[%s] = %d, want %d (all: %v)", impact, got.ByImpact[impact], want, got.ByImpact)
				}
			}
			if tt.wantRefs != nil {
				if strings.Join(got.Rules[0].Refs, ",") != strings.Join(tt.wantRefs, ",") {
					t.Errorf("refs = %v, want %v", got.Rules[0].Refs, tt.wantRefs)
				}
			}
			if tt.wantTargets != nil {
				if strings.Join(got.Rules[0].Targets, ",") != strings.Join(tt.wantTargets, ",") {
					t.Errorf("targets = %v, want %v", got.Rules[0].Targets, tt.wantTargets)
				}
			}
			if got.Passes != 1 || got.Inapplicable != 2 {
				t.Errorf("passes/inapplicable = %d/%d, want 1/2", got.Passes, got.Inapplicable)
			}
			if got.Note != "" {
				t.Errorf("note = %q, want none when brw's own engine ran", got.Note)
			}
			// The document bound for the artifact has to carry every node,
			// including the ones the caps dropped from the summary.
			for _, id := range collectRuleIDs(tt.document) {
				if !strings.Contains(string(got.Report), `"`+id+`"`) {
					t.Errorf("the stored report dropped rule %s", id)
				}
			}
		})
	}
}

// TestSummarizeAuditNamesWhatRanAndWhatItLeft: an audit is read-shaped but not
// effect-free, and the engine that produced a rule id may not be the embedded
// one. Both facts belong in the answer, because a caller cannot see either.
func TestSummarizeAuditNamesWhatRanAndWhatItLeft(t *testing.T) {
	tests := []struct {
		name            string
		raw             RawAudit
		wantEffects     []string
		wantNotEffects  []string
		wantNote        []string
		wantNoteAbsent  bool
		wantEngineNamed string
	}{
		{
			// brw injected the engine, and it stays: the probe on the next
			// audit finds it and skips the half-megabyte re-injection.
			name:        "brw's own engine is left installed and the answer says so",
			raw:         RawAudit{OK: true, Ours: true, Engine: "axe-core " + axe.Version, Report: axeDocument("")},
			wantEffects: []string{"data-brw-ref", "axe-core " + axe.Version, "window.axe"},
			// Nothing to warn about: the embedded engine is what ran.
			wantNoteAbsent: true,
		},
		{
			// The page shipped its own engine, so brw added nothing beyond the
			// refs — and the rule ids came from a version that is not this one.
			name:           "a borrowed engine is named next to the embedded version",
			raw:            RawAudit{OK: true, Ours: false, Engine: "axe-core 3.5.5", Report: axeDocument("")},
			wantEffects:    []string{"data-brw-ref", "the page's own axe-core"},
			wantNotEffects: []string{"window.axe"},
			wantNote:       []string{"axe-core 3.5.5", axe.Version},
		},
		{
			name:        "an engine that would not name itself still reports the embedded version",
			raw:         RawAudit{OK: true, Ours: false, Engine: "", Report: axeDocument("")},
			wantEffects: []string{"data-brw-ref"},
			wantNote:    []string{"unidentified", axe.Version},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SummarizeAudit(tt.raw, AuditOptions{}, time.Unix(0, 0))
			if err != nil {
				t.Fatalf("summarize: %v", err)
			}
			for _, want := range tt.wantEffects {
				if !strings.Contains(got.PageEffects, want) {
					t.Errorf("page_effects = %q, want it to mention %q", got.PageEffects, want)
				}
			}
			for _, unwanted := range tt.wantNotEffects {
				if strings.Contains(got.PageEffects, unwanted) {
					t.Errorf("page_effects = %q, want it not to claim %q", got.PageEffects, unwanted)
				}
			}
			for _, want := range tt.wantNote {
				if !strings.Contains(got.Note, want) {
					t.Errorf("note = %q, want it to mention %q", got.Note, want)
				}
			}
			if tt.wantNoteAbsent && got.Note != "" {
				t.Errorf("note = %q, want none when brw's own engine ran", got.Note)
			}
		})
	}
}

func TestSummarizeAuditReportsAFailedRun(t *testing.T) {
	if _, err := SummarizeAudit(RawAudit{OK: false, Error: "axe crashed"}, AuditOptions{}, time.Unix(0, 0)); err == nil ||
		!strings.Contains(err.Error(), "axe crashed") {
		t.Fatalf("error = %v, want the page's own failure carried out", err)
	}
	if _, err := SummarizeAudit(RawAudit{OK: false}, AuditOptions{}, time.Unix(0, 0)); err == nil {
		t.Fatal("a failed run with no message was reported as success")
	}
}

func collectRuleIDs(document json.RawMessage) []string {
	var decoded struct {
		Violations []struct {
			ID string `json:"id"`
		} `json:"violations"`
	}
	_ = json.Unmarshal(document, &decoded)
	ids := make([]string, 0, len(decoded.Violations))
	for _, rule := range decoded.Violations {
		ids = append(ids, rule.ID)
	}
	return ids
}

func TestAuditOptionsNormalize(t *testing.T) {
	tests := []struct {
		name         string
		in           AuditOptions
		wantMaxRules int
		wantMaxRefs  int
		wantTags     []string
	}{
		{name: "defaults", in: AuditOptions{}, wantMaxRules: DefaultAuditMaxRules, wantMaxRefs: DefaultAuditMaxRefs},
		{name: "caps", in: AuditOptions{MaxRules: 9999, MaxRefs: 9999}, wantMaxRules: MaxAuditMaxRules, wantMaxRefs: MaxAuditMaxRefs},
		{name: "blank tags are dropped", in: AuditOptions{Tags: []string{" wcag2aa ", "", "  "}}, wantMaxRules: DefaultAuditMaxRules, wantMaxRefs: DefaultAuditMaxRefs, wantTags: []string{"wcag2aa"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in.Normalize()
			if got.MaxRules != tt.wantMaxRules || got.MaxRefs != tt.wantMaxRefs {
				t.Fatalf("max_rules/max_refs = %d/%d, want %d/%d", got.MaxRules, got.MaxRefs, tt.wantMaxRules, tt.wantMaxRefs)
			}
			if strings.Join(got.Tags, ",") != strings.Join(tt.wantTags, ",") {
				t.Fatalf("tags = %v, want %v", got.Tags, tt.wantTags)
			}
		})
	}
}

// The selector has to reach the page, and an empty one must not scope the audit
// to nothing.
func TestBuildAuditExpressionCarriesSelector(t *testing.T) {
	withSelector := BuildAuditExpression(AuditOptions{Selector: " #main "})
	if !strings.Contains(withSelector, `"selector":"#main"`) {
		t.Fatalf("selector was not trimmed into the expression: %s", withSelector)
	}
	without := BuildAuditExpression(AuditOptions{})
	if strings.Contains(without, `"selector":"`) && !strings.Contains(without, `"selector":""`) {
		t.Fatalf("an empty selector produced a scoped run: %s", without)
	}
}
