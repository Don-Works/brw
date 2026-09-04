package extensionbridge

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

func TestCapturePDFDecodesCDPBytes(t *testing.T) {
	b := New("", time.Second, "")
	want := []byte("%PDF-1.7\nsynthetic\n%%EOF")
	cleanup := serveDownloadsStub(t, b, true, map[string]any{
		"data": base64.StdEncoding.EncodeToString(want),
	}, "")
	defer cleanup()

	got, err := b.CapturePDF(browser.WithTabID(context.Background(), "42"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("PDF bytes = %q, want %q", got, want)
	}
}

func TestDocumentIdentityUsesExtensionDocumentAndNavigationEpoch(t *testing.T) {
	b := New("", time.Second, "")
	cleanup := serveDownloadsStub(t, b, true, map[string]any{
		"document_id":     "document-a",
		"document_epoch":  7,
		"worker_instance": "worker-a",
		"origin":          "https://allowed.example.test",
		"tab_id":          42,
	}, "")
	defer cleanup()

	got, err := b.DocumentIdentity(browser.WithTabID(context.Background(), "42"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != "https://allowed.example.test" ||
		!strings.Contains(got.ID, "worker-a") || !strings.Contains(got.ID, "document-a") || !strings.HasSuffix(got.ID, "\x007") {
		t.Fatalf("document identity = %+v", got)
	}
	if !isIdempotentType("get_document_identity") {
		t.Fatal("document identity probe must be safe to retry after a transient bridge reconnect")
	}
}

func TestDocumentIdentityRejectsIncompleteExtensionReply(t *testing.T) {
	b := New("", time.Second, "")
	cleanup := serveDownloadsStub(t, b, true, map[string]any{
		"document_id": "document-a",
		"origin":      "https://private.example.test/path",
		"tab_id":      42,
	}, "")
	defer cleanup()
	_, err := b.DocumentIdentity(browser.WithTabID(context.Background(), "42"))
	if err == nil || !strings.Contains(err.Error(), "invalid main-document identity") {
		t.Fatalf("incomplete identity error = %v", err)
	}
	if strings.Contains(err.Error(), "private.example.test") {
		t.Fatalf("identity validation leaked extension payload: %v", err)
	}
}

func TestDocumentIdentityRejectsOpaqueOriginUsedOnlyForNavigation(t *testing.T) {
	b := New("", time.Second, "")
	cleanup := serveDownloadsStub(t, b, true, map[string]any{
		"document_id":     "document-data",
		"document_epoch":  1,
		"worker_instance": "worker-a",
		"origin":          "null",
		"tab_id":          42,
	}, "")
	defer cleanup()
	_, err := b.DocumentIdentity(browser.WithTabID(context.Background(), "42"))
	if err == nil || !strings.Contains(err.Error(), "invalid main-document identity") {
		t.Fatalf("opaque artifact identity error = %v", err)
	}
}
