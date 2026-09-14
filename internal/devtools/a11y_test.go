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
			name: "a rule with no impact of its own takes the worst its nodes carry",
			document: axeDocument(
				`{"id":"mixed","impact":"","help":"h","helpUrl":"","tags":[],"nodes":[{"target":["#a"],"impact":"minor","brw_ref":"e1"},{"target":["#b"],"impact":"critical","brw_ref":"e2"}]}`),
			wantOrder:  []string{"mixed"},
			wantNodes:  2,
			wantImpact: map[string]int{"critical": 2},
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

func TestSummarizeAuditReportsAPageEngineAndAFailedRun(t *testing.T) {
	borrowed, err := SummarizeAudit(RawAudit{OK: true, Ours: false, Engine: "axe-core 3.5.5", Report: axeDocument("")}, AuditOptions{}, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if !strings.Contains(borrowed.Note, "page's own axe-core") {
		t.Errorf("note = %q, want it to say whose engine ran", borrowed.Note)
	}

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
