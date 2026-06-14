package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/revitt/agent-browser/internal/browser"
	"github.com/revitt/agent-browser/internal/readability"
	"github.com/revitt/agent-browser/internal/snapshot"
)

func TestSnapshotAppliesQueryParams(t *testing.T) {
	ctrl := &fakeController{snap: sampleSnapshot()}
	server := New("", ctrl)

	req := httptest.NewRequest(http.MethodGet, "/api/page/snapshot?mode=frontier&query=email&limit=7&viewport_only=true&include_hidden=true&include_ax=true&since=42&max_bytes=2000", nil)
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got snapshot.PageSnapshot
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if ctrl.snapshotOpts.Mode != "frontier" || ctrl.snapshotOpts.Query != "email" || ctrl.snapshotOpts.Limit != 7 || !ctrl.snapshotOpts.ViewportOnly || !ctrl.snapshotOpts.IncludeHidden || !ctrl.snapshotOpts.IncludeAX || ctrl.snapshotOpts.Since != 42 {
		t.Fatalf("snapshot options = %#v", ctrl.snapshotOpts)
	}
}

func TestFindForwardsQueryParams(t *testing.T) {
	ctrl := &fakeController{snap: sampleSnapshot()}
	server := New("", ctrl)

	req := httptest.NewRequest(http.MethodGet, "/api/page/find?query=email&role=textbox&limit=1&include_hidden=true", nil)
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got snapshot.FindResult
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if ctrl.findOpts.Query != "email" || ctrl.findOpts.Role != "textbox" || ctrl.findOpts.Limit != 1 || !ctrl.findOpts.IncludeHidden {
		t.Fatalf("find options = %#v", ctrl.findOpts)
	}
	if len(got.Elements) != 1 || got.Elements[0].Ref != "e1" {
		t.Fatalf("elements = %#v, want only e1", got.Elements)
	}
}

func TestFillForwardsBody(t *testing.T) {
	ctrl := &fakeController{snap: sampleSnapshot()}
	server := New("", ctrl)
	body := bytes.NewBufferString(`{"query":"email","role":"textbox","text":"max@example.com","replace":true}`)

	req := httptest.NewRequest(http.MethodPost, "/api/page/fill", body)
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ctrl.fillOpts.Query != "email" || ctrl.fillOpts.Role != "textbox" || ctrl.fillOpts.Text != "max@example.com" || !ctrl.fillOpts.Replace {
		t.Fatalf("fill options = %#v", ctrl.fillOpts)
	}
	var got browser.ActionResult
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.OK {
		t.Fatalf("result = %#v, want ok", got)
	}
}

func TestUploadFileForwardsBody(t *testing.T) {
	ctrl := &fakeController{snap: sampleSnapshot()}
	server := New("", ctrl)
	body := bytes.NewBufferString(`{"query":"File input","path":"/tmp/upload.txt","paths":["/tmp/extra.txt"]}`)

	req := httptest.NewRequest(http.MethodPost, "/api/page/upload_file", body)
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ctrl.uploadOpts.Query != "File input" || ctrl.uploadOpts.Path != "/tmp/upload.txt" || len(ctrl.uploadOpts.Paths) != 1 || ctrl.uploadOpts.Paths[0] != "/tmp/extra.txt" {
		t.Fatalf("upload options = %#v", ctrl.uploadOpts)
	}
	var got browser.ActionResult
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.OK {
		t.Fatalf("result = %#v, want ok", got)
	}
}

func TestNewPageActionRoutes(t *testing.T) {
	ctrl := &fakeController{snap: sampleSnapshot()}
	server := New("", ctrl)

	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/page/batch", `{"steps":[{"action":"click","ref":"e1"}]}`},
		{http.MethodGet, "/api/page/observe", ``},
		{http.MethodPost, "/api/page/commit", `{"ref":"e1"}`},
		{http.MethodPost, "/api/page/assert_visible", `{"ref":"e1","timeout_ms":100}`},
		{http.MethodPost, "/api/page/assert_hidden", `{"ref":"e1","timeout_ms":100}`},
		{http.MethodPost, "/api/page/assert_text", `{"ref":"e1","text":"Email","timeout_ms":100}`},
		{http.MethodPost, "/api/page/assert_value", `{"ref":"e1","value":"x","timeout_ms":100}`},
		{http.MethodPost, "/api/page/click_xy", `{"x":12,"y":34}`},
		{http.MethodGet, "/api/page/console", ``},
		{http.MethodGet, "/api/page/trace", ``},
		{http.MethodPost, "/api/page/clear_trace", `{}`},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			var body *bytes.Buffer
			if tc.body == "" {
				body = bytes.NewBuffer(nil)
			} else {
				body = bytes.NewBufferString(tc.body)
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			req.Header.Set("content-type", "application/json")
			rec := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}

	if len(ctrl.batchSteps) != 1 || ctrl.batchSteps[0].Action != "click" {
		t.Fatalf("batch steps = %#v", ctrl.batchSteps)
	}
	if ctrl.commitRef != "e1" {
		t.Fatalf("commit ref = %q", ctrl.commitRef)
	}
	if ctrl.clickX != 12 || ctrl.clickY != 34 {
		t.Fatalf("click xy = %v,%v", ctrl.clickX, ctrl.clickY)
	}
}

func sampleSnapshot() snapshot.PageSnapshot {
	return snapshot.PageSnapshot{
		URL:   "https://example.test/form",
		Title: "Form",
		Elements: []snapshot.Element{
			{Ref: "e1", Role: "textbox", Name: "Email address", Tag: "input", Type: "email", Visible: true, InViewport: true},
			{Ref: "e2", Role: "textbox", Name: "Email confirmation", Tag: "input", Visible: true},
			{Ref: "e3", Role: "textbox", Name: "Hidden email", Tag: "input"},
			{Ref: "e4", Role: "button", Name: "Submit", Tag: "button", Visible: true, InViewport: true},
		},
		Accessibility: snapshot.AccessibilitySummary{Available: true, NodeCount: 4},
	}
}

type fakeController struct {
	snap         snapshot.PageSnapshot
	snapshotOpts snapshot.SnapshotOptions
	findOpts     snapshot.FindOptions
	fillOpts     snapshot.FillOptions
	uploadOpts   snapshot.UploadOptions
	batchSteps   []browser.BatchStep
	commitRef    string
	clickX       float64
	clickY       float64
}

func (f *fakeController) Open(context.Context, string) (browser.OpenResult, error) {
	return browser.OpenResult{}, nil
}

func (f *fakeController) OpenInGroup(context.Context, string, string) (browser.OpenResult, error) {
	return browser.OpenResult{}, nil
}

func (f *fakeController) ListTabs(context.Context) ([]browser.Tab, error) {
	return nil, nil
}

func (f *fakeController) FocusTab(context.Context, string) error {
	return nil
}

func (f *fakeController) CloseTab(context.Context, string) error {
	return nil
}

func (f *fakeController) GroupTabs(context.Context, []string, string, string) error { return nil }
func (f *fakeController) UngroupTabs(context.Context, []string) error               { return nil }

func (f *fakeController) Snapshot(_ context.Context, opts snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	f.snapshotOpts = opts
	return f.snap, nil
}

func (f *fakeController) Find(_ context.Context, opts snapshot.FindOptions) (snapshot.FindResult, error) {
	f.findOpts = opts
	return snapshot.FindResult{Elements: []snapshot.Element{f.snap.Elements[0]}}, nil
}

func (f *fakeController) Read(context.Context) (readability.PageRead, error) {
	return readability.PageRead{}, nil
}

func (f *fakeController) Click(context.Context, string) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}

func (f *fakeController) Type(context.Context, string, string) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}

func (f *fakeController) Fill(_ context.Context, opts snapshot.FillOptions) (browser.ActionResult, error) {
	f.fillOpts = opts
	return browser.ActionResult{OK: true}, nil
}

func (f *fakeController) UploadFile(_ context.Context, opts snapshot.UploadOptions) (browser.ActionResult, error) {
	f.uploadOpts = opts
	return browser.ActionResult{OK: true}, nil
}

func (f *fakeController) Select(context.Context, string, string) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}

func (f *fakeController) Press(context.Context, string) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}

func (f *fakeController) Scroll(context.Context, string) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}

func (f *fakeController) WaitFor(context.Context, string, time.Duration) error {
	return nil
}

func (f *fakeController) Screenshot(context.Context) (browser.Screenshot, error) {
	return browser.Screenshot{}, nil
}

func (f *fakeController) ScreenshotElement(context.Context, string) (browser.Screenshot, error) {
	return browser.Screenshot{}, nil
}

func (f *fakeController) Hover(context.Context, string) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}

func (f *fakeController) Evaluate(context.Context, string) (any, error) {
	return nil, nil
}

func (f *fakeController) NetworkRequests(context.Context, string) ([]browser.NetworkRequest, error) {
	return nil, nil
}

func (f *fakeController) ExecutePlan(context.Context, []browser.PlanStep) (browser.PlanResult, error) {
	return browser.PlanResult{OK: true}, nil
}

func (f *fakeController) ExecuteBatch(_ context.Context, steps []browser.BatchStep) (browser.BatchResult, error) {
	f.batchSteps = steps
	return browser.BatchResult{OK: true}, nil
}

func (f *fakeController) Observe(context.Context) (browser.ObserveResult, error) {
	return browser.ObserveResult{}, nil
}

func (f *fakeController) ConsoleMessages(context.Context) ([]browser.ConsoleMessage, error) {
	return nil, nil
}

func (f *fakeController) ClickXY(_ context.Context, x float64, y float64) (snapshot.ClickXYResult, error) {
	f.clickX = x
	f.clickY = y
	return snapshot.ClickXYResult{}, nil
}

func (f *fakeController) GetTrace() browser.TraceResult { return browser.TraceResult{} }
func (f *fakeController) ClearTrace()                   {}

func (f *fakeController) AssertVisible(context.Context, string, time.Duration) error { return nil }
func (f *fakeController) AssertText(context.Context, string, string, time.Duration) error {
	return nil
}
func (f *fakeController) AssertValue(context.Context, string, string, time.Duration) error {
	return nil
}
func (f *fakeController) AssertHidden(context.Context, string, time.Duration) error { return nil }

func (f *fakeController) CommitField(_ context.Context, ref string) error {
	f.commitRef = ref
	return nil
}
