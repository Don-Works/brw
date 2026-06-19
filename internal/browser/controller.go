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
	Read(context.Context) (readability.PageRead, error)
	ReadData(context.Context) (snapshot.StructuredData, error)
	Snapshot(context.Context, snapshot.SnapshotOptions) (snapshot.PageSnapshot, error)
	Find(context.Context, snapshot.FindOptions) (snapshot.FindResult, error)
	Click(context.Context, string) (ActionResult, error)
	ClickText(context.Context, snapshot.ClickTextOptions) (ActionResult, error)
	Navigate(context.Context, string) (ActionResult, error)
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
	GetTrace() TraceResult
	ClearTrace()
	AssertVisible(context.Context, string, time.Duration) error
	AssertText(context.Context, string, string, time.Duration) error
	AssertValue(context.Context, string, string, time.Duration) error
	AssertHidden(context.Context, string, time.Duration) error
	CommitField(context.Context, string) error
	Notify(context.Context, NotifyOptions) (NotifyResult, error)
}
