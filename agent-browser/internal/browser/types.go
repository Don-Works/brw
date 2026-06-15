package browser

import (
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
