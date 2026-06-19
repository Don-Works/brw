package browser

import (
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

type Config struct {
	ChromePath       string
	UserDataDir      string
	ProfileDirectory string
	RemoteURL        string
	Port             int
	Extensions       []string
	ChromeArgs       []string
	Timeout          time.Duration
}

type Tab struct {
	ID            string `json:"id"`
	URL           string `json:"url"`
	Title         string `json:"title"`
	Type          string `json:"type"`
	WindowID      int    `json:"window_id,omitempty"`
	WindowType    string `json:"window_type,omitempty"`
	Active        bool   `json:"active,omitempty"`
	Highlighted   bool   `json:"highlighted,omitempty"`
	WindowFocused bool   `json:"window_focused,omitempty"`
	OpenerTabID   string `json:"opener_tab_id,omitempty"`
	Popup         bool   `json:"popup,omitempty"`
	// BrowserContextID is set when the tab lives in a non-default (incognito)
	// browser context created via browser_open_incognito. Pass it to
	// browser_close_context to dispose that isolated context.
	BrowserContextID string `json:"browser_context_id,omitempty"`
}

type OpenResult struct {
	Tab Tab `json:"tab"`
	// Ready reports whether the page was confirmed usable (document committed /
	// readyState settled) before Open returned. When true, callers can issue an
	// immediate browser_evaluate / browser_read without racing the transient
	// about:blank state Chrome reports mid-navigation. False means readiness
	// could not be confirmed within the wait window — the tab still exists, but a
	// caller may want to browser_wait before acting on it.
	Ready bool `json:"ready"`
}

type ActionResult struct {
	OK           bool               `json:"ok"`
	Message      string             `json:"message,omitempty"`
	Warning      string             `json:"warning,omitempty"`
	TabID        string             `json:"tab_id,omitempty"`
	Version      int64              `json:"version,omitempty"`
	URL          string             `json:"url,omitempty"`
	Title        string             `json:"title,omitempty"`
	Focus        string             `json:"focus,omitempty"`
	ChangedState *bool              `json:"changed_state,omitempty"`
	Targets      []Tab              `json:"targets,omitempty"`
	Changed      []string           `json:"changed,omitempty"`
	Elements     []snapshot.Element `json:"elements,omitempty"`
	DurationMS   int64              `json:"duration_ms,omitempty"`
}

type Screenshot struct {
	MIMEType string `json:"mime_type"`
	Data     []byte `json:"-"`
	Base64   string `json:"base64,omitempty"`
}

// LegendEntry maps one Set-of-Marks label drawn on an annotated screenshot back
// to the semantic ref agents pass to browser_click. X/Y/Width/Height are the
// element's top-level viewport box (the same coordinate space the overlay label
// is painted at and that the CDP mouse path operates in).
type LegendEntry struct {
	Ref    string  `json:"ref"`
	Name   string  `json:"name,omitempty"`
	Role   string  `json:"role,omitempty"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// ScreenshotRegion is an optional viewport-space clip rectangle for an annotated
// screenshot. When set (Width and Height both > 0) the CDP capture is clipped to
// this box, producing a TIGHT crop of just one visual island — a far smaller PNG
// (fewer vision tokens) than a full-viewport capture.
type ScreenshotRegion struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// IsZero reports whether the region carries no usable clip (no width/height), in
// which case the capture falls back to the full viewport.
func (r ScreenshotRegion) IsZero() bool { return r.Width <= 0 || r.Height <= 0 }

// AnnotatedScreenshotOptions drives a Set-of-Marks capture. Mode selects which
// elements are labelled (defaults to "frontier"). Ref OR Region scopes the
// capture to a tight crop: Ref clips to that element's box (with a small margin so
// the label badge is not clipped off), Region clips to an explicit viewport
// rectangle. When both are empty the capture is the full viewport (today's
// behavior). Marks outside the clip are dropped so the legend matches the crop.
type AnnotatedScreenshotOptions struct {
	Mode   string           `json:"mode,omitempty"`
	Ref    string           `json:"ref,omitempty"`
	Region ScreenshotRegion `json:"region,omitempty"`
}

// AnnotatedScreenshot is a Set-of-Marks (SoM) capture: a PNG with transient
// numbered/labelled boxes drawn over the frontier elements, plus a legend mapping
// each drawn ref to its box + role + name. The overlay is injected immediately
// before capture and removed immediately after, so it never mutates the page the
// agent then acts on. Labels are the SAME refs returned by browser_snapshot.
type AnnotatedScreenshot struct {
	MIMEType string                 `json:"mime_type"`
	Data     []byte                 `json:"-"`
	Base64   string                 `json:"base64,omitempty"`
	Legend   map[string]LegendEntry `json:"legend"`
}

// NotifyOptions describes a desktop notification to surface to the human
// operator when the agent reaches a hand-off point (needs_input), finishes
// (done), or fails (error). It is transport-agnostic: the extension bridge
// turns it into a chrome.notifications.create call, while a direct-CDP session
// falls back to the in-page Notification API on a best-effort basis.
type NotifyOptions struct {
	// Kind classifies the notification: "needs_input", "done", or "error".
	Kind string `json:"kind"`
	// Title is the short heading line of the notification.
	Title string `json:"title"`
	// Message is the notification body.
	Message string `json:"message"`
}

// NotifyResult reports how a notification request was satisfied so the caller
// can tell whether a real desktop notification was raised or only a
// best-effort/unavailable fallback occurred. It never fakes success.
type NotifyResult struct {
	OK bool `json:"ok"`
	// Delivery is one of: "extension" (chrome.notifications), "page"
	// (in-page Notification API best-effort), or "unavailable".
	Delivery string `json:"delivery"`
	// Note carries human-readable detail, e.g. why delivery is best-effort
	// or why a real notification could not be raised.
	Note string `json:"note,omitempty"`
}

type NetworkRequest struct {
	URL           string `json:"url"`
	InitiatorType string `json:"initiator_type,omitempty"`
	StartTime     int64  `json:"start_time"`
	Duration      int64  `json:"duration"`
	TransferSize  int64  `json:"transfer_size,omitempty"`
	Status        int    `json:"status,omitempty"`
}

type PlanStep struct {
	Action     string `json:"action"`
	Ref        string `json:"ref,omitempty"`
	Text       string `json:"text,omitempty"`
	Value      string `json:"value,omitempty"`
	Direction  string `json:"direction,omitempty"`
	Condition  string `json:"condition,omitempty"`
	TimeoutMS  int    `json:"timeout_ms,omitempty"`
	URL        string `json:"url,omitempty"`
	ID         string `json:"id,omitempty"`
	Key        string `json:"key,omitempty"`
	ExpectRef  string `json:"expect_ref,omitempty"`
	ExpectRole string `json:"expect_role,omitempty"`
}

type PlanStepResult struct {
	Index    int                    `json:"index"`
	Action   string                 `json:"action"`
	OK       bool                   `json:"ok"`
	Message  string                 `json:"message,omitempty"`
	Error    string                 `json:"error,omitempty"`
	Snapshot *snapshot.PageSnapshot `json:"snapshot,omitempty"`
}

type PlanResult struct {
	OK             bool             `json:"ok"`
	Steps          []PlanStepResult `json:"steps"`
	FailedAt       *int             `json:"failed_at,omitempty"`
	Error          string           `json:"error,omitempty"`
	Cancelled      bool             `json:"cancelled,omitempty"`
	StepsCompleted int              `json:"steps_completed"`
}

type BatchStep struct {
	Action        string `json:"action"`
	Ref           string `json:"ref,omitempty"`
	Text          string `json:"text,omitempty"`
	Value         string `json:"value,omitempty"`
	Direction     string `json:"direction,omitempty"`
	Condition     string `json:"condition,omitempty"`
	TimeoutMS     int    `json:"timeout_ms,omitempty"`
	URL           string `json:"url,omitempty"`
	ID            string `json:"id,omitempty"`
	Key           string `json:"key,omitempty"`
	AssertRef     string `json:"assert_ref,omitempty"`
	AssertText    string `json:"assert_text,omitempty"`
	AssertValue   string `json:"assert_value,omitempty"`
	AssertVisible *bool  `json:"assert_visible,omitempty"`
	AssertHidden  *bool  `json:"assert_hidden,omitempty"`
}

type BatchStepResult struct {
	Index  int    `json:"index"`
	Action string `json:"action"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

type BatchResult struct {
	OK             bool              `json:"ok"`
	Steps          []BatchStepResult `json:"steps"`
	Error          string            `json:"error,omitempty"`
	TabID          string            `json:"tab_id,omitempty"`
	URL            string            `json:"url,omitempty"`
	Title          string            `json:"title,omitempty"`
	Focus          string            `json:"focus,omitempty"`
	Changed        []string          `json:"changed,omitempty"`
	Version        int64             `json:"version,omitempty"`
	Cancelled      bool              `json:"cancelled,omitempty"`
	StepsCompleted int               `json:"steps_completed"`
}

// MousePoint identifies a mouse target either by a semantic element ref or by
// explicit viewport coordinates. Exactly one form should be supplied; when both
// are present the ref wins and is resolved (with scroll-into-view + iframe
// translation) through the standard ResolveOrRecoverBox path.
type MousePoint struct {
	Ref string   `json:"ref,omitempty"`
	X   *float64 `json:"x,omitempty"`
	Y   *float64 `json:"y,omitempty"`
}

// HasRef reports whether a semantic ref was supplied.
func (p MousePoint) HasRef() bool { return strings.TrimSpace(p.Ref) != "" }

// HasXY reports whether explicit coordinates were supplied.
func (p MousePoint) HasXY() bool { return p.X != nil && p.Y != nil }

// ClickButtonOptions drives a single click with an explicit mouse button and
// click count, covering left/right/middle button context menus and
// single/double/triple click selection. The target is a ref or x,y point.
type ClickButtonOptions struct {
	MousePoint
	Button     string `json:"button,omitempty"`
	ClickCount int    `json:"click_count,omitempty"`
}

// MouseButtonOptions drives a decomposed press-and-hold (mouse_down) or release
// (mouse_up) at a ref or x,y point with an explicit mouse button.
type MouseButtonOptions struct {
	MousePoint
	Button string `json:"button,omitempty"`
}

// DragOptions presses at a source point, moves to a target point in a number of
// intermediate steps, then releases — covering sliders/range inputs,
// drag-and-drop reorder, and canvas/map panning. Each endpoint is a ref or x,y.
type DragOptions struct {
	From   MousePoint `json:"from"`
	To     MousePoint `json:"to"`
	Steps  int        `json:"steps,omitempty"`
	Button string     `json:"button,omitempty"`
}

type TraceEntry struct {
	Action     string `json:"action"`
	Ref        string `json:"ref,omitempty"`
	Text       string `json:"text,omitempty"`
	Value      string `json:"value,omitempty"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Timestamp  string `json:"timestamp"`
}

type TraceResult struct {
	Entries []TraceEntry `json:"entries"`
	Count   int          `json:"count"`
}

// IsDefaultLeftSingleRefClick reports whether a browser_click call is a plain
// left single-click on a ref (no explicit button/count/coordinates), which can
// keep the optimized in-page click path. Shared by MCP and HTTP servers.
func IsDefaultLeftSingleRefClick(button string, clickCount int, ref string, x, y *float64) bool {
	if x != nil || y != nil {
		return false
	}
	if ref == "" {
		return false
	}
	if clickCount > 1 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(button)) {
	case "", "left":
		return true
	default:
		return false
	}
}
