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

// WindowReader is an optional transport capability that applies page-content
// filtering on the browser host. The upstream HTTP controller implements it so
// a 20 KiB MCP read does not transfer and materialize an entire megabyte-scale
// document before the outer process slices it.
type WindowReader interface {
	ReadWindow(context.Context, readability.ReadOptions) (readability.PageRead, error)
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
