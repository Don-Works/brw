package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/recipe"
)

func TestArtifactAndRecipeCapabilitiesStayOnUpstreamBrowserHost(t *testing.T) {
	var captureTab, runTab string
	var captureTTL float64
	artifactID := "art_0123456789abcdef0123456789abcdef"
	privateQuery := "PRIVATE_QUERY_SENTINEL"
	artifactRequests := make(map[string]map[string]any)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/artifacts/capture", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		captureTab, _ = body["tab_id"].(string)
		captureTTL, _ = body["ttl_seconds"].(float64)
		json.NewEncoder(w).Encode(artifact.Meta{ID: artifactID, Kind: "screenshot", SizeBytes: 12345})
	})
	for _, path := range []string{"/api/artifacts/info", "/api/artifacts/read", "/api/artifacts/search", "/api/artifacts/delete"} {
		path := path
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.RawQuery != "" || strings.Contains(r.RequestURI, artifactID) || strings.Contains(r.RequestURI, privateQuery) {
				t.Errorf("artifact request leaked private values in URL: %s", r.RequestURI)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode %s: %v", path, err)
				return
			}
			if _, ok := body["tab_id"]; ok {
				t.Errorf("artifact request %s unexpectedly inherited tab_id", path)
			}
			if _, ok := body["snapshot"]; ok {
				t.Errorf("artifact request %s unexpectedly inherited snapshot", path)
			}
			artifactRequests[path] = body
			switch path {
			case "/api/artifacts/info":
				_ = json.NewEncoder(w).Encode(artifact.Meta{ID: artifactID, Kind: "screenshot"})
			case "/api/artifacts/read":
				_ = json.NewEncoder(w).Encode(artifact.Chunk{ArtifactID: artifactID, Text: "elevenbytes", Encoding: "utf-8"})
			case "/api/artifacts/search":
				_ = json.NewEncoder(w).Encode([]artifact.TextHit{{Line: 3, Excerpt: "match"}})
			case "/api/artifacts/delete":
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			}
		})
	}
	mux.HandleFunc("POST /api/recipes/run", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		runTab, _ = body["tab_id"].(string)
		json.NewEncoder(w).Encode(recipe.RunResult{RecipeID: "billing.invoice.download", Status: "done"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client, err := New(srv.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := browser.WithTabID(context.Background(), "tab-42")
	meta, err := client.CaptureArtifact(ctx, artifact.CaptureOptions{Kind: "screenshot", TTL: time.Minute + 500*time.Millisecond})
	if err != nil || meta.ID == "" || captureTab != "tab-42" || captureTTL != 61 {
		t.Fatalf("capture meta=%+v tab=%q ttl=%v err=%v", meta, captureTab, captureTTL, err)
	}
	if _, err := client.ArtifactInfo(ctx, meta.ID); err != nil {
		t.Fatalf("artifact info: %v", err)
	}
	chunk, err := client.ReadArtifact(ctx, meta.ID, 7, 11)
	if err != nil || chunk.Text != "elevenbytes" {
		t.Fatalf("chunk=%+v err=%v", chunk, err)
	}
	hits, err := client.SearchArtifact(ctx, meta.ID, privateQuery, 3)
	if err != nil || len(hits) != 1 || hits[0].Line != 3 {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	if err := client.DeleteArtifact(ctx, meta.ID); err != nil {
		t.Fatalf("artifact delete: %v", err)
	}
	if got := artifactRequests["/api/artifacts/info"]["artifact_id"]; got != artifactID {
		t.Fatalf("info artifact_id=%v", got)
	}
	if body := artifactRequests["/api/artifacts/read"]; body["artifact_id"] != artifactID || body["offset"] != float64(7) || body["max_bytes"] != float64(11) {
		t.Fatalf("read body=%v", body)
	}
	if body := artifactRequests["/api/artifacts/search"]; body["artifact_id"] != artifactID || body["query"] != privateQuery || body["limit"] != float64(3) {
		t.Fatalf("search body=%v", body)
	}
	if got := artifactRequests["/api/artifacts/delete"]["artifact_id"]; got != artifactID {
		t.Fatalf("delete artifact_id=%v", got)
	}
	result, err := client.RunRecipe(ctx, recipe.RunRequest{ID: "billing.invoice.download", Version: "1", Digest: strings.Repeat("c", 64)})
	if err != nil || result.Status != "done" || runTab != "tab-42" {
		t.Fatalf("run=%+v tab=%q err=%v", result, runTab, err)
	}
}

func TestArtifactClientBoundsResponsesAndRedactsUpstreamErrors(t *testing.T) {
	t.Run("reflected upstream error", func(t *testing.T) {
		const privateQuery = "PRIVATE_QUERY_SENTINEL"
		const artifactID = "art_0123456789abcdef0123456789abcdef"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/artifacts/search" || r.URL.RawQuery != "" {
				t.Errorf("unexpected request URL: %s", r.URL.String())
			}
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed for " + privateQuery + " in " + artifactID})
		}))
		defer srv.Close()
		client, err := New(srv.URL, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.SearchArtifact(context.Background(), artifactID, privateQuery, 1)
		if err == nil || strings.Contains(err.Error(), privateQuery) || strings.Contains(err.Error(), artifactID) {
			t.Fatalf("unsafe client error: %v", err)
		}
	})

	t.Run("operation-specific response cap", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(strings.Repeat("x", int(maxArtifactInfoResponseBytes)+1)))
		}))
		defer srv.Close()
		client, err := New(srv.URL, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.ArtifactInfo(context.Background(), "art_0123456789abcdef0123456789abcdef")
		if err == nil || err.Error() != "artifact info request failed" {
			t.Fatalf("oversized artifact response was not safely rejected: %v", err)
		}
	})
}

func TestArtifactClientPropagatesCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	client, err := New(srv.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, callErr := client.SearchArtifact(ctx, "art_0123456789abcdef0123456789abcdef", "find me", 1)
		done <- callErr
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("artifact request did not stop after cancellation")
	}
}

func TestRecipeRunOutlivesOrdinaryProxyTimeout(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/recipes/run", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(recipe.RunResult{RecipeID: "example.timer", Status: "done"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client, err := New(srv.URL, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.RunRecipe(context.Background(), recipe.RunRequest{ID: "example.timer"})
	if err != nil || result.Status != "done" {
		t.Fatalf("timed recipe was cut off by ordinary proxy timeout: result=%+v err=%v", result, err)
	}
}
