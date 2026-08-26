package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/readability"
)

type boundsController struct {
	fakeController
	read    readability.PageRead
	console []browser.ConsoleMessage
}

func (c *boundsController) Read(context.Context) (readability.PageRead, error) {
	return c.read, nil
}

func (c *boundsController) ConsoleMessages(context.Context) ([]browser.ConsoleMessage, error) {
	return c.console, nil
}

func getJSON(t *testing.T, server *Server, path string, dst any) int {
	t.Helper()
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code == http.StatusOK && dst != nil {
		if err := json.NewDecoder(rec.Body).Decode(dst); err != nil {
			t.Fatalf("decode %s: %v (%s)", path, err, rec.Body.String())
		}
	}
	return rec.Code
}

func longRead() readability.PageRead {
	return readability.PageRead{
		URL:   "https://example.com",
		Title: "Long",
		Main:  strings.Repeat("q", 60000),
		Links: []readability.Link{{Text: "a", Href: "/a"}, {Text: "b", Href: "/b"}},
	}
}

func TestHTTPReadBoundsProseByDefault(t *testing.T) {
	server := New("", &boundsController{read: longRead()})

	var got readability.PageRead
	if code := getJSON(t, server, "/api/page/read", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(got.Main) != readability.DefaultReadMaxChars {
		t.Fatalf("prose = %d chars, want the %d-char default bound", len(got.Main), readability.DefaultReadMaxChars)
	}
	if !got.MainTruncated || got.NextOffset != readability.DefaultReadMaxChars {
		t.Fatalf("paging metadata missing: truncated=%v next_offset=%d", got.MainTruncated, got.NextOffset)
	}
}

// The --upstream-http proxy asks for max_chars=-1 so the MCP layer can apply
// the caller's own window. If the endpoint refused or ignored the sentinel, an
// explicitly unbounded read would come back silently capped in proxy mode.
func TestHTTPReadHonoursUnboundedSentinel(t *testing.T) {
	server := New("", &boundsController{read: longRead()})

	var got readability.PageRead
	code := getJSON(t, server, "/api/page/read?max_chars=-1&max_links=-1&max_headings=-1", &got)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want the -1 sentinel accepted", code)
	}
	if len(got.Main) != 60000 {
		t.Fatalf("prose = %d chars, want the whole 60000", len(got.Main))
	}
	if got.MainTruncated {
		t.Fatal("unbounded read reported truncation")
	}
}

func TestHTTPReadIncludeAndBadInput(t *testing.T) {
	server := New("", &boundsController{read: longRead()})

	var got readability.PageRead
	if code := getJSON(t, server, "/api/page/read?include=links", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got.Main != "" || len(got.Links) != 2 {
		t.Fatalf("include=links returned main=%d chars links=%d", len(got.Main), len(got.Links))
	}

	if code := getJSON(t, server, "/api/page/read?include=linkz", nil); code != http.StatusBadRequest {
		t.Fatalf("unknown section status = %d, want 400", code)
	}
	if code := getJSON(t, server, "/api/page/read?max_chars=-9", nil); code != http.StatusBadRequest {
		t.Fatalf("max_chars=-9 status = %d, want 400", code)
	}
}

// The console endpoint must keep returning a bare array: the proxy decodes it
// straight into []ConsoleMessage, and an envelope would empty the console in
// --upstream-http mode.
func TestHTTPConsoleKeepsArrayContractWhileFiltering(t *testing.T) {
	ctrl := &boundsController{console: []browser.ConsoleMessage{
		{Level: "log", Text: "mounted"},
		{Level: "error", Text: "TypeError: boom"},
		{Level: "warn", Text: "deprecated"},
	}}
	server := New("", ctrl)

	var all []browser.ConsoleMessage
	if code := getJSON(t, server, "/api/page/console", &all); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered console returned %d messages, want 3", len(all))
	}

	var errorsOnly []browser.ConsoleMessage
	getJSON(t, server, "/api/page/console?only_errors=true", &errorsOnly)
	if len(errorsOnly) != 1 || errorsOnly[0].Text != "TypeError: boom" {
		t.Fatalf("only_errors returned %+v", errorsOnly)
	}

	var matched []browser.ConsoleMessage
	getJSON(t, server, "/api/page/console?pattern=depre", &matched)
	if len(matched) != 1 || matched[0].Text != "deprecated" {
		t.Fatalf("pattern returned %+v", matched)
	}

	var limited []browser.ConsoleMessage
	getJSON(t, server, "/api/page/console?limit=1", &limited)
	if len(limited) != 1 || limited[0].Text != "deprecated" {
		t.Fatalf("limit=1 returned %+v, want the most recent message", limited)
	}

	if code := getJSON(t, server, "/api/page/console?pattern=%5Bunclosed", nil); code != http.StatusBadRequest {
		t.Fatalf("invalid pattern status = %d, want 400", code)
	}
}
