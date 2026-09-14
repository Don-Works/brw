package browser

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func pdfFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	var body strings.Builder
	body.WriteString(`<!doctype html><html><head><title>PDF fixture</title></head><body>`)
	// Enough content that the rendered document needs several IO.read chunks at
	// the lowered chunk size below, so the loop and its EOF handling are exercised
	// rather than short-circuited by a one-chunk document.
	for index := range 400 {
		fmt.Fprintf(&body, "<p>Statement line %d: the quick brown fox jumps over the lazy dog.</p>", index)
	}
	body.WriteString(`</body></html>`)
	page := body.String()

	mux := http.NewServeMux()
	mux.HandleFunc("/statement", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, page)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestCapturePDFStreamAgainstRealChrome drives Page.printToPDF with
// transferMode=ReturnAsStream and the IO.read chunk loop against real headless
// Chrome, and checks the streamed document against the buffered one the
// non-streaming path produces from the same page.
func TestCapturePDFStreamAgainstRealChrome(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := pdfFixtureServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := manager.Open(ctx, srv.URL+"/statement"); err != nil {
		t.Fatalf("open fixture: %v", err)
	}

	// A small chunk here is the point: it forces many IO.read round trips over a
	// document Chrome really rendered.
	previousChunk := pdfStreamChunkBytes
	pdfStreamChunkBytes = 4096
	t.Cleanup(func() { pdfStreamChunkBytes = previousChunk })

	stream, err := manager.CapturePDFStream(ctx)
	if err != nil {
		t.Fatalf("capture PDF stream: %v", err)
	}
	streamed, err := io.ReadAll(stream)
	if closeErr := stream.Close(); closeErr != nil {
		t.Fatalf("close PDF stream: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("read PDF stream: %v", err)
	}

	if !bytes.HasPrefix(streamed, []byte("%PDF-")) {
		t.Fatalf("streamed capture is not a PDF: %q", streamed[:min(16, len(streamed))])
	}
	if !bytes.Contains(streamed, []byte("%%EOF")) {
		t.Fatal("streamed capture has no PDF trailer, so the stream ended early")
	}
	if int64(len(streamed)) <= pdfStreamChunkBytes {
		t.Fatalf("streamed %d bytes at a %d-byte chunk size; the fixture no longer exercises the chunk loop",
			len(streamed), pdfStreamChunkBytes)
	}

	buffered, err := manager.CapturePDF(ctx)
	if err != nil {
		t.Fatalf("capture buffered PDF: %v", err)
	}
	if !bytes.HasPrefix(buffered, []byte("%PDF-")) {
		t.Fatal("buffered capture is not a PDF")
	}
	// Chrome embeds a creation date, so the bytes differ run to run; the size is
	// what has to agree between the two transfer modes.
	ratio := float64(len(streamed)) / float64(len(buffered))
	if ratio < 0.9 || ratio > 1.1 {
		t.Fatalf("streamed %d bytes vs buffered %d bytes; the stream did not deliver the same document",
			len(streamed), len(buffered))
	}

	// A closed stream must stay closed rather than reading a released handle.
	if _, err := stream.Read(make([]byte, 8)); err == nil {
		t.Fatal("read succeeded after the stream was closed")
	}
	t.Logf("streamed=%d bytes buffered=%d bytes chunk=%d bytes", len(streamed), len(buffered), pdfStreamChunkBytes)
}
