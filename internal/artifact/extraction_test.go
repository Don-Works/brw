package artifact

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/snapshot"
)

type extractionBrowser struct {
	serviceFakeBrowser
	structured snapshot.StructuredData
	identities int
	driftAt    int
}

func (b *extractionBrowser) DocumentIdentity(context.Context) (browser.DocumentIdentity, error) {
	b.identities++
	id := "document-one"
	if b.driftAt > 0 && b.identities >= b.driftAt {
		id = "document-two"
	}
	return browser.DocumentIdentity{ID: id, Origin: "https://extract.example"}, nil
}
func (b *extractionBrowser) ReadData(context.Context) (snapshot.StructuredData, error) {
	return b.structured, nil
}

func extractionFixture(t *testing.T) (*Service, *extractionBrowser, *Store) {
	t.Helper()
	zero, next := 0, 15
	b := &extractionBrowser{serviceFakeBrowser: serviceFakeBrowser{read: readability.PageRead{URL: "https://extract.example/report?private=value", TablesComplete: true, SectionsAnchored: true, Main: "Summary\nReady\n\nNext\nOther", Headings: []readability.Heading{{Text: "Summary", Level: 1, Offset: &zero}, {Text: "Next", Level: 1, Offset: &next}}, Tables: []readability.Table{{Caption: "Quarterly", Headers: []string{"Month", "Count"}, Rows: [][]string{{"Jan", "10"}}}}}}, structured: snapshot.StructuredData{URL: "https://extract.example/product", Source: "json_ld", Price: "10", Currency: "GBP", Raw: map[string]string{"password": "must-not-appear"}}}
	store := newTestStore(t, 1<<20, 2<<20)
	service, err := NewService(store, b)
	if err != nil {
		t.Fatal(err)
	}
	return service, b, store
}

func sectionExtraction() CaptureOptions {
	return CaptureOptions{Kind: "extraction_json", Name: "summary", Extract: &ExtractionSpec{Source: "section", Section: "Summary", MaxChars: 100, MaxBytes: 4096}}
}
func tableExtraction() CaptureOptions {
	return CaptureOptions{Kind: "extraction_json", Name: "quarterly", Extract: &ExtractionSpec{Source: "table", Caption: "Quarterly", MaxRows: 10, MaxColumns: 5, MaxCellChars: 50, MaxBytes: 4096}}
}

func TestExtractionStoresNamedBoundedOutputsWithProvenance(t *testing.T) {
	for _, opts := range []CaptureOptions{sectionExtraction(), tableExtraction(), {Kind: "extraction_json", Name: "product", Extract: &ExtractionSpec{Source: "structured", ExpectedSource: "json_ld", Fields: []string{"price", "currency"}, MaxBytes: 4096}}} {
		t.Run(opts.Extract.Source, func(t *testing.T) {
			service, _, store := extractionFixture(t)
			meta, err := service.CaptureArtifact(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if meta.Kind != "extraction_json" || meta.MIMEType != "application/json" {
				t.Fatalf("unexpected metadata: %+v", meta)
			}
			data, _, more, err := store.Read(meta.ID, 0, 4096)
			if err != nil || more {
				t.Fatalf("read err=%v more=%v", err, more)
			}
			var output ExtractionResult
			if err := json.Unmarshal(data, &output); err != nil {
				t.Fatal(err)
			}
			if output.Name != opts.Name || output.Provenance.Origin != "https://extract.example" || output.Provenance.DocumentID != "document-one" || output.Provenance.CapturedAt.IsZero() {
				t.Fatalf("output=%+v", output)
			}
			if strings.Contains(string(data), "private=value") || strings.Contains(string(data), "must-not-appear") || strings.Contains(string(data), `"raw"`) {
				t.Fatalf("unexpected source material in output: %s", data)
			}
		})
	}
}

func TestExtractionFailsClosedForAmbiguityMissingStateAndBudgets(t *testing.T) {
	tests := []struct {
		name   string
		opts   CaptureOptions
		mutate func(*extractionBrowser)
	}{
		{"missing section", sectionExtraction(), func(b *extractionBrowser) { b.read.Headings = nil }},
		{"duplicate section", sectionExtraction(), func(b *extractionBrowser) { b.read.Headings = append(b.read.Headings, b.read.Headings[0]) }},
		{"case distinct section", sectionExtraction(), func(b *extractionBrowser) {
			z := 0
			b.read.Headings = append([]readability.Heading{{Text: "summary", Level: 1, Offset: &z}}, b.read.Headings...)
		}},
		{"approximate section offset", sectionExtraction(), func(b *extractionBrowser) { b.read.SectionsAnchored = false }},
		{"unaddressable section", sectionExtraction(), func(b *extractionBrowser) { b.read.Headings[0].Offset = nil }},
		{"section overflow", sectionExtraction(), nil},
		{"bytes overflow", sectionExtraction(), nil},
		{"missing table", tableExtraction(), func(b *extractionBrowser) { b.read.Tables = nil }},
		{"duplicate table", tableExtraction(), func(b *extractionBrowser) { b.read.Tables = append(b.read.Tables, b.read.Tables[0]) }},
		{"old table source", tableExtraction(), func(b *extractionBrowser) { b.read.TablesComplete = false }},
		{"tables clipped", tableExtraction(), func(b *extractionBrowser) { b.read.TablesTruncated = true }},
		{"raw rows clipped before empty filter", tableExtraction(), func(b *extractionBrowser) { b.read.Tables[0].Truncated = true }},
		{"row overflow", tableExtraction(), func(b *extractionBrowser) { b.read.Tables[0].Rows = make([][]string, 11) }},
		{"cell overflow", tableExtraction(), func(b *extractionBrowser) { b.read.Tables[0].Rows[0][0] = strings.Repeat("x", 51) }},
		{"origin drift", sectionExtraction(), func(b *extractionBrowser) { b.read.URL = "https://other.example/report" }},
		{"windowed read", sectionExtraction(), func(b *extractionBrowser) { b.read.MainTruncated = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, b, store := extractionFixture(t)
			if test.mutate != nil {
				test.mutate(b)
			}
			if test.name == "section overflow" {
				test.opts.Extract.MaxChars = 1
			}
			if test.name == "bytes overflow" {
				test.opts.Extract.MaxBytes = 1
			}
			_, err := service.CaptureArtifact(context.Background(), test.opts)
			if test.name == "case distinct section" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected fail-closed error")
			}
			entries, _ := os.ReadDir(store.root)
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".json") {
					t.Fatalf("failed capture persisted metadata: %s", entry.Name())
				}
			}
		})
	}
}

func TestExtractionRollsBackAfterPersistenceDocumentDrift(t *testing.T) {
	for _, recipe := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "recipe"}[recipe], func(t *testing.T) {
			service, b, store := extractionFixture(t)
			b.driftAt = 3
			ctx := context.Background()
			if recipe {
				ctx = browser.WithAllowedOrigins(ctx, []string{"https://extract.example"})
			}
			if _, err := service.CaptureArtifact(ctx, sectionExtraction()); err == nil {
				t.Fatal("expected document drift failure")
			}
			entries, _ := os.ReadDir(store.root)
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".blob") {
					t.Fatalf("drift left artifact: %s", entry.Name())
				}
			}
		})
	}
}

func TestExtractionRejectsRawStructuredMissingFieldsAndSensitiveContexts(t *testing.T) {
	for _, spec := range []ExtractionSpec{{Source: "structured", ExpectedSource: "json_ld", Fields: []string{"raw"}, MaxBytes: 4096}, {Source: "structured", ExpectedSource: "meta", Fields: []string{"price"}, MaxBytes: 4096}, {Source: "structured", ExpectedSource: "json_ld", Fields: []string{"brand"}, MaxBytes: 4096}} {
		service, _, _ := extractionFixture(t)
		if _, err := service.CaptureArtifact(context.Background(), CaptureOptions{Kind: "extraction_json", Name: "product", Extract: &spec}); err == nil {
			t.Fatal("expected invalid structured extraction failure")
		}
	}
	service, _, _ := extractionFixture(t)
	if _, err := service.CaptureArtifact(browser.WithSensitiveAction(context.Background()), sectionExtraction()); err == nil {
		t.Fatal("expected sensitive extraction refusal")
	}
}
