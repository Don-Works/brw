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

// TestSnapshotDefaultsToBoundedFrontier guards the fix for the unbounded-HTTP-
// snapshot bug the Grok fleet surfaced: a bare /api/page/snapshot (no mode)
// must collapse to the bounded frontier so dense pages don't dump thousands of
// elements, matching the MCP surface.
func TestSnapshotDefaultsToBoundedFrontier(t *testing.T) {
	ctrl := &fakeController{snap: sampleSnapshot()}
	server := New("", ctrl)

	req := httptest.NewRequest(http.MethodGet, "/api/page/snapshot", nil)
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ctrl.snapshotOpts.Mode != "frontier" {
		t.Fatalf("default mode = %q, want frontier", ctrl.snapshotOpts.Mode)
	}
	if ctrl.snapshotOpts.Limit != 40 {
		t.Fatalf("default limit = %d, want 40", ctrl.snapshotOpts.Limit)
	}
	if !ctrl.snapshotOpts.ViewportOnly {
		t.Fatal("default viewport_only = false, want true for frontier")
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
		{http.MethodPost, "/api/page/cancel", `{"token":"op-9"}`},
		{http.MethodPost, "/api/page/click_text", `{"text":"Submit"}`},
		{http.MethodPost, "/api/page/drag", `{"from":{"ref":"e1"},"to":{"x":120,"y":40},"steps":8,"button":"left"}`},
		{http.MethodPost, "/api/page/mouse_down", `{"ref":"e1","button":"left"}`},
		{http.MethodPost, "/api/page/mouse_up", `{"x":12,"y":34,"button":"left"}`},
		{http.MethodPost, "/api/page/click", `{"ref":"e1","button":"right","click_count":2}`},
		{http.MethodGet, "/api/page/observe", ``},
		{http.MethodPost, "/api/page/commit", `{"ref":"e1"}`},
		{http.MethodPost, "/api/page/notify", `{"kind":"done","title":"Checkout complete","message":"Order placed"}`},
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
	if ctrl.cancelToken != "op-9" {
		t.Fatalf("cancel token = %q, want op-9", ctrl.cancelToken)
	}
	if ctrl.notifyOpts.Kind != "done" || ctrl.notifyOpts.Title != "Checkout complete" || ctrl.notifyOpts.Message != "Order placed" {
		t.Fatalf("notify options = %#v", ctrl.notifyOpts)
	}
	if ctrl.dragOpts.From.Ref != "e1" || ctrl.dragOpts.To.X == nil || *ctrl.dragOpts.To.X != 120 || ctrl.dragOpts.Steps != 8 {
		t.Fatalf("drag opts = %#v", ctrl.dragOpts)
	}
	if ctrl.mouseDownOpt.Ref != "e1" || ctrl.mouseDownOpt.Button != "left" {
		t.Fatalf("mouse_down opts = %#v", ctrl.mouseDownOpt)
	}
	if ctrl.mouseUpOpt.X == nil || *ctrl.mouseUpOpt.X != 12 || ctrl.mouseUpOpt.Y == nil || *ctrl.mouseUpOpt.Y != 34 {
		t.Fatalf("mouse_up opts = %#v", ctrl.mouseUpOpt)
	}
	// A non-default click (right button, double) must route through ClickButton.
	if ctrl.clickButton.Ref != "e1" || ctrl.clickButton.Button != "right" || ctrl.clickButton.ClickCount != 2 {
		t.Fatalf("click button opts = %#v", ctrl.clickButton)
	}
}

func TestNotifyForwardsBodyAndReturnsDelivery(t *testing.T) {
	ctrl := &fakeController{snap: sampleSnapshot()}
	server := New("", ctrl)
	body := bytes.NewBufferString(`{"kind":"error","title":"Login failed","message":"Wrong password"}`)

	req := httptest.NewRequest(http.MethodPost, "/api/page/notify", body)
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ctrl.notifyOpts.Kind != "error" || ctrl.notifyOpts.Title != "Login failed" || ctrl.notifyOpts.Message != "Wrong password" {
		t.Fatalf("notify options = %#v", ctrl.notifyOpts)
	}
	var got browser.NotifyResult
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Delivery != "extension" {
		t.Fatalf("notify result = %#v", got)
	}
}

// TestScreenshotAnnotateReturnsLegend verifies ?annotate=1&base64=1 routes to
// the Set-of-Marks path and returns the ref->box legend as JSON, while a plain
// request stays on the un-annotated path.
func TestScreenshotAnnotateReturnsLegend(t *testing.T) {
	ctrl := &fakeController{snap: sampleSnapshot()}
	server := New("", ctrl)

	req := httptest.NewRequest(http.MethodGet, "/api/visual/screenshot?annotate=1&base64=1", nil)
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got browser.AnnotatedScreenshot
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	entry, ok := got.Legend["e1"]
	if !ok {
		t.Fatalf("legend missing e1: %#v", got.Legend)
	}
	if entry.Ref != "e1" || entry.Role != "button" || entry.Width != 100 {
		t.Fatalf("legend entry = %#v", entry)
	}
	if got.Base64 == "" {
		t.Fatal("annotated response has empty base64 PNG")
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
	cancelToken  string
	notifyOpts   browser.NotifyOptions
	clickButton  browser.ClickButtonOptions
	dragOpts     browser.DragOptions
	mouseDownOpt browser.MouseButtonOptions
	mouseUpOpt   browser.MouseButtonOptions
}

func (f *fakeController) Open(context.Context, string) (browser.OpenResult, error) {
	return browser.OpenResult{}, nil
}

func (f *fakeController) OpenInGroup(context.Context, string, string) (browser.OpenResult, error) {
	return browser.OpenResult{}, nil
}

func (f *fakeController) OpenIncognito(context.Context, string) (browser.OpenResult, error) {
	return browser.OpenResult{}, nil
}

func (f *fakeController) CloseContext(context.Context, string) error { return nil }

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

func (f *fakeController) ReadData(context.Context) (snapshot.StructuredData, error) {
	return snapshot.StructuredData{}, nil
}

func (f *fakeController) Click(context.Context, string) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}

func (f *fakeController) ClickText(context.Context, snapshot.ClickTextOptions) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}

func (f *fakeController) Navigate(context.Context, string) (browser.ActionResult, error) {
	return browser.ActionResult{OK: true}, nil
}

func (f *fakeController) ClickButton(_ context.Context, opts browser.ClickButtonOptions) (browser.ActionResult, error) {
	f.clickButton = opts
	return browser.ActionResult{OK: true}, nil
}

func (f *fakeController) MouseDown(_ context.Context, opts browser.MouseButtonOptions) (browser.ActionResult, error) {
	f.mouseDownOpt = opts
	return browser.ActionResult{OK: true}, nil
}

func (f *fakeController) MouseUp(_ context.Context, opts browser.MouseButtonOptions) (browser.ActionResult, error) {
	f.mouseUpOpt = opts
	return browser.ActionResult{OK: true}, nil
}

func (f *fakeController) Drag(_ context.Context, opts browser.DragOptions) (browser.ActionResult, error) {
	f.dragOpts = opts
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

func (f *fakeController) ScreenshotAnnotated(context.Context, string) (browser.AnnotatedScreenshot, error) {
	return browser.AnnotatedScreenshot{
		MIMEType: "image/png",
		Data:     []byte("ANNOTATEDPNG"),
		Base64:   "QU5OT1RBVEVEUE5H",
		Legend: map[string]browser.LegendEntry{
			"e1": {Ref: "e1", Name: "Submit", Role: "button", X: 10, Y: 20, Width: 100, Height: 40},
		},
	}, nil
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

func (f *fakeController) NetworkCapture(context.Context, string) ([]snapshot.CapturedRequest, error) {
	return nil, nil
}

func (f *fakeController) ReplayRequest(context.Context, browser.ReplayRequestParams) (snapshot.ReplayResult, error) {
	return snapshot.ReplayResult{}, nil
}

func (f *fakeController) ExecutePlan(context.Context, []browser.PlanStep) (browser.PlanResult, error) {
	return browser.PlanResult{OK: true}, nil
}

func (f *fakeController) ExecuteBatch(_ context.Context, steps []browser.BatchStep) (browser.BatchResult, error) {
	f.batchSteps = steps
	return browser.BatchResult{OK: true}, nil
}

func (f *fakeController) Cancel(_ context.Context, token string) (browser.CancelResult, error) {
	f.cancelToken = token
	return browser.CancelResult{OK: true, Token: token, Cancelled: 1}, nil
}

func (f *fakeController) Observe(context.Context) (browser.ObserveResult, error) {
	return browser.ObserveResult{}, nil
}

func (f *fakeController) ConsoleMessages(context.Context) ([]browser.ConsoleMessage, error) {
	return nil, nil
}

func (f *fakeController) Downloads(context.Context) (browser.DownloadsResult, error) {
	return browser.DownloadsResult{Downloads: []browser.DownloadEntry{}}, nil
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

func (f *fakeController) Notify(_ context.Context, opts browser.NotifyOptions) (browser.NotifyResult, error) {
	f.notifyOpts = opts
	return browser.NotifyResult{OK: true, Delivery: "extension"}, nil
}
