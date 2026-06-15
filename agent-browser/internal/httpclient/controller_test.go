package httpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/revitt/agent-browser/internal/browser"
	"github.com/revitt/agent-browser/internal/snapshot"
)

func TestNew_EmptyURL(t *testing.T) {
	_, err := New("", 0)
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
}

func TestNew_InvalidURL(t *testing.T) {
	_, err := New("://bad", 0)
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestNew_AddsHTTPScheme(t *testing.T) {
	c, err := New("localhost:1234", 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.baseURL != "http://localhost:1234" {
		t.Fatalf("expected http:// prefix, got %q", c.baseURL)
	}
}

func TestNew_TrimsTrailingSlash(t *testing.T) {
	c, err := New("http://localhost:1234/", 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.baseURL != "http://localhost:1234" {
		t.Fatalf("expected trimmed URL, got %q", c.baseURL)
	}
}

func TestNew_DefaultTimeout(t *testing.T) {
	c, err := New("http://localhost:1234", 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.client.Timeout != 20*time.Second {
		t.Fatalf("expected 20s timeout, got %v", c.client.Timeout)
	}
}

func TestOpen_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/browser/open" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		if req["url"] != "https://example.com" {
			t.Errorf("unexpected url: %s", req["url"])
		}
		json.NewEncoder(w).Encode(browser.OpenResult{
			Tab: browser.Tab{ID: "tab1", URL: "https://example.com", Title: "Example"},
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.Open(context.Background(), "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	if result.Tab.ID != "tab1" {
		t.Fatalf("expected tab1, got %q", result.Tab.ID)
	}
}

func TestListTabs_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/browser/tabs" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode([]browser.Tab{
			{ID: "tab1", URL: "https://a.com", Title: "A"},
			{ID: "tab2", URL: "https://b.com", Title: "B"},
		})
	}))
	defer srv.Close()

	c, _ := New(srv.URL, 5*time.Second)
	tabs, err := c.ListTabs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tabs) != 2 {
		t.Fatalf("expected 2 tabs, got %d", len(tabs))
	}
}

func TestClick_ForwardsTabID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/page/click" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["ref"] != "e17" {
			t.Errorf("unexpected ref: %v", req["ref"])
		}
		json.NewEncoder(w).Encode(browser.ActionResult{OK: true, Message: "clicked e17"})
	}))
	defer srv.Close()

	c, _ := New(srv.URL, 5*time.Second)
	result, err := c.Click(context.WithValue(context.Background(), struct{}{}, "test"), "e17")
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatal("expected OK")
	}
}

func TestSnapshot_ForwardsOptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/page/snapshot" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("mode") != "all" {
			t.Errorf("expected mode=all, got %q", r.URL.Query().Get("mode"))
		}
		json.NewEncoder(w).Encode(snapshot.PageSnapshot{
			URL:   "https://example.com",
			Title: "Example",
		})
	}))
	defer srv.Close()

	c, _ := New(srv.URL, 5*time.Second)
	_, err := c.Snapshot(context.Background(), snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestServerDown_ReturnsError(t *testing.T) {
	c, _ := New("http://127.0.0.1:1", 100*time.Millisecond)
	_, err := c.Open(context.Background(), "https://example.com")
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestServerError_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "something broke"})
	}))
	defer srv.Close()

	c, _ := New(srv.URL, 5*time.Second)
	_, err := c.Open(context.Background(), "https://example.com")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestScreenshot_ReturnsBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("base64") != "1" {
			t.Errorf("expected base64=1 query param")
		}
		json.NewEncoder(w).Encode(browser.Screenshot{
			MIMEType: "image/png",
			Base64:   "ZmFrZS1wbmctZGF0YQ==",
		})
	}))
	defer srv.Close()

	c, _ := New(srv.URL, 5*time.Second)
	shot, err := c.Screenshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if shot.MIMEType != "image/png" {
		t.Fatalf("unexpected mime type: %q", shot.MIMEType)
	}
	if shot.Base64 != "ZmFrZS1wbmctZGF0YQ==" {
		t.Fatalf("unexpected base64: %q", shot.Base64)
	}
	if string(shot.Data) != "fake-png-data" {
		t.Fatalf("unexpected decoded data: %q", shot.Data)
	}
}
