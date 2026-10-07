package artifact

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/devtools"
)

type proxyAPI struct{ API }

func auditResult() devtools.AuditResult {
	return devtools.AuditResult{
		URL:            "https://example.com/page",
		Title:          "Page",
		Engine:         "axe-core 4.10.2",
		Violations:     1,
		ViolationNodes: 2,
		Report:         json.RawMessage(`{"violations":[{"id":"color-contrast","nodes":[{"brw_ref":"e1"},{"brw_ref":"e2"}]}]}`),
	}
}

func TestAttachAuditReport(t *testing.T) {
	store := newTestStore(t, 1<<20, 4<<20)
	service, err := NewService(store, serviceFakeBrowser{})
	if err != nil {
		t.Fatalf("artifact service: %v", err)
	}

	tests := []struct {
		name        string
		api         API
		in          devtools.AuditResult
		ttl         time.Duration
		wantStored  bool
		wantNote    string
		wantSummary bool

		wantExpiryWithin time.Duration
	}{
		{name: "a local store keeps the report and returns a handle", api: service, in: auditResult(), wantStored: true},
		{

			name:             "a caller's ttl shortens the store retention",
			api:              service,
			in:               auditResult(),
			ttl:              90 * time.Second,
			wantStored:       true,
			wantExpiryWithin: 5 * time.Minute,
		},
		{

			name:             "a ttl past the store retention takes the store default",
			api:              service,
			in:               auditResult(),
			ttl:              30 * 24 * time.Hour,
			wantStored:       true,
			wantExpiryWithin: 2 * time.Hour,
		},
		{
			name:     "no store at all says the report is gone",
			api:      nil,
			in:       auditResult(),
			wantNote: "no artifact store",
		},
		{
			name:     "an upstream proxy cannot store here either",
			api:      proxyAPI{},
			in:       auditResult(),
			wantNote: "no artifact store",
		},
		{
			name: "a handle the browser host already produced is left alone",
			api:  nil,
			in: func() devtools.AuditResult {
				result := auditResult()
				result.Artifact = &devtools.ArtifactRef{ID: "art_" + strings.Repeat("a", 32)}
				return result
			}(),
			wantStored: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AttachAuditReport(context.Background(), tt.api, tt.in, tt.ttl)
			if len(got.Report) != 0 {
				t.Fatal("the full report survived into the answer; it must never travel in a summary")
			}
			if (got.Artifact != nil) != tt.wantStored {
				t.Fatalf("artifact = %+v, want stored = %v", got.Artifact, tt.wantStored)
			}
			if tt.wantExpiryWithin > 0 {
				if got.Artifact.ExpiresAt.IsZero() {
					t.Fatal("the handle carries no expiry, so a caller cannot tell how long the report is kept")
				}
				if left := time.Until(got.Artifact.ExpiresAt); left > tt.wantExpiryWithin {
					t.Fatalf("report expires in %v, want at most %v for ttl %v", left, tt.wantExpiryWithin, tt.ttl)
				}
			}
			if tt.wantNote != "" && !strings.Contains(got.Note, tt.wantNote) {
				t.Fatalf("note = %q, want it to mention %q", got.Note, tt.wantNote)
			}
			if tt.wantNote == "" && got.Note != "" {
				t.Fatalf("note = %q, want none", got.Note)
			}

			if got.Violations != tt.in.Violations || got.ViolationNodes != tt.in.ViolationNodes {
				t.Fatalf("summary = %d/%d, want %d/%d", got.Violations, got.ViolationNodes, tt.in.Violations, tt.in.ViolationNodes)
			}
		})
	}

	stored := AttachAuditReport(context.Background(), service, auditResult(), 0)
	chunk, err := service.ReadArtifact(context.Background(), stored.Artifact.ID, 0, MaxReadBytes)
	if err != nil {
		t.Fatalf("read the stored report: %v", err)
	}
	if !strings.Contains(chunk.Text, "color-contrast") {
		t.Fatalf("stored report = %q, want the audit document", chunk.Text)
	}
	if stored.Artifact.MIMEType != "application/json" {
		t.Errorf("mime type = %q, want application/json so the report is searchable", stored.Artifact.MIMEType)
	}
}

func TestPutReportRefusesWhatItShouldNotStore(t *testing.T) {
	service, err := NewService(newTestStore(t, 1<<20, 4<<20), serviceFakeBrowser{})
	if err != nil {
		t.Fatalf("artifact service: %v", err)
	}

	tests := []struct {
		name    string
		kind    string
		data    []byte
		wantErr string
	}{
		{name: "an unknown kind", kind: "text", data: []byte(`{}`), wantErr: "unsupported report kind"},
		{name: "no document", kind: KindAccessibilityReport, wantErr: "report is empty"},
		{name: "a runaway document", kind: KindAccessibilityReport, data: make([]byte, maxReportBytes+1), wantErr: "too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := service.PutReport(context.Background(), ReportOptions{Kind: tt.kind}, tt.data)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want one mentioning %q", err, tt.wantErr)
			}
		})
	}

	var absent *Service
	if _, err := absent.PutReport(context.Background(), ReportOptions{Kind: KindAccessibilityReport}, []byte(`{}`)); err == nil {
		t.Fatal("a report was accepted with no service behind it")
	}
}

func TestPerformanceReportsPersistThroughSharedStore(t *testing.T) {
	for _, kind := range []string{KindPerformanceTrace, KindCPUProfile} {
		t.Run(kind, func(t *testing.T) {
			service, err := NewService(newTestStore(t, 1<<20, 4<<20), serviceFakeBrowser{})
			if err != nil {
				t.Fatal(err)
			}
			meta, err := service.PutReport(context.Background(), ReportOptions{Kind: kind}, []byte(`{"capture":"profile"}`))
			if err != nil {
				t.Fatal(err)
			}
			data, _, _, err := service.store.Read(meta.ID, 0, 1024)
			if err != nil || string(data) != `{"capture":"profile"}` {
				t.Fatalf("stored report %q %v", data, err)
			}
		})
	}
}
