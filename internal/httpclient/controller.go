package httpclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/recipe"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/Don-Works/brw/internal/usagelog"
)

const (
	maxUpstreamResponseBytes       = int64(64 << 20)
	maxUpstreamErrorBytes          = 8 << 10
	maxArtifactInfoResponseBytes   = int64(64 << 10)
	maxArtifactReadResponseBytes   = int64(8 << 20)
	maxArtifactSearchResponseBytes = int64(1 << 20)
	maxArtifactDeleteResponseBytes = int64(64 << 10)
)

type Controller struct {
	baseURL     string
	client      *http.Client
	sessionID   string
	ownerID     string
	agentName   atomic.Value // string; display name for the daemon's per-agent tab group
	nextRequest atomic.Uint64
}

type Health struct {
	OK       bool                 `json:"ok"`
	Identity brwidentity.Identity `json:"identity,omitempty"`
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
	sessionID := usagelog.NewID()
	c := &Controller{
		baseURL:   baseURL,
		client:    &http.Client{Timeout: timeout},
		sessionID: sessionID,
		ownerID:   stableOwnerID(sessionID),
	}
	c.agentName.Store(sanitizeAgentName(os.Getenv("BRW_AGENT_NAME")))
	return c, nil
}

// SetAgentName installs the MCP client's display name (whoami) as this
// session's tab-group label, unless the operator already pinned one via
// BRW_AGENT_NAME. The daemon appends a per-owner suffix, so two agents with
// the same display name still get separate tab groups.
func (c *Controller) SetAgentName(name string) {
	if current, _ := c.agentName.Load().(string); current != "" {
		return
	}
	if name = sanitizeAgentName(name); name != "" {
		c.agentName.Store(name)
	}
}

// sanitizeAgentName keeps the header value header-safe and title-shaped. The
// daemon applies its own (stricter) sanitization before showing it in Chrome.
func sanitizeAgentName(name string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
		if b.Len() >= 64 {
			break
		}
	}
	return b.String()
}

// stableOwnerID converts the gateway's logical browser-session id into a
// privacy-safe fixed-width lease owner. The fallback remains this proxy's
// correlation session, so direct brwd --upstream-http users are isolated too.
func stableOwnerID(fallback string) string {
	raw := strings.TrimSpace(os.Getenv("BRW_OWNER_ID"))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("MCPLEXER_BROWSER_SESSION_ID"))
	}
	if raw == "" {
		return fallback
	}
	sum := sha256.Sum256([]byte("brw-tab-owner-v1\x00" + raw))
	return fmt.Sprintf("owner-%x", sum[:12])
}

// SessionID is the non-secret correlation id forwarded to the long-lived brw
// daemon. It lets usage logs group calls made by one disposable MCP proxy
// without recording prompts, arguments, URLs, or browser content.
func (c *Controller) SessionID() string { return c.sessionID }

// OwnerID is the stable, non-secret tab-lease identity sent to the shared
// daemon. It can outlive a disposable upstream proxy for the same agent session.
func (c *Controller) OwnerID() string { return c.ownerID }

func (c *Controller) Health(ctx context.Context) (Health, error) {
	var out Health
	err := c.get(ctx, "/health", nil, &out)
	return out, err
}

func (c *Controller) Open(ctx context.Context, targetURL string) (browser.OpenResult, error) {
	var out browser.OpenResult
	err := c.post(ctx, "/api/browser/open", map[string]string{"url": targetURL}, &out)
	return out, err
}

func (c *Controller) OpenInGroup(ctx context.Context, targetURL string, opts browser.TabGroupOptions) (browser.OpenResult, error) {
	var out browser.OpenResult
	err := c.post(ctx, "/api/browser/open", map[string]string{
		"url":         targetURL,
		"group":       opts.Name,
		"group_id":    opts.GroupID,
		"group_color": opts.Color,
	}, &out)
	return out, err
}

func (c *Controller) OpenIncognito(ctx context.Context, targetURL string) (browser.OpenResult, error) {
	var out browser.OpenResult
	err := c.post(ctx, "/api/browser/open_incognito", map[string]string{"url": targetURL}, &out)
	return out, err
}

func (c *Controller) CloseContext(ctx context.Context, contextID string) error {
	var out browser.ActionResult
	return c.post(ctx, "/api/browser/close_context", map[string]string{"context_id": contextID}, &out)
}

func (c *Controller) ListTabs(ctx context.Context) ([]browser.Tab, error) {
	var out []browser.Tab
	err := c.get(ctx, "/api/browser/tabs", nil, &out)
	return out, err
}

func (c *Controller) ListTabGroups(ctx context.Context) ([]browser.TabGroup, error) {
	var out []browser.TabGroup
	err := c.get(ctx, "/api/browser/tab_groups", nil, &out)
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

// Read fetches the page unbounded. In proxy mode the MCP server applies the
// caller's own max_chars/offset window to what comes back, so letting the
// upstream apply its default bound too would silently cap an explicitly
// unbounded read at the upstream default.
func (c *Controller) Read(ctx context.Context) (readability.PageRead, error) {
	var out readability.PageRead
	values := url.Values{}
	values.Set("max_chars", strconv.Itoa(readability.UnboundedReadChars))
	values.Set("max_links", strconv.Itoa(readability.UnboundedReadChars))
	values.Set("max_headings", strconv.Itoa(readability.UnboundedReadChars))
	// Section selection is applied by the MCP layer on the full document, so the
	// proxy deliberately does not forward it here.
	err := c.get(ctx, "/api/page/read", values, &out)
	return out, err
}

// ReadWindow applies the requested section/include/paging bounds on the browser
// host. MCP uses this optional capability in upstream mode, avoiding full-page
// transfer merely to return a small context window.
func (c *Controller) ReadWindow(ctx context.Context, opts readability.ReadOptions) (readability.PageRead, error) {
	if err := opts.Validate(); err != nil {
		return readability.PageRead{}, err
	}
	values := url.Values{}
	// Send zero explicitly: on the HTTP surface zero means the normal bounded
	// default, while absence preserves the legacy unbounded endpoint contract.
	values.Set("max_chars", strconv.Itoa(opts.MaxChars))
	values.Set("offset", strconv.Itoa(opts.Offset))
	values.Set("max_links", strconv.Itoa(opts.MaxLinks))
	values.Set("max_headings", strconv.Itoa(opts.MaxHeadings))
	if len(opts.Include) > 0 {
		values.Set("include", strings.Join(opts.Include, ","))
	}
	if strings.TrimSpace(opts.Section) != "" {
		values.Set("section", strings.TrimSpace(opts.Section))
	}
	var out readability.PageRead
	err := c.get(ctx, "/api/page/read", values, &out)
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

func (c *Controller) NavigateTo(ctx context.Context, url string) (browser.ActionResult, error) {
	var out browser.ActionResult
	err := c.post(ctx, "/api/page/navigate_to", map[string]string{"url": url}, &out)
	return out, err
}

func (c *Controller) ClickButton(ctx context.Context, opts browser.ClickButtonOptions) (browser.ActionResult, error) {
	var out browser.ActionResult
	body := map[string]any{"ref": opts.Ref, "button": opts.Button, "click_count": opts.ClickCount}
	if opts.X != nil {
		body["x"] = *opts.X
	}
	if opts.Y != nil {
		body["y"] = *opts.Y
	}
	err := c.post(ctx, "/api/page/click", body, &out)
	return out, err
}

func (c *Controller) MouseDown(ctx context.Context, opts browser.MouseButtonOptions) (browser.ActionResult, error) {
	return c.mouseButton(ctx, "/api/page/mouse_down", opts)
}

func (c *Controller) MouseUp(ctx context.Context, opts browser.MouseButtonOptions) (browser.ActionResult, error) {
	return c.mouseButton(ctx, "/api/page/mouse_up", opts)
}

func (c *Controller) mouseButton(ctx context.Context, path string, opts browser.MouseButtonOptions) (browser.ActionResult, error) {
	var out browser.ActionResult
	body := map[string]any{"ref": opts.Ref, "button": opts.Button}
	if opts.X != nil {
		body["x"] = *opts.X
	}
	if opts.Y != nil {
		body["y"] = *opts.Y
	}
	err := c.post(ctx, path, body, &out)
	return out, err
}

func (c *Controller) Drag(ctx context.Context, opts browser.DragOptions) (browser.ActionResult, error) {
	var out browser.ActionResult
	err := c.post(ctx, "/api/page/drag", map[string]any{
		"from":   opts.From,
		"to":     opts.To,
		"steps":  opts.Steps,
		"button": opts.Button,
	}, &out)
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
		out.Data, err = base64.StdEncoding.DecodeString(out.Base64)
	}
	return out, err
}

func (c *Controller) ScreenshotAnnotated(ctx context.Context, aopts browser.AnnotatedScreenshotOptions) (browser.AnnotatedScreenshot, error) {
	var out browser.AnnotatedScreenshot
	// annotate=1 routes the bridge to the Set-of-Marks path; base64=1 forces the
	// JSON response so the ref->box legend (not representable in a raw PNG body)
	// comes back. ref/region scope the capture to a tight annotated crop.
	vals := url.Values{
		"base64":   []string{"1"},
		"annotate": []string{"1"},
	}
	if strings.TrimSpace(aopts.Mode) != "" {
		vals.Set("mode", aopts.Mode)
	}
	if strings.TrimSpace(aopts.Ref) != "" {
		vals.Set("ref", aopts.Ref)
	}
	if !aopts.Region.IsZero() {
		vals.Set("region_x", strconv.FormatFloat(aopts.Region.X, 'f', -1, 64))
		vals.Set("region_y", strconv.FormatFloat(aopts.Region.Y, 'f', -1, 64))
		vals.Set("region_w", strconv.FormatFloat(aopts.Region.Width, 'f', -1, 64))
		vals.Set("region_h", strconv.FormatFloat(aopts.Region.Height, 'f', -1, 64))
	}
	err := c.get(ctx, "/api/visual/screenshot", vals, &out)
	if err == nil && len(out.Data) == 0 && out.Base64 != "" {
		out.Data, err = base64.StdEncoding.DecodeString(out.Base64)
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
		out.Data, err = base64.StdEncoding.DecodeString(out.Base64)
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

func (c *Controller) NetworkCapture(ctx context.Context, filter string) ([]snapshot.CapturedRequest, error) {
	var out []snapshot.CapturedRequest
	err := c.post(ctx, "/api/page/network_capture", map[string]string{"filter": filter}, &out)
	return out, err
}

func (c *Controller) ReplayRequest(ctx context.Context, params browser.ReplayRequestParams) (snapshot.ReplayResult, error) {
	var out snapshot.ReplayResult
	err := c.post(ctx, "/api/page/replay_request", map[string]any{
		"method":    params.Method,
		"url":       params.URL,
		"headers":   params.Headers,
		"body":      params.Body,
		"offset":    params.Offset,
		"max_bytes": params.MaxBytes,
	}, &out)
	return out, err
}

func (c *Controller) ExecutePlan(ctx context.Context, steps []browser.PlanStep) (browser.PlanResult, error) {
	var out browser.PlanResult
	err := c.post(ctx, "/api/page/execute_plan", map[string]any{"steps": steps}, &out)
	return out, err
}

func (c *Controller) GroupTabs(ctx context.Context, tabIDs []string, opts browser.TabGroupOptions) error {
	return c.post(ctx, "/api/browser/group_tabs", map[string]any{
		"tab_ids":  tabIDs,
		"name":     opts.Name,
		"color":    opts.Color,
		"group_id": opts.GroupID,
	}, nil)
}

func (c *Controller) UngroupTabs(ctx context.Context, tabIDs []string) error {
	return c.post(ctx, "/api/browser/ungroup_tabs", map[string]any{"tab_ids": tabIDs}, nil)
}

func (c *Controller) EmulateDevice(ctx context.Context, opts browser.DeviceEmulationOptions) (browser.DeviceEmulationResult, error) {
	var out browser.DeviceEmulationResult
	err := c.post(ctx, "/api/browser/emulate_device", opts, &out)
	return out, err
}

func (c *Controller) ExecuteBatch(ctx context.Context, steps []browser.BatchStep) (browser.BatchResult, error) {
	var out browser.BatchResult
	err := c.post(ctx, "/api/page/batch", map[string]any{"steps": steps}, &out)
	return out, err
}

func (c *Controller) Cancel(ctx context.Context, token string) (browser.CancelResult, error) {
	var out browser.CancelResult
	err := c.post(ctx, "/api/page/cancel", map[string]any{"token": token}, &out)
	return out, err
}

func (c *Controller) Observe(ctx context.Context) (browser.ObserveResult, error) {
	var out browser.ObserveResult
	err := c.get(ctx, "/api/page/observe", nil, &out)
	return out, err
}

// ConsoleMessages drains the upstream console unfiltered. The MCP server keeps
// its own retention buffer and applies the caller's filter to that, so an
// upstream limit here would truncate messages before brw ever buffered them.
func (c *Controller) ConsoleMessages(ctx context.Context) ([]browser.ConsoleMessage, error) {
	var out []browser.ConsoleMessage
	values := url.Values{}
	values.Set("limit", "-1")
	err := c.get(ctx, "/api/page/console", values, &out)
	return out, err
}

func (c *Controller) Downloads(ctx context.Context) (browser.DownloadsResult, error) {
	var out browser.DownloadsResult
	err := c.get(ctx, "/api/page/downloads", nil, &out)
	return out, err
}

func (c *Controller) ClickXY(ctx context.Context, x, y float64) (snapshot.ClickXYResult, error) {
	var out snapshot.ClickXYResult
	err := c.post(ctx, "/api/page/click_xy", map[string]any{"x": x, "y": y}, &out)
	return out, err
}

// ResizeWindow proxies a real OS window change to the upstream daemon, which
// owns the browser and therefore the window.
func (c *Controller) ResizeWindow(ctx context.Context, opts browser.WindowResizeOptions) (browser.WindowResizeResult, error) {
	var out browser.WindowResizeResult
	err := c.post(ctx, "/api/browser/resize_window", opts, &out)
	return out, err
}

func (c *Controller) WindowBounds(ctx context.Context) (snapshot.WindowBoundsResult, error) {
	var out snapshot.WindowBoundsResult
	err := c.get(ctx, "/api/page/window_bounds", nil, &out)
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

func (c *Controller) Notify(ctx context.Context, opts browser.NotifyOptions) (browser.NotifyResult, error) {
	var out browser.NotifyResult
	err := c.post(ctx, "/api/page/notify", map[string]any{
		"kind":    opts.Kind,
		"title":   opts.Title,
		"message": opts.Message,
	}, &out)
	return out, err
}

// CaptureArtifact delegates capture to the browser host. In upstream mode this
// is the critical data-locality boundary: only payload-free metadata crosses
// back to the disposable MCP process.
func (c *Controller) CaptureArtifact(ctx context.Context, opts artifact.CaptureOptions) (artifact.Meta, error) {
	if opts.TTL > 0 && opts.TTLSeconds == 0 {
		seconds := opts.TTL / time.Second
		if opts.TTL%time.Second != 0 {
			seconds++
		}
		maxInt := int64(^uint(0) >> 1)
		if int64(seconds) > maxInt {
			return artifact.Meta{}, errors.New("artifact TTL is too large for the remote protocol")
		}
		opts.TTLSeconds = int(seconds)
	}
	var out artifact.Meta
	client := c.client
	if opts.Kind == "video" {
		minimum := time.Duration(opts.DurationMS)*time.Millisecond + 30*time.Second
		client = withMinimumTimeout(client, minimum)
	}
	err := c.postWithClient(ctx, client, "/api/artifacts/capture", opts, &out)
	return out, err
}

func (c *Controller) ArtifactInfo(ctx context.Context, id string) (artifact.Meta, error) {
	var out artifact.Meta
	err := c.postExactWithLimit(ctx, "/api/artifacts/info", artifactIDRequest(id), &out, maxArtifactInfoResponseBytes)
	return out, artifactClientError(ctx, "info", err)
}

func (c *Controller) ReadArtifact(ctx context.Context, id string, offset int64, maxBytes int) (artifact.Chunk, error) {
	var out artifact.Chunk
	body := map[string]any{"artifact_id": id, "offset": offset, "max_bytes": maxBytes}
	err := c.postExactWithLimit(ctx, "/api/artifacts/read", body, &out, maxArtifactReadResponseBytes)
	return out, artifactClientError(ctx, "read", err)
}

func (c *Controller) SearchArtifact(ctx context.Context, id, query string, limit int) ([]artifact.TextHit, error) {
	var out []artifact.TextHit
	body := map[string]any{"artifact_id": id, "query": query, "limit": limit}
	err := c.postExactWithLimit(ctx, "/api/artifacts/search", body, &out, maxArtifactSearchResponseBytes)
	return out, artifactClientError(ctx, "search", err)
}

func (c *Controller) DeleteArtifact(ctx context.Context, id string) error {
	err := c.postExactWithLimit(ctx, "/api/artifacts/delete", artifactIDRequest(id), nil, maxArtifactDeleteResponseBytes)
	return artifactClientError(ctx, "delete", err)
}

func artifactIDRequest(id string) map[string]any {
	return map[string]any{"artifact_id": id}
}

func artifactClientError(ctx context.Context, operation string, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// An older or compromised upstream may reflect request values in its error
	// body. Never forward that text across the artifact privacy boundary.
	return fmt.Errorf("artifact %s request failed", operation)
}

func (c *Controller) SearchRecipes(ctx context.Context, query, origin string, limit int) ([]recipe.Match, error) {
	var out []recipe.Match
	err := c.post(ctx, "/api/recipes/search", map[string]any{"query": query, "origin": origin, "limit": limit}, &out)
	return out, err
}

func (c *Controller) RunRecipe(ctx context.Context, request recipe.RunRequest) (recipe.RunResult, error) {
	var out recipe.RunResult
	// Ordinary browser calls default to a short transport timeout, but one valid
	// recipe may contain bounded timers/events up to the runner's 30-minute cap.
	// Keep caller context cancellation authoritative while preventing the proxy
	// client from terminating a still-valid recipe at 20 seconds.
	client := withMinimumTimeout(c.client, recipe.DefaultMaxRunDuration+30*time.Second)
	err := c.postWithClient(ctx, client, "/api/recipes/run", request, &out)
	return out, err
}

func withMinimumTimeout(client *http.Client, minimum time.Duration) *http.Client {
	if client.Timeout == 0 || client.Timeout >= minimum {
		return client
	}
	copy := *client
	copy.Timeout = minimum
	return &copy
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
	return c.postWithClient(ctx, c.client, path, body, out)
}

func (c *Controller) postWithClient(ctx context.Context, client *http.Client, path string, body any, out any) error {
	body = withTabID(ctx, body)
	body = withSnapshot(ctx, body)
	return c.postJSONWithLimit(ctx, client, path, body, out, maxUpstreamResponseBytes)
}

// postExactWithLimit intentionally does not add tab_id or snapshot fields.
// Artifact handle operations are host-local and use strict, fixed request
// schemas; keeping this separate prevents unrelated context values from making
// those requests invalid or widening what crosses the proxy boundary.
func (c *Controller) postExactWithLimit(ctx context.Context, path string, body any, out any, maxResponseBytes int64) error {
	return c.postJSONWithLimit(ctx, c.client, path, body, out, maxResponseBytes)
}

func (c *Controller) postJSONWithLimit(ctx context.Context, client *http.Client, path string, body any, out any, maxResponseBytes int64) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	return c.doWithClientLimit(client, req, out, maxResponseBytes)
}

func (c *Controller) do(req *http.Request, out any) error {
	return c.doWithClient(c.client, req, out)
}

func (c *Controller) doWithClient(client *http.Client, req *http.Request, out any) error {
	return c.doWithClientLimit(client, req, out, maxUpstreamResponseBytes)
}

func (c *Controller) doWithClientLimit(client *http.Client, req *http.Request, out any, maxResponseBytes int64) error {
	req.Header.Set(usagelog.HeaderSessionID, c.sessionID)
	req.Header.Set(usagelog.HeaderOwnerID, c.ownerID)
	req.Header.Set(usagelog.HeaderRequestID, fmt.Sprintf("%s:%d", c.sessionID, c.nextRequest.Add(1)))
	req.Header.Set(usagelog.HeaderClient, "brw-httpclient")
	if name, _ := c.agentName.Load().(string); name != "" {
		req.Header.Set(usagelog.HeaderAgentName, name)
	}
	return c.doRequestWithLimit(client, req, out, maxResponseBytes)
}

func (c *Controller) doRequestWithLimit(client *http.Client, req *http.Request, out any, maxResponseBytes int64) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if maxResponseBytes < 1 || maxResponseBytes > maxUpstreamResponseBytes {
		maxResponseBytes = maxUpstreamResponseBytes
	}
	limit := maxResponseBytes
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Error text is diagnostic, not a data result. Never read tens of MiB only
		// to throw almost all of it away after allocation.
		limit = maxUpstreamErrorBytes
	}
	if resp.ContentLength > limit && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return upstreamResponseBoundError(maxResponseBytes)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return upstreamResponseBoundError(maxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		truncated := int64(len(data)) > limit
		if truncated {
			data = data[:limit]
		}
		var payload struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(data, &payload); err == nil && payload.Error != "" {
			return errors.New(boundedUpstreamError(payload.Error))
		}
		message := boundedUpstreamError(string(data))
		if truncated && !strings.Contains(message, "[truncated]") {
			message += "… [truncated]"
		}
		return errors.New(boundedUpstreamError(fmt.Sprintf("upstream HTTP %s: %s", resp.Status, message)))
	}
	if out == nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("upstream HTTP response contains trailing JSON")
	}
	return nil
}

func upstreamResponseBoundError(maxResponseBytes int64) error {
	if maxResponseBytes == maxUpstreamResponseBytes {
		return errors.New("upstream HTTP response exceeds 64 MiB")
	}
	return errors.New("upstream HTTP response exceeds artifact operation bound")
}

func boundedUpstreamError(value string) string {
	value = strings.ToValidUTF8(strings.TrimSpace(value), "�")
	if len(value) <= maxUpstreamErrorBytes {
		return value
	}
	value = strings.ToValidUTF8(value[:maxUpstreamErrorBytes], "�")
	return value + "… [truncated]"
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

// withSnapshot forwards the post-action snapshot request across the HTTP boundary.
// The MCP server signals snapshot:true by stashing a flag in the context
// (browser.WithWantSnapshot); a context value cannot cross to the bridge over
// HTTP, so we re-materialize it as an explicit body field that the bridge's HTTP
// handlers read back into WithWantSnapshot. Without this, snapshot:true is
// silently dropped on the upstream-http topology (works only in single-process
// direct-CDP mode).
func withSnapshot(ctx context.Context, body any) any {
	if !browser.WantSnapshotFromCtx(ctx) {
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
	if _, ok := payload["snapshot"]; !ok {
		payload["snapshot"] = true
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
	addBool(values, "include_frames", opts.IncludeFrames)
	addBool(values, "text_content", opts.TextContent)
	addBool(values, "visual_islands", opts.VisualIslands)
	addInt(values, "visual_islands_limit", opts.VisualIslandsLimit)
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
	addBool(values, "text_content", opts.TextContent)
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
