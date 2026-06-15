package httpclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/revitt/agent-browser/internal/browser"
	"github.com/revitt/agent-browser/internal/readability"
	"github.com/revitt/agent-browser/internal/snapshot"
)

type Controller struct {
	baseURL string
	client  *http.Client
}

func New(baseURL string, timeout time.Duration) (*Controller, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, errors.New("upstream HTTP URL is required")
	}
	if !strings.Contains(baseURL, "://") {
		baseURL = "http://" + baseURL
	}
	if _, err := url.ParseRequestURI(baseURL); err != nil {
		return nil, fmt.Errorf("invalid upstream HTTP URL: %w", err)
	}
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	return &Controller{
		baseURL: baseURL,
		client:  &http.Client{Timeout: timeout},
	}, nil
}

func (c *Controller) Open(ctx context.Context, targetURL string) (browser.OpenResult, error) {
	var out browser.OpenResult
	err := c.post(ctx, "/api/browser/open", map[string]string{"url": targetURL}, &out)
	return out, err
}

func (c *Controller) OpenInGroup(ctx context.Context, targetURL string, groupName string) (browser.OpenResult, error) {
	var out browser.OpenResult
	err := c.post(ctx, "/api/browser/open", map[string]string{"url": targetURL, "group": groupName}, &out)
	return out, err
}

func (c *Controller) ListTabs(ctx context.Context) ([]browser.Tab, error) {
	var out []browser.Tab
	err := c.get(ctx, "/api/browser/tabs", nil, &out)
	return out, err
}

func (c *Controller) FocusTab(ctx context.Context, id string) error {
	var out browser.ActionResult
	return c.post(ctx, "/api/browser/focus", map[string]string{"id": id}, &out)
}

func (c *Controller) CloseTab(ctx context.Context, id string) error {
	var out browser.ActionResult
	return c.post(ctx, "/api/browser/close", map[string]string{"id": id}, &out)
}

func (c *Controller) Read(ctx context.Context) (readability.PageRead, error) {
	var out readability.PageRead
	err := c.get(ctx, "/api/page/read", nil, &out)
	return out, err
}

func (c *Controller) ReadData(ctx context.Context) (snapshot.StructuredData, error) {
	var out snapshot.StructuredData
	err := c.get(ctx, "/api/page/read_data", nil, &out)
	return out, err
}

func (c *Controller) Snapshot(ctx context.Context, opts snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	var out snapshot.PageSnapshot
	err := c.get(ctx, "/api/page/snapshot", snapshotValues(opts), &out)
	return out, err
}

func (c *Controller) Find(ctx context.Context, opts snapshot.FindOptions) (snapshot.FindResult, error) {
	var out snapshot.FindResult
	err := c.get(ctx, "/api/page/find", findValues(opts), &out)
	return out, err
}

func (c *Controller) Click(ctx context.Context, ref string) (browser.ActionResult, error) {
	var out browser.ActionResult
	err := c.post(ctx, "/api/page/click", map[string]string{"ref": ref}, &out)
	return out, err
}

func (c *Controller) ClickText(ctx context.Context, opts snapshot.ClickTextOptions) (browser.ActionResult, error) {
	var out browser.ActionResult
	err := c.post(ctx, "/api/page/click_text", opts, &out)
	return out, err
}

func (c *Controller) Navigate(ctx context.Context, direction string) (browser.ActionResult, error) {
	var out browser.ActionResult
	err := c.post(ctx, "/api/page/navigate", map[string]string{"direction": direction}, &out)
	return out, err
}

func (c *Controller) Type(ctx context.Context, ref, text string) (browser.ActionResult, error) {
	var out browser.ActionResult
	err := c.post(ctx, "/api/page/type", map[string]string{"ref": ref, "text": text}, &out)
	return out, err
}

func (c *Controller) Fill(ctx context.Context, opts snapshot.FillOptions) (browser.ActionResult, error) {
	var out browser.ActionResult
	err := c.post(ctx, "/api/page/fill", opts, &out)
	return out, err
}

func (c *Controller) UploadFile(ctx context.Context, opts snapshot.UploadOptions) (browser.ActionResult, error) {
	var out browser.ActionResult
	err := c.post(ctx, "/api/page/upload_file", opts, &out)
	return out, err
}

func (c *Controller) Select(ctx context.Context, ref, value string) (browser.ActionResult, error) {
	var out browser.ActionResult
	err := c.post(ctx, "/api/page/select", map[string]string{"ref": ref, "value": value}, &out)
	return out, err
}

func (c *Controller) Press(ctx context.Context, key string) (browser.ActionResult, error) {
	var out browser.ActionResult
	err := c.post(ctx, "/api/page/press", map[string]string{"key": key}, &out)
	return out, err
}

func (c *Controller) Scroll(ctx context.Context, direction string) (browser.ActionResult, error) {
	var out browser.ActionResult
	err := c.post(ctx, "/api/page/scroll", map[string]string{"direction": direction}, &out)
	return out, err
}

func (c *Controller) WaitFor(ctx context.Context, condition string, timeout time.Duration) error {
	var out browser.ActionResult
	return c.post(ctx, "/api/page/wait_for", map[string]any{
		"condition":  condition,
		"timeout_ms": int(timeout / time.Millisecond),
	}, &out)
}

func (c *Controller) Screenshot(ctx context.Context) (browser.Screenshot, error) {
	var out browser.Screenshot
	err := c.get(ctx, "/api/visual/screenshot", url.Values{"base64": []string{"1"}}, &out)
	if err == nil && len(out.Data) == 0 && out.Base64 != "" {
		out.Data, _ = base64.StdEncoding.DecodeString(out.Base64)
	}
	return out, err
}

func (c *Controller) ScreenshotElement(ctx context.Context, ref string) (browser.Screenshot, error) {
	var out browser.Screenshot
	err := c.get(ctx, "/api/visual/screenshot_element", url.Values{
		"base64": []string{"1"},
		"ref":    []string{ref},
	}, &out)
	if err == nil && len(out.Data) == 0 && out.Base64 != "" {
		out.Data, _ = base64.StdEncoding.DecodeString(out.Base64)
	}
	return out, err
}

func (c *Controller) Hover(ctx context.Context, ref string) (browser.ActionResult, error) {
	var out browser.ActionResult
	err := c.post(ctx, "/api/page/hover", map[string]string{"ref": ref}, &out)
	return out, err
}

func (c *Controller) Evaluate(ctx context.Context, expression string) (any, error) {
	var out any
	err := c.post(ctx, "/api/page/evaluate", map[string]string{"expression": expression}, &out)
	return out, err
}

func (c *Controller) NetworkRequests(ctx context.Context, filter string) ([]browser.NetworkRequest, error) {
	var out []browser.NetworkRequest
	err := c.post(ctx, "/api/page/network_requests", map[string]string{"filter": filter}, &out)
	return out, err
}

func (c *Controller) ExecutePlan(ctx context.Context, steps []browser.PlanStep) (browser.PlanResult, error) {
	var out browser.PlanResult
	err := c.post(ctx, "/api/page/execute_plan", map[string]any{"steps": steps}, &out)
	return out, err
}

func (c *Controller) GroupTabs(ctx context.Context, tabIDs []string, name string, color string) error {
	return c.post(ctx, "/api/browser/group_tabs", map[string]any{"tab_ids": tabIDs, "name": name, "color": color}, nil)
}

func (c *Controller) UngroupTabs(ctx context.Context, tabIDs []string) error {
	return c.post(ctx, "/api/browser/ungroup_tabs", map[string]any{"tab_ids": tabIDs}, nil)
}

func (c *Controller) ExecuteBatch(ctx context.Context, steps []browser.BatchStep) (browser.BatchResult, error) {
	var out browser.BatchResult
	err := c.post(ctx, "/api/page/batch", map[string]any{"steps": steps}, &out)
	return out, err
}

func (c *Controller) Observe(ctx context.Context) (browser.ObserveResult, error) {
	var out browser.ObserveResult
	err := c.get(ctx, "/api/page/observe", nil, &out)
	return out, err
}

func (c *Controller) ConsoleMessages(ctx context.Context) ([]browser.ConsoleMessage, error) {
	var out []browser.ConsoleMessage
	err := c.get(ctx, "/api/page/console", nil, &out)
	return out, err
}

func (c *Controller) ClickXY(ctx context.Context, x, y float64) (snapshot.ClickXYResult, error) {
	var out snapshot.ClickXYResult
	err := c.post(ctx, "/api/page/click_xy", map[string]any{"x": x, "y": y}, &out)
	return out, err
}

func (c *Controller) GetTrace() browser.TraceResult {
	var out browser.TraceResult
	_ = c.get(context.Background(), "/api/page/trace", nil, &out)
	return out
}

func (c *Controller) ClearTrace() {
	_ = c.post(context.Background(), "/api/page/clear_trace", nil, nil)
}

func (c *Controller) AssertVisible(ctx context.Context, ref string, timeout time.Duration) error {
	return c.post(ctx, "/api/page/assert_visible", map[string]any{"ref": ref, "timeout_ms": timeout.Milliseconds()}, nil)
}

func (c *Controller) AssertText(ctx context.Context, ref, text string, timeout time.Duration) error {
	return c.post(ctx, "/api/page/assert_text", map[string]any{"ref": ref, "text": text, "timeout_ms": timeout.Milliseconds()}, nil)
}

func (c *Controller) AssertValue(ctx context.Context, ref, value string, timeout time.Duration) error {
	return c.post(ctx, "/api/page/assert_value", map[string]any{"ref": ref, "value": value, "timeout_ms": timeout.Milliseconds()}, nil)
}

func (c *Controller) AssertHidden(ctx context.Context, ref string, timeout time.Duration) error {
	return c.post(ctx, "/api/page/assert_hidden", map[string]any{"ref": ref, "timeout_ms": timeout.Milliseconds()}, nil)
}

func (c *Controller) CommitField(ctx context.Context, ref string) error {
	return c.post(ctx, "/api/page/commit", map[string]any{"ref": ref}, nil)
}

func (c *Controller) get(ctx context.Context, path string, values url.Values, out any) error {
	reqURL := c.baseURL + path
	if tabID := browser.TabIDFromContext(ctx); tabID != "" {
		if values == nil {
			values = url.Values{}
		}
		if values.Get("tab_id") == "" {
			values.Set("tab_id", tabID)
		}
	}
	if len(values) > 0 {
		reqURL += "?" + values.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *Controller) post(ctx context.Context, path string, body any, out any) error {
	body = withTabID(ctx, body)
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	return c.do(req, out)
}

func (c *Controller) do(req *http.Request, out any) error {
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var payload struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(data, &payload); err == nil && payload.Error != "" {
			return errors.New(payload.Error)
		}
		return fmt.Errorf("upstream HTTP %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func withTabID(ctx context.Context, body any) any {
	tabID := browser.TabIDFromContext(ctx)
	if tabID == "" {
		return body
	}
	data, err := json.Marshal(body)
	if err != nil {
		return body
	}
	payload := map[string]any{}
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &payload); err != nil {
			return body
		}
	}
	if _, ok := payload["tab_id"]; !ok {
		payload["tab_id"] = tabID
	}
	return payload
}

func snapshotValues(opts snapshot.SnapshotOptions) url.Values {
	values := url.Values{}
	addString(values, "mode", opts.Mode)
	addString(values, "query", opts.Query)
	addString(values, "text", opts.Text)
	addString(values, "role", opts.Role)
	addInt(values, "limit", opts.Limit)
	addBool(values, "viewport_only", opts.ViewportOnly)
	addBool(values, "include_hidden", opts.IncludeHidden)
	addBool(values, "include_ax", opts.IncludeAX)
	if opts.Since > 0 {
		values.Set("since", strconv.FormatInt(opts.Since, 10))
	}
	return values
}

func findValues(opts snapshot.FindOptions) url.Values {
	values := url.Values{}
	addString(values, "query", opts.Query)
	addString(values, "text", opts.Text)
	addString(values, "role", opts.Role)
	addInt(values, "limit", opts.Limit)
	addBool(values, "viewport_only", opts.ViewportOnly)
	addBool(values, "include_hidden", opts.IncludeHidden)
	return values
}

func addString(values url.Values, name, value string) {
	if value != "" {
		values.Set(name, value)
	}
}

func addInt(values url.Values, name string, value int) {
	if value > 0 {
		values.Set(name, strconv.Itoa(value))
	}
}

func addBool(values url.Values, name string, value bool) {
	if value {
		values.Set(name, "true")
	}
}
