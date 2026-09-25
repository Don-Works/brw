package snapshot_test

import (
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
)

// chromiumNativeBooking mirrors Chromium 152's native getTools output on a live
// site: consequentialHint is dropped, readOnlyHint and untrustedContentHint pass
// through.
const chromiumNativeBooking = `(function(){
  var tools = [
    { name: 'book_meeting', description: 'Book a meeting', window: window, inputSchema: '{"type":"object"}',
      annotations: { readOnlyHint: false, untrustedContentHint: false } },
    { name: 'find_slots', description: 'Find slots', window: window, inputSchema: '{"type":"object"}',
      annotations: { readOnlyHint: true, untrustedContentHint: false } }
  ];
  Object.defineProperty(document, 'modelContext', { configurable: true, value: {
    getTools: function(){ return Promise.resolve(tools); },
    executeTool: function(){ return Promise.resolve('{}'); },
    registerTool: function(){}
  }});
})()`

func TestNativeWriteToolWithoutConsequentialHintIsTreatedAsConsequential(t *testing.T) {
	srv := servePage(t, `<!doctype html><html><body><h1>native</h1></body></html>`)
	ctx := openArmed(t, srv.URL, chromiumNativeBooking)
	listing := listTools(t, ctx, "")
	byName := map[string]snapshot.PageToolDescriptor{}
	for _, tool := range listing.Tools {
		byName[tool.Name] = tool
	}
	book, slots := byName["book_meeting"], byName["find_slots"]
	if !book.Consequential() || !book.Annotations["consequentialInferred"] {
		t.Fatalf("book_meeting annotations = %v, want consequential inferred from readOnlyHint:false", book.Annotations)
	}
	if slots.Consequential() {
		t.Fatalf("find_slots annotations = %v, a read-only tool must not be consequential", slots.Annotations)
	}
	digest, err := snapshot.ReadPageSurfaces(ctx, chromedpEvaluator(ctx))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range digest.Tools {
		if row.Name == "book_meeting" && !row.Consequential {
			t.Fatalf("digest row %+v, want consequential", row)
		}
	}
}
