package mcp

// Cross-origin ref classification, at the tool surface.
//
// internal/extensionbridge/cross_origin_ref_test.go enumerates the Controller
// interface and the optional capability interfaces, so every verb that reaches a
// transport by one of those methods has to say what it does with a ref inside a
// cross-origin iframe. That covers most tools and misses the ones whose ref never
// becomes a Controller argument: brw_get hands its target to Evaluate as
// JavaScript, brw_highlight goes to the devtools observer, and brw_artifact_capture
// goes to the artifact service. Those refs reach the page all the same, and an
// unguarded one resolves against the TOP document, where it does not exist — so
// the tool answers "no element matched", advice that can never help because
// re-snapshotting mints the same ref.
//
// The classification is therefore ALSO taken from the catalogue: every tool whose
// schema declares a string ref parameter is listed here with what happens to a
// cross-origin ref, and TestEveryToolTakingARefIsClassifiedForCrossOriginRefs
// fails on one that is not.

// refToolDisposition says what a tool does when handed an f<i>:<ref>.
type refToolDisposition int

const (
	// refToolGuardedAtTransport: the ref becomes a Controller (or capability)
	// argument, and GuardCrossOriginRefs refuses it in both backends. The reflect
	// test in internal/extensionbridge is what holds that.
	refToolGuardedAtTransport refToolDisposition = iota
	// refToolGuardedHere: the ref never becomes a Controller argument, so the
	// refusal is applied in callTool, at the one place that knows the argument is
	// a ref.
	refToolGuardedHere
	// refToolRoutesIntoTheFrame: the tool reaches the frame's own document and
	// acts there, so refusing would deny the capability it advertises.
	refToolRoutesIntoTheFrame
	// refToolNamesTheFrame: the parameter takes a frame ref BY DESIGN — f<i> or
	// f<i>:<ref> — and reports the frame rather than acting inside it.
	refToolNamesTheFrame
)

// refTakingTools maps each tool advertising a string ref parameter to what it
// does with a cross-origin one. The reason is the argument a reviewer has with a
// tool put in the wrong bucket.
var refTakingTools = map[string]struct {
	Disposition refToolDisposition
	Reason      string
}{
	"brw_click":              {refToolRoutesIntoTheFrame, "direct CDP attaches a session to the frame's own target and clicks there; the extension bridge refuses by name"},
	"brw_hover":              {refToolGuardedAtTransport, "Hover resolves against the top document"},
	"brw_type":               {refToolGuardedAtTransport, "Type resolves against the top document"},
	"brw_fill":               {refToolGuardedAtTransport, "Fill resolves against the top document"},
	"brw_select":             {refToolGuardedAtTransport, "Select resolves against the top document"},
	"brw_focus":              {refToolGuardedAtTransport, "Focus resolves against the top document"},
	"brw_commit":             {refToolGuardedAtTransport, "CommitField resolves against the top document"},
	"brw_mouse_down":         {refToolGuardedAtTransport, "MouseDown resolves against the top document"},
	"brw_mouse_up":           {refToolGuardedAtTransport, "MouseUp resolves against the top document"},
	"brw_upload_file":        {refToolGuardedAtTransport, "UploadFile resolves both its ref and its click_ref against the top document"},
	"brw_screenshot":         {refToolGuardedAtTransport, "a ref makes this an annotated crop, which goes through ScreenshotAnnotated"},
	"brw_screenshot_element": {refToolGuardedAtTransport, "ScreenshotElement resolves against the top document"},
	"brw_assert":             {refToolGuardedAtTransport, "Assert resolves against the top document"},
	"brw_assert_visible":     {refToolGuardedAtTransport, "AssertVisible resolves against the top document"},
	"brw_assert_hidden":      {refToolGuardedAtTransport, "AssertHidden resolves against the top document"},
	"brw_assert_text":        {refToolGuardedAtTransport, "AssertText resolves against the top document"},
	"brw_assert_value":       {refToolGuardedAtTransport, "AssertValue resolves against the top document"},
	"brw_frame":              {refToolNamesTheFrame, "the target IS a frame ref: the element half is dropped and the frame's box is reported"},
	"brw_get":                {refToolGuardedHere, "the target reaches the transport as JavaScript through Evaluate, which is ref-free"},
	"brw_highlight":          {refToolGuardedHere, "the overlay is drawn in the top document by the devtools observer, not through a Controller ref method"},
	"brw_artifact_capture":   {refToolGuardedHere, "the ref reaches the artifact service, which captures from the top document"},
	// Added with the 2026-09 parity wave. Each ref becomes a capability-method
	// argument and GuardCrossOriginRefs refuses it in both backends.
	"brw_touch":  {refToolGuardedAtTransport, "Touch resolves both its ref and its to_ref against the top document"},
	"brw_scroll": {refToolGuardedAtTransport, "Scrolling to a target resolves the ref or selector against the top document"},
	"brw_react":  {refToolGuardedAtTransport, "React inspection resolves its target against the top document"},
	"brw_check":  {refToolGuardedAtTransport, "Check resolves its ref against the top document and refuses a cross-origin one by name"},
}
