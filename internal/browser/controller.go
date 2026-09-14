package browser

import (
	"context"
	"time"

	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/snapshot"
)

// Controller is the unified interface for browser control transports. Every
// transport (direct-CDP Manager, extension Bridge, upstream HTTP proxy)
// implements this interface. MCP and HTTP servers accept a Controller to remain
// transport-agnostic.
type Controller interface {
	Open(context.Context, string) (OpenResult, error)
	OpenInGroup(context.Context, string, TabGroupOptions) (OpenResult, error)
	OpenIncognito(context.Context, string) (OpenResult, error)
	CloseContext(context.Context, string) error
	ListTabs(context.Context) ([]Tab, error)
	ListTabGroups(context.Context) ([]TabGroup, error)
	FocusTab(context.Context, string) error
	CloseTab(context.Context, string) error
	GroupTabs(context.Context, []string, TabGroupOptions) error
	UngroupTabs(context.Context, []string) error
	EmulateDevice(context.Context, DeviceEmulationOptions) (DeviceEmulationResult, error)
	Read(context.Context) (readability.PageRead, error)
	ReadData(context.Context) (snapshot.StructuredData, error)
	Snapshot(context.Context, snapshot.SnapshotOptions) (snapshot.PageSnapshot, error)
	Find(context.Context, snapshot.FindOptions) (snapshot.FindResult, error)
	Click(context.Context, string) (ActionResult, error)
	ClickText(context.Context, snapshot.ClickTextOptions) (ActionResult, error)
	Navigate(context.Context, string) (ActionResult, error)
	NavigateTo(context.Context, string) (ActionResult, error)
	ClickButton(context.Context, ClickButtonOptions) (ActionResult, error)
	MouseDown(context.Context, MouseButtonOptions) (ActionResult, error)
	MouseUp(context.Context, MouseButtonOptions) (ActionResult, error)
	Drag(context.Context, DragOptions) (ActionResult, error)
	Hover(context.Context, string) (ActionResult, error)
	Type(context.Context, string, string) (ActionResult, error)
	Fill(context.Context, snapshot.FillOptions) (ActionResult, error)
	UploadFile(context.Context, snapshot.UploadOptions) (ActionResult, error)
	Select(context.Context, string, string) (ActionResult, error)
	Press(context.Context, string) (ActionResult, error)
	Scroll(context.Context, string) (ActionResult, error)
	Screenshot(context.Context) (Screenshot, error)
	ScreenshotAnnotated(context.Context, AnnotatedScreenshotOptions) (AnnotatedScreenshot, error)
	ScreenshotElement(context.Context, string) (Screenshot, error)
	WaitFor(context.Context, string, time.Duration) error
	Evaluate(context.Context, string) (any, error)
	NetworkRequests(context.Context, string) ([]NetworkRequest, error)
	NetworkCapture(context.Context, string) ([]snapshot.CapturedRequest, error)
	ReplayRequest(context.Context, ReplayRequestParams) (snapshot.ReplayResult, error)
	Cookies(context.Context, CookieParams) (CookieResult, error)
	ExecutePlan(context.Context, []PlanStep) (PlanResult, error)
	ExecuteBatch(context.Context, []BatchStep) (BatchResult, error)
	Cancel(context.Context, string) (CancelResult, error)
	Observe(context.Context) (ObserveResult, error)
	ConsoleMessages(context.Context) ([]ConsoleMessage, error)
	Downloads(context.Context) (DownloadsResult, error)
	ClickXY(context.Context, float64, float64) (snapshot.ClickXYResult, error)
	WindowBounds(context.Context) (snapshot.WindowBoundsResult, error)
	ResizeWindow(context.Context, WindowResizeOptions) (WindowResizeResult, error)
	GetTrace() TraceResult
	ClearTrace()
	AssertVisible(context.Context, string, time.Duration) error
	AssertText(context.Context, string, string, time.Duration) error
	AssertValue(context.Context, string, string, time.Duration) error
	AssertHidden(context.Context, string, time.Duration) error
	CommitField(context.Context, string) error
	Notify(context.Context, NotifyOptions) (NotifyResult, error)
}

// Sources a wait can resolve from. "event" means a CDP (or, on the extension
// bridge, an extension-side) subscription delivered the signal; "script" means
// the answer came from an awaited in-page promise; "poll" means the transport
// had to re-ask on a timer because it has no subscription for that signal.
const (
	WaitResolvedByEvent  = "event"
	WaitResolvedByScript = "script"
	WaitResolvedByPoll   = "poll"
)

// WaitOutcome reports how one wait resolved. ResolvedBy names the source of
// truth and Wakeups counts how many times the wait re-evaluated: an event-driven
// resolve wakes once for the event that satisfied it, while anything polling
// wakes on a cadence and its count grows with how long the wait lasted.
type WaitOutcome struct {
	OK         bool   `json:"ok"`
	Condition  string `json:"condition"`
	ResolvedBy string `json:"resolved_by,omitempty"`
	WaitedMS   int64  `json:"waited_ms"`
	Wakeups    int    `json:"wakeups"`
}

// WaitObserver is the optional capability of reporting a wait's outcome rather
// than just its error. Both first-party transports implement it; the interface
// exists so an upstream HTTP controller that has not been upgraded degrades to
// the plain WaitFor instead of failing to compile.
type WaitObserver interface {
	WaitForOutcome(ctx context.Context, condition string, timeout time.Duration) (WaitOutcome, error)
}

// DialogController is an optional transport capability for JavaScript dialogs
// (alert / confirm / prompt / beforeunload). Both first-party transports
// implement it; the interface exists so an upstream HTTP controller that has not
// been upgraded degrades to "unsupported" instead of failing to compile.
//
// Every transport ANSWERS dialogs whether or not this capability is present,
// because an unanswered dialog blocks the renderer. The capability is about
// choosing the answer in advance and seeing what happened.
type DialogController interface {
	Dialog(context.Context, DialogOptions) (DialogResult, error)
}

// RouteController is an optional transport capability for request interception:
// answering a matching request from a mock instead of the network.
type RouteController interface {
	Route(context.Context, RouteOptions) (RouteResult, error)
}

// RouteReplayer is the half of RouteController that answers from a recorded
// HAR. A transport implements it to say in advance whether it can, so the
// surface holding the artifact store can refuse with the transport's own named
// error instead of reading and decoding a recording the transport will reject
// anyway — and so a bad artifact id on that transport reports the capability
// gap rather than an artifact error.
type RouteReplayer interface {
	CheckRouteReplay() error
}

// ClipboardController is an optional transport capability for the system
// clipboard. Direct-CDP only: reading the clipboard needs a permission granted
// through the browser-level Browser.setPermission command, which is not
// reachable from the extension bridge's per-tab chrome.debugger session.
type ClipboardController interface {
	Clipboard(context.Context, ClipboardOptions) (ClipboardResult, error)
}

// KeyHoldController is an optional transport capability for press-and-hold keys:
// a keydown whose keyup comes later, so Ctrl+drag and Shift+click-range are
// expressible. Direct-CDP only, because holding a key means the transport must
// stamp the modifier mask onto every input event it dispatches afterwards.
type KeyHoldController interface {
	KeyDown(context.Context, KeyHoldOptions) (KeyHoldResult, error)
	KeyUp(context.Context, KeyHoldOptions) (KeyHoldResult, error)
}

// HistoryController is an optional transport capability for same-document
// history changes (pushState/replaceState), which drive a client-side router
// without loading a new document. Direct-CDP only.
type HistoryController interface {
	PushState(context.Context, HistoryStateOptions) (HistoryStateResult, error)
}

// ElementFocuser is an optional transport capability that gives one element the
// document focus by ref and reports the page afterwards. Both first-party
// transports implement it; the interface exists so an upstream HTTP controller
// that has not been upgraded degrades to "unsupported" instead of failing to
// compile.
//
// It returns an ActionResult rather than an error alone because focus is an
// action tool, and every action tool answers with the post-action observation —
// an agent that focuses a field has to be able to see from the result whether
// focus landed and what the page did about it. FocusRef stays as the narrow
// recipe-internal form.
type ElementFocuser interface {
	Focus(context.Context, string) (ActionResult, error)
}

// WindowReader is an optional transport capability that applies page-content
// filtering on the browser host. The upstream HTTP controller implements it so
// a 20 KiB MCP read does not transfer and materialize an entire megabyte-scale
// document before the outer process slices it.
type WindowReader interface {
	ReadWindow(context.Context, readability.ReadOptions) (readability.PageRead, error)
}

// ActiveTabReporter names the tab an untargeted page call lands in. All three
// controllers brwd can install implement it, each asserted at compile time next
// to its implementation, because the caller that needs it needs it on every
// transport rather than on whichever one it was written against.
//
// It is deliberately not ResolveActiveTabID (internal/mcp): that one RESOLVES a
// tab and pins it for the rest of a tool call, which is why only the extension
// bridge implements it — the bridge is where per-sub-call re-resolution was
// costing round trips. This one only reports, opens nothing and pins nothing, so
// a report can name the tab it ran in without changing what anything else
// targets. A transport with no tab open answers with an error rather than an
// empty string, so a caller can tell "no tab" from "not asked".
type ActiveTabReporter interface {
	ActiveTabID(context.Context) (string, error)
}

// DocumentIdentity is an opaque, main-frame document identity plus its exact
// security origin. ID must remain stable across same-document history changes
// (pushState/replaceState/hash changes) and change whenever Chrome commits a
// replacement document, including a reload or same-origin navigation.
//
// The value is an internal capture guard. It is deliberately not exposed by
// the MCP/HTTP artifact APIs or included in error messages.
type DocumentIdentity struct {
	ID     string
	Origin string
}

// DocumentIdentityProvider is an optional transport capability used only by
// deterministic recipe-scoped artifact capture. Manual artifact capture does
// not pay for this probe. A recipe capture fails closed when its transport
// cannot provide an exact main-document identity.
type DocumentIdentityProvider interface {
	DocumentIdentity(context.Context) (DocumentIdentity, error)
}

type sensitiveActionContextKey struct{}
type allowedOriginsContextKey struct{}

// WithSensitiveAction marks one browser actuation as carrying a caller-declared
// secret. Transport implementations must still perform the action, but omit its
// text/value from their replayable trace even when the page field itself is not
// recognizably credential-bearing.
func WithSensitiveAction(ctx context.Context) context.Context {
	return context.WithValue(ctx, sensitiveActionContextKey{}, true)
}

func RedactTraceEntry(ctx context.Context, entry TraceEntry) TraceEntry {
	if ctx != nil {
		redact, _ := ctx.Value(sensitiveActionContextKey{}).(bool)
		if redact {
			entry.Text = ""
			entry.Value = ""
			entry.Redacted = true
		}
	}
	return entry
}

// WithAllowedOrigins carries a deterministic recipe's exact page-origin
// boundary into lower-level capture code. Ordinary model-driven browser calls
// do not set it; artifact capture then keeps its existing unrestricted behavior.
func WithAllowedOrigins(ctx context.Context, origins []string) context.Context {
	return context.WithValue(ctx, allowedOriginsContextKey{}, append([]string(nil), origins...))
}

// AllowedOriginsFromContext returns a defensive copy so transport code cannot
// mutate the runner's reviewed allowlist through a shared backing array.
func AllowedOriginsFromContext(ctx context.Context) ([]string, bool) {
	if ctx == nil {
		return nil, false
	}
	origins, ok := ctx.Value(allowedOriginsContextKey{}).([]string)
	if !ok || len(origins) == 0 {
		return nil, false
	}
	return append([]string(nil), origins...), true
}
