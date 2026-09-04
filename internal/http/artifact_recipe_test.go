package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/recipe"
)

type artifactAPIFake struct {
	tabID        string
	capture      artifact.CaptureOptions
	readID       string
	readOffset   int64
	readMax      int
	deletedID    string
	infoID       string
	searchID     string
	searchQuery  string
	searchLimit  int
	contextErr   error
	operationErr error
}

func (f *artifactAPIFake) CaptureArtifact(ctx context.Context, opts artifact.CaptureOptions) (artifact.Meta, error) {
	f.tabID, f.capture = browser.TabIDFromContext(ctx), opts
	return artifact.Meta{ID: "art_0123456789abcdef0123456789abcdef", Kind: opts.Kind, MIMEType: "text/plain", SizeBytes: 999}, nil
}
func (f *artifactAPIFake) ArtifactInfo(ctx context.Context, id string) (artifact.Meta, error) {
	f.infoID, f.contextErr = id, ctx.Err()
	return artifact.Meta{ID: id, Kind: "text"}, f.operationErr
}
func (f *artifactAPIFake) ReadArtifact(ctx context.Context, id string, offset int64, maxBytes int) (artifact.Chunk, error) {
	f.readID, f.readOffset, f.readMax, f.contextErr = id, offset, maxBytes, ctx.Err()
	return artifact.Chunk{ArtifactID: id, Offset: offset, SizeBytes: 4, Text: "safe", Encoding: "utf-8"}, f.operationErr
}
func (f *artifactAPIFake) SearchArtifact(ctx context.Context, id string, query string, limit int) ([]artifact.TextHit, error) {
	f.searchID, f.searchQuery, f.searchLimit, f.contextErr = id, query, limit, ctx.Err()
	return []artifact.TextHit{{Line: 9, Excerpt: "matching line"}}, f.operationErr
}
func (f *artifactAPIFake) DeleteArtifact(ctx context.Context, id string) error {
	f.deletedID, f.contextErr = id, ctx.Err()
	return f.operationErr
}

type recipeAPIFake struct {
	tabID   string
	request recipe.RunRequest
}

func (f *recipeAPIFake) SearchRecipes(context.Context, string, string, int) ([]recipe.Match, error) {
	return []recipe.Match{{ID: "billing.invoice.download", Version: "1", Description: "Download invoices", Digest: strings.Repeat("a", 64)}}, nil
}
func (f *recipeAPIFake) RunRecipe(ctx context.Context, request recipe.RunRequest) (recipe.RunResult, error) {
	f.tabID, f.request = browser.TabIDFromContext(ctx), request
	return recipe.RunResult{RecipeID: request.ID, RecipeVersion: request.Version, RecipeDigest: request.Digest, Status: "done"}, nil
}

func TestArtifactHTTPRoutesKeepPayloadOnHostAndForwardBounds(t *testing.T) {
	server := New("", &fakeController{})
	api := &artifactAPIFake{}
	server.SetArtifactAPI(api)
	artifactID := "art_0123456789abcdef0123456789abcdef"

	capture := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(capture, httptest.NewRequest(http.MethodPost, "/api/artifacts/capture", bytes.NewBufferString(`{"kind":"text","ttl_seconds":60,"tab_id":"77"}`)))
	if capture.Code != http.StatusOK || api.tabID != "77" || api.capture.Kind != "text" || api.capture.TTLSeconds != 60 {
		t.Fatalf("capture status=%d tab=%q opts=%+v body=%s", capture.Code, api.tabID, api.capture, capture.Body.String())
	}
	if capture.Header().Get("Cache-Control") != "no-store" || capture.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("sensitive JSON cache headers = %v", capture.Header())
	}
	if strings.Contains(capture.Body.String(), "payload") || strings.Contains(capture.Body.String(), "safe") {
		t.Fatalf("capture response leaked artifact bytes: %s", capture.Body.String())
	}
	badCapture := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(badCapture, httptest.NewRequest(http.MethodPost, "/api/artifacts/capture", bytes.NewBufferString(`{"kind":"text","ignored":true}`)))
	if badCapture.Code != http.StatusBadRequest {
		t.Fatalf("unknown capture field status=%d body=%s", badCapture.Code, badCapture.Body.String())
	}

	info := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(info, httptest.NewRequest(http.MethodPost, "/api/artifacts/info", bytes.NewBufferString(`{"artifact_id":"`+artifactID+`"}`)))
	if info.Code != http.StatusOK || api.infoID != artifactID {
		t.Fatalf("info status=%d id=%q body=%s", info.Code, api.infoID, info.Body.String())
	}

	read := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(read, httptest.NewRequest(http.MethodPost, "/api/artifacts/read", bytes.NewBufferString(`{"artifact_id":"`+artifactID+`","offset":123,"max_bytes":456}`)))
	if read.Code != http.StatusOK || api.readOffset != 123 || api.readMax != 456 {
		t.Fatalf("read status=%d offset=%d max=%d body=%s", read.Code, api.readOffset, api.readMax, read.Body.String())
	}

	search := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(search, httptest.NewRequest(http.MethodPost, "/api/artifacts/search", bytes.NewBufferString(`{"artifact_id":"`+artifactID+`","query":"invoice","limit":3}`)))
	if search.Code != http.StatusOK || api.searchID != artifactID || api.searchQuery != "invoice" || api.searchLimit != 3 {
		t.Fatalf("search status=%d id=%q query=%q limit=%d", search.Code, api.searchID, api.searchQuery, api.searchLimit)
	}

	del := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(del, httptest.NewRequest(http.MethodPost, "/api/artifacts/delete", bytes.NewBufferString(`{"artifact_id":"`+artifactID+`"}`)))
	if del.Code != http.StatusOK || api.deletedID != artifactID {
		t.Fatalf("delete status=%d id=%q", del.Code, api.deletedID)
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{"info": info, "read": read, "search": search, "delete": del} {
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s sensitive JSON cache headers = %v", name, rec.Header())
		}
	}
}

func TestArtifactPOSTRequestsAreStrictBoundedAndMethodRestricted(t *testing.T) {
	server := New("", &fakeController{})
	server.SetArtifactAPI(&artifactAPIFake{})

	tests := []struct {
		name string
		body string
	}{
		{name: "unknown field", body: `{"artifact_id":"art_0123456789abcdef0123456789abcdef","PRIVATE_FIELD_SENTINEL":true}`},
		{name: "trailing json", body: `{"artifact_id":"art_0123456789abcdef0123456789abcdef"} {"PRIVATE_TRAILING_SENTINEL":true}`},
		{name: "oversized", body: `{"artifact_id":"` + strings.Repeat("PRIVATE_BODY_SENTINEL", maxArtifactRequestBodyBytes) + `"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/artifacts/info", strings.NewReader(test.body)))
			if rec.Code != http.StatusBadRequest || rec.Body.String() != "{\"error\":\"invalid artifact request\"}\n" {
				t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
			}
			for _, secret := range []string{"PRIVATE_FIELD_SENTINEL", "PRIVATE_TRAILING_SENTINEL", "PRIVATE_BODY_SENTINEL"} {
				if strings.Contains(rec.Body.String(), secret) {
					t.Fatalf("error reflected private request data: %s", rec.Body.String())
				}
			}
		})
	}
	for _, invalid := range []struct {
		path string
		body string
	}{
		{path: "/api/artifacts/read", body: `{"artifact_id":"art_0123456789abcdef0123456789abcdef","offset":-1,"max_bytes":1}`},
		{path: "/api/artifacts/read", body: `{"artifact_id":"art_0123456789abcdef0123456789abcdef","offset":0,"max_bytes":1048577}`},
		{path: "/api/artifacts/search", body: `{"artifact_id":"art_0123456789abcdef0123456789abcdef","query":"x","limit":101}`},
		{path: "/api/artifacts/search", body: `{"artifact_id":"art_0123456789abcdef0123456789abcdef","query":"` + strings.Repeat("x", 257) + `","limit":1}`},
	} {
		rec := httptest.NewRecorder()
		server.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, invalid.path, strings.NewReader(invalid.body)))
		if rec.Code != http.StatusBadRequest || rec.Body.String() != "{\"error\":\"invalid artifact request\"}\n" {
			t.Fatalf("invalid bounded request path=%s status=%d body=%q", invalid.path, rec.Code, rec.Body.String())
		}
	}

	method := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(method, httptest.NewRequest(http.MethodGet, "/api/artifacts/info", nil))
	if method.Code != http.StatusMethodNotAllowed || method.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("method restriction status=%d allow=%q", method.Code, method.Header().Get("Allow"))
	}
	if method.Header().Get("Cache-Control") != "no-store" || method.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("method rejection cache headers = %v", method.Header())
	}
}

func TestArtifactPOSTErrorsAndCancellationDoNotExposePrivateValues(t *testing.T) {
	server := New("", &fakeController{})
	api := &artifactAPIFake{operationErr: errors.New("failed around PRIVATE_QUERY_SENTINEL and art_0123456789abcdef0123456789abcdef")}
	server.SetArtifactAPI(api)
	body := `{"artifact_id":"art_0123456789abcdef0123456789abcdef","query":"PRIVATE_QUERY_SENTINEL","limit":1}`
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/artifacts/search", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest || rec.Body.String() != "{\"error\":\"artifact operation failed\"}\n" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "PRIVATE_QUERY_SENTINEL") || strings.Contains(rec.Body.String(), "art_0123456789abcdef0123456789abcdef") {
		t.Fatalf("artifact error exposed private request values: %s", rec.Body.String())
	}

	api.operationErr = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/artifacts/info", strings.NewReader(`{"artifact_id":"art_0123456789abcdef0123456789abcdef"}`)).WithContext(ctx)
	server.server.Handler.ServeHTTP(cancelled, req)
	if !errors.Is(api.contextErr, context.Canceled) {
		t.Fatalf("request cancellation was not forwarded to artifact API: %v", api.contextErr)
	}
}

func TestLegacyArtifactRoutesRemainAvailableButAdvertiseDeprecation(t *testing.T) {
	server := New("", &fakeController{})
	api := &artifactAPIFake{}
	server.SetArtifactAPI(api)
	path := "/api/artifacts/art_0123456789abcdef0123456789abcdef/read?offset=7&max_bytes=11"
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK || api.readOffset != 7 || api.readMax != 11 {
		t.Fatalf("legacy read status=%d offset=%d max=%d", rec.Code, api.readOffset, api.readMax)
	}
	if rec.Header().Get("Deprecation") == "" || !strings.Contains(rec.Header().Get("Link"), "/api/artifacts/read") {
		t.Fatalf("legacy deprecation headers = %v", rec.Header())
	}
}

func TestRecipeHTTPRoutesPinTabAndNeverEchoInputs(t *testing.T) {
	server := New("", &fakeController{})
	api := &recipeAPIFake{}
	server.SetRecipeAPI(api)
	digest := strings.Repeat("b", 64)
	body := `{"id":"billing.invoice.download","version":"1","digest":"` + digest + `","inputs":{"credential":"never-echo-me"},"tab_id":"88"}`
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/recipes/run", bytes.NewBufferString(body)))
	if rec.Code != http.StatusOK || api.tabID != "88" || api.request.Inputs["credential"] != "never-echo-me" {
		t.Fatalf("status=%d tab=%q request=%+v body=%s", rec.Code, api.tabID, api.request, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "never-echo-me") {
		t.Fatalf("recipe response echoed a sensitive input: %s", rec.Body.String())
	}
	bad := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(bad, httptest.NewRequest(http.MethodPost, "/api/recipes/run", bytes.NewBufferString(`{"id":"x.y","version":"1.0.0","digest":"`+digest+`","ignored":true}`)))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("unknown recipe field status=%d body=%s", bad.Code, bad.Body.String())
	}
	var result recipe.RunResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || result.Status != "done" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
