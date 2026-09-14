package httpclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
	httpapi "github.com/Don-Works/brw/internal/http"
	"github.com/Don-Works/brw/internal/readability"
)

// strictHandlerController is the browser side of internal/http's handler, cut
// down to the methods the artifact routes reach. The embedded interface is nil,
// so any other call panics rather than quietly answering.
type strictHandlerController struct {
	browser.Controller
}

func (strictHandlerController) Read(context.Context) (readability.PageRead, error) {
	return readability.PageRead{}, nil
}

type readArtifactStub struct {
	artifact.API
	id     string
	offset int64
	max    int
}

func (s *readArtifactStub) ReadArtifact(_ context.Context, id string, offset int64, maxBytes int) (artifact.Chunk, error) {
	s.id, s.offset, s.max = id, offset, maxBytes
	return artifact.Chunk{ArtifactID: id, Offset: offset, SizeBytes: 4, Text: "safe", Encoding: "utf-8"}, nil
}

// The artifact routes decode a fixed schema with DisallowUnknownFields, so any
// field folded in from the context turns the call into a 400. This runs against
// internal/http's real handler rather than a permissive stand-in, because a
// stand-in accepts the extra field and the bug only shows against the daemon.
func TestRequestExactKeepsContextFieldsOutOfStrictBodies(t *testing.T) {
	stub := &readArtifactStub{}
	daemon := httpapi.New("", strictHandlerController{})
	daemon.SetArtifactAPI(stub)
	server := httptest.NewServer(daemon.Handler())
	t.Cleanup(server.Close)

	client, err := New(server.URL, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	const id = "art_0123456789abcdef0123456789abcdef"
	body := map[string]any{"artifact_id": id, "offset": int64(16), "max_bytes": 64}
	ctx := browser.WithTabID(context.Background(), "77")

	if _, err := client.Request(ctx, "POST", "/api/artifacts/read", nil, body); err == nil {
		t.Fatal("the generic path folded tab_id into a strict body and the daemon accepted it; the 400 it answers is the whole reason RequestExact exists")
	} else if !strings.Contains(err.Error(), "invalid artifact request") {
		t.Fatalf("generic path error = %v, want the daemon's invalid-artifact-request refusal", err)
	}

	raw, err := client.RequestExact(ctx, "POST", "/api/artifacts/read", nil, body)
	if err != nil {
		t.Fatalf("RequestExact: %v", err)
	}
	if !strings.Contains(string(raw), `"text":"safe"`) {
		t.Fatalf("response = %s, want the artifact chunk", raw)
	}
	if stub.id != id || stub.offset != 16 || stub.max != 64 {
		t.Fatalf("daemon read id=%q offset=%d max=%d, want the window the caller asked for", stub.id, stub.offset, stub.max)
	}
}

// Each artifact operation has its own response bound; an untyped request must
// get the same one its typed method applies rather than the generic 64 MiB.
func TestExactResponseBoundMatchesTheTypedMethods(t *testing.T) {
	tests := []struct {
		path string
		want int64
	}{
		{path: "/api/artifacts/read", want: maxArtifactReadResponseBytes},
		{path: "/api/artifacts/search", want: maxArtifactSearchResponseBytes},
		{path: "/api/artifacts/info", want: maxArtifactInfoResponseBytes},
		{path: "/api/artifacts/delete", want: maxArtifactDeleteResponseBytes},
		{path: "/api/artifacts/unknown", want: maxArtifactDeleteResponseBytes},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := exactResponseBound(tt.path); got != tt.want {
				t.Fatalf("bound = %d, want %d", got, tt.want)
			}
			if got := exactResponseBound(tt.path); got >= maxUpstreamResponseBytes {
				t.Fatalf("bound = %d, want it below the generic upstream bound %d", got, maxUpstreamResponseBytes)
			}
		})
	}
}

// queryRecorder answers every request with an empty envelope and keeps the
// query string it was reached with.
func queryRecorder(t *testing.T) (*Controller, *url.Values) {
	t.Helper()
	var seen url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query()
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(server.Close)
	client, err := New(server.URL, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client, &seen
}

// A strict route that takes its arguments in the query rejects a parameter it
// does not declare for the same reason it rejects a body field, so the GET half
// of RequestExact has to leave the query as the caller built it.
func TestRequestExactKeepsTheContextTabOutOfTheQuery(t *testing.T) {
	tests := []struct {
		name    string
		exact   bool
		wantTab string
	}{
		{name: "the generic path folds the context tab in", wantTab: "77"},
		{name: "the exact path sends the caller's query verbatim", exact: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, seen := queryRecorder(t)
			send := client.Request
			if tt.exact {
				send = client.RequestExact
			}
			ctx := browser.WithTabID(context.Background(), "77")
			if _, err := send(ctx, "GET", "/api/artifacts/info", url.Values{"artifact_id": {"art_1"}}, nil); err != nil {
				t.Fatalf("send: %v", err)
			}
			if got := seen.Get("tab_id"); got != tt.wantTab {
				t.Fatalf("tab_id = %q, want %q", got, tt.wantTab)
			}
			if got := seen.Get("artifact_id"); got != "art_1" {
				t.Fatalf("artifact_id = %q, want the caller's own parameter", got)
			}
		})
	}
}

// The GET half of RequestExact applies the route's own response bound, the same
// as the POST half; an unrecognised strict route gets the smallest of them.
func TestRequestExactGETHoldsTheResponseToTheRouteBound(t *testing.T) {
	payload := `{"padding":"` + strings.Repeat("x", int(maxArtifactDeleteResponseBytes)) + `"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(server.Close)
	client, err := New(server.URL, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.RequestExact(context.Background(), "GET", "/api/artifacts/unknown", nil, nil); err == nil {
		t.Fatal("RequestExact accepted a response past the artifact bound")
	} else if !strings.Contains(err.Error(), "artifact operation bound") {
		t.Fatalf("RequestExact error = %v, want the artifact bound refusal", err)
	}
	if _, err := client.Request(context.Background(), "GET", "/api/artifacts/unknown", nil, nil); err != nil {
		t.Fatalf("Request: %v, want the generic bound to accept the same body", err)
	}
}

// Neither entry point invents a verb the daemon does not serve.
func TestRequestExactRefusesAnUnsupportedMethod(t *testing.T) {
	client, _ := queryRecorder(t)
	if _, err := client.RequestExact(context.Background(), "DELETE", "/api/artifacts/delete", nil, nil); err == nil {
		t.Fatal("RequestExact accepted DELETE")
	} else if !strings.Contains(err.Error(), "unsupported method") {
		t.Fatalf("error = %v, want an unsupported-method refusal", err)
	}
}
