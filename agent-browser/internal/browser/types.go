package browser

import (
	"strings"
	"time"

	"github.com/revitt/agent-browser/internal/snapshot"
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
}

type OpenResult struct {
	Tab Tab `json:"tab"`
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
	OK       bool             `json:"ok"`
	Steps    []PlanStepResult `json:"steps"`
	FailedAt *int             `json:"failed_at,omitempty"`
	Error    string           `json:"error,omitempty"`
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
	OK      bool              `json:"ok"`
	Steps   []BatchStepResult `json:"steps"`
	Error   string            `json:"error,omitempty"`
	TabID   string            `json:"tab_id,omitempty"`
	URL     string            `json:"url,omitempty"`
	Title   string            `json:"title,omitempty"`
	Focus   string            `json:"focus,omitempty"`
	Changed []string          `json:"changed,omitempty"`
	Version int64             `json:"version,omitempty"`
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
