package cli

import (
	"bytes"
	"strings"
	"testing"
)

// The CLI decodes whatever the daemon sent, not whatever the summarizer on the
// other side produced. Both fields it reads in parallel carry omitempty, so a
// shape the daemon is entitled to send must print a row rather than panic the
// shell.

func TestRenderAuditSurvivesAPartialRule(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		want     []string
		wantMiss []string
	}{
		{
			name: "refs and targets in step",
			body: `{"engine":"axe-core 4.10.2","violations":1,"violation_nodes":2,
				"rules":[{"id":"color-contrast","impact":"serious","nodes":2,
				"refs":["e4","" ],"targets":["#a","#b"],"help":"Contrast"}]}`,
			want: []string{"@e4", "#b", "color-contrast"},
		},
		{
			// refs present, targets omitted entirely: the fallback has nothing
			// to index, and indexing it anyway took the whole CLI down.
			name: "a ref that did not resolve with no target beside it",
			body: `{"engine":"axe-core 4.10.2","violations":1,"violation_nodes":1,
				"rules":[{"id":"image-alt","impact":"critical","nodes":1,"refs":[""],"help":"Alt text"}]}`,
			want: []string{"image-alt", "critical"},
		},
		{
			name: "fewer targets than refs",
			body: `{"engine":"axe-core 4.10.2","violations":1,"violation_nodes":3,
				"rules":[{"id":"label","impact":"minor","nodes":3,"refs":["e1","",""],"targets":["#a"],"help":"Label"}]}`,
			want: []string{"@e1", "label"},
		},
		{
			name:     "no failures at all",
			body:     `{"engine":"axe-core 4.10.2","violations":0,"violation_nodes":0}`,
			want:     []string{"no failures"},
			wantMiss: []string{"left in the page"},
		},
		{
			// The audit is read-shaped but not effect-free, so the shell says
			// what it left the way it says a highlight is reversible.
			name: "what the audit left in the page is printed",
			body: `{"engine":"axe-core 4.10.2","violations":0,"violation_nodes":0,
				"page_effects":"data-brw-ref attributes stamped on the failing elements; axe-core 4.10.2 is left installed"}`,
			want: []string{"left in the page", "axe-core 4.10.2 is left installed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := renderAudit(&out, &options{}, []byte(tt.body)); err != nil {
				t.Fatalf("render: %v", err)
			}
			for _, want := range tt.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output does not contain %q:\n%s", want, out.String())
				}
			}
			for _, miss := range tt.wantMiss {
				if strings.Contains(out.String(), miss) {
					t.Errorf("output should not contain %q:\n%s", miss, out.String())
				}
			}
		})
	}
}

// TestRenderVitalsDistinguishesAbsentFromZero: "nothing moved" and "nobody was
// watching" print differently, or the shell reports a perfect score for a
// browser that never observed one.
func TestRenderVitalsDistinguishesAbsentFromZero(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		want     []string
		wantMiss []string
	}{
		{
			name: "an observed zero prints as a score",
			body: `{"url":"https://example.test/","cls":0,"cls_shifts":0,"ratings":{"cls":"good"}}`,
			want: []string{"CLS", "0.0000", "good"},
		},
		{
			name: "an unobservable metric prints as absent",
			body: `{"url":"https://example.test/","cls":null,"cls_shifts":0,
				"ratings":{"cls":"unknown"},"unavailable":["layout-shift"]}`,
			want:     []string{"CLS", "—", "unknown", "not observable here", "layout-shift"},
			wantMiss: []string{"0.0000"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := renderVitals(&out, &options{}, []byte(tt.body)); err != nil {
				t.Fatalf("render: %v", err)
			}
			for _, want := range tt.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output does not contain %q:\n%s", want, out.String())
				}
			}
			for _, miss := range tt.wantMiss {
				if strings.Contains(out.String(), miss) {
					t.Errorf("output should not contain %q:\n%s", miss, out.String())
				}
			}
		})
	}
}
