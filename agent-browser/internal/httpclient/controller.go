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

func (c *Controller) Snapshot(ctx context.Context, opts snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	var out snapshot.PageSnapshot
	err := c.get(ctx, "/api/page/snapshot", snapshotValues(opts), &out)
	return out, err
}

func (c *Controller) Find(ctx context.Context, opts snapshot.FindOptions) (snapshot.FindResult, error) {
	var out snapshot.FindResult
	err := c.post(ctx, "/api/page/find", opts, &out)
	return out, err
}

func (c *Controller) Click(ctx context.Context, ref string) (browser.ActionResult, error) {
	var out browser.ActionResult
	err := c.post(ctx, "/api/page/click", map[string]string{"ref": ref}, &out)
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

func (c *Controller) get(ctx context.Context, path string, values url.Values, out any) error {
	reqURL := c.baseURL + path
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
