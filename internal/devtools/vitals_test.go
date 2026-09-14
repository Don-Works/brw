package devtools

import (
	"strings"
	"testing"
)

// TestBuildVitalsExpressionPassesOnlyWhatTheScriptReads: the argument is
// rendered into an expression that runs in the page, so anything marshalled
// into it is handed to the document. tab_id is daemon-side routing — the script
// never reads it — and the two sibling builders already pass only what their
// scripts use.
func TestBuildVitalsExpressionPassesOnlyWhatTheScriptReads(t *testing.T) {
	tests := []struct {
		name string
		opts VitalsOptions
		want string
	}{
		{name: "an explicit settle window", opts: VitalsOptions{SettleMS: 400, TabID: "tab-7"}, want: `{"settle_ms":400}`},
		{name: "the default fills in", opts: VitalsOptions{TabID: "tab-7"}, want: `{"settle_ms":250}`},
		{name: "the ceiling applies before the page sees it", opts: VitalsOptions{SettleMS: 3600000}, want: `{"settle_ms":5000}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expression := BuildVitalsExpression(tt.opts)
			argument := strings.TrimPrefix(expression, VitalsScript)
			if argument != "("+tt.want+")" {
				t.Fatalf("argument = %s, want (%s)", argument, tt.want)
			}
			if strings.Contains(argument, "tab_id") || strings.Contains(argument, tt.opts.TabID) && tt.opts.TabID != "" {
				t.Fatalf("argument = %s, want the daemon's tab id kept out of the page", argument)
			}
		})
	}
}

// TestAuditOptionsNormalizeTTL covers the retention knob the stored report
// needs: it holds the raw HTML of every failing element.
func TestAuditOptionsNormalizeTTL(t *testing.T) {
	tests := []struct {
		name        string
		in          AuditOptions
		wantSeconds int
		wantTTL     string
	}{
		{name: "unset means the store default", in: AuditOptions{}, wantSeconds: 0, wantTTL: "0s"},
		{name: "a negative request means the store default", in: AuditOptions{TTLSeconds: -5}, wantSeconds: 0, wantTTL: "0s"},
		{name: "a request is carried through", in: AuditOptions{TTLSeconds: 90}, wantSeconds: 90, wantTTL: "1m30s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in.Normalize()
			if got.TTLSeconds != tt.wantSeconds {
				t.Fatalf("ttl_seconds = %d, want %d", got.TTLSeconds, tt.wantSeconds)
			}
			if got.ReportTTL().String() != tt.wantTTL {
				t.Fatalf("report ttl = %s, want %s", got.ReportTTL(), tt.wantTTL)
			}
		})
	}
}
