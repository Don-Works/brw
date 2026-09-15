package browser

import "github.com/Don-Works/brw/internal/snapshot"

// Remedies a transport offers when it is handed a ref inside a cross-origin
// iframe. The text differs because the way out differs: the bridge has no way to
// reach the frame at all, while direct CDP reaches it for a click.
const (
	// BridgeCrossOriginRemedy is the extension bridge's way out. Its per-tab
	// chrome.debugger session speaks to one target, and an out-of-process iframe
	// is a target of its own.
	BridgeCrossOriginRemedy = "the extension bridge drives one tab target and cannot open a session on a frame's own target: use the direct-CDP backend, or brw_click_xy at the frame box from brw_snapshot include_frames"
	// DirectCrossOriginRemedy is direct CDP's way out: it attaches to the frame
	// target for a click, and nothing else routes there yet.
	DirectCrossOriginRemedy = "only brw_click routes into a cross-origin iframe on this backend: click the ref, or brw_click_xy at the frame box from brw_snapshot include_frames"
	// GenericCrossOriginRemedy is for the transport-agnostic entry points, which
	// run the same code whichever controller is installed and so cannot name one
	// transport's way out over the other's.
	GenericCrossOriginRemedy = "brw_click reaches that frame on the direct-CDP backend; otherwise brw_click_xy at the frame box from brw_snapshot include_frames"
)

// GuardCrossOriginRefs refuses, BY NAME, any ref that points inside a
// cross-origin iframe. action names the verb for the message; remedy is the
// transport's way out.
//
// The refusal is the point. Every one of these verbs resolves a ref against the
// TOP document, where an f<i>:<inner> ref does not exist — so without the guard
// the verb either reports "ref not found, re-snapshot" (advice that can never
// help, because re-snapshotting mints the same ref) or, worse, falls back to
// something adjacent and reports success for an action that landed elsewhere.
func GuardCrossOriginRefs(action, remedy string, refs ...string) error {
	for _, ref := range refs {
		if snapshot.IsCrossOriginElementRef(ref) {
			return snapshot.CrossOriginRefError(ref, action, remedy)
		}
	}
	return nil
}

// PlanStepRefs returns every element ref a plan step can carry.
func PlanStepRefs(steps []PlanStep) []string {
	refs := make([]string, 0, len(steps)*2)
	for _, step := range steps {
		refs = append(refs, step.Ref, step.ExpectRef)
	}
	return refs
}

// BatchStepRefs returns every element ref a batch step can carry.
func BatchStepRefs(steps []BatchStep) []string {
	refs := make([]string, 0, len(steps)*3)
	for _, step := range steps {
		refs = append(refs, step.Ref, step.AssertRef)
		if step.Assertion != nil {
			refs = append(refs, step.Assertion.Ref)
		}
	}
	return refs
}

// ControllerRefMethods names every Controller method that can be handed an
// element ref, and says where the refs live in its arguments.
//
// The classification is a table because the guard has to hold for the CAPABILITY
// — "this ref is in another document" — and not for two conveniently remembered
// verb names. A test enumerates the Controller and capability-interface method
// sets against this map and ControllerRefFreeMethods and fails on any member of
// neither, so a new ref-taking verb cannot be added without deciding what it does
// with a ref inside a cross-origin iframe.
//
// The two first-party transports are guarded here. The upstream-HTTP controller
// is not: it forwards each verb to a daemon that is, so the refusal comes back
// over the wire rather than being decided twice.
var ControllerRefMethods = map[string][]string{
	"Click":               {"ref"},
	"ClickButton":         {"opts.Ref"},
	"MouseDown":           {"opts.Ref"},
	"MouseUp":             {"opts.Ref"},
	"Drag":                {"opts.From.Ref", "opts.To.Ref"},
	"Hover":               {"ref"},
	"Type":                {"ref"},
	"Fill":                {"opts.Ref"},
	"UploadFile":          {"opts.Ref", "opts.ClickRef"},
	"Select":              {"ref"},
	"ScreenshotAnnotated": {"opts.Ref"},
	"ScreenshotElement":   {"ref"},
	"AssertVisible":       {"ref"},
	"AssertText":          {"ref"},
	"AssertValue":         {"ref"},
	"AssertValueContains": {"ref"},
	"AssertHidden":        {"ref"},
	"CommitField":         {"ref"},
	"ExecutePlan":         {"steps[].Ref", "steps[].ExpectRef"},
	"ExecuteBatch":        {"steps[].Ref", "steps[].AssertRef", "steps[].Assertion.Ref"},
	// A wait carries its ref inside the condition string ("ref:e12"), which is
	// why it is here and not with the ref-free verbs.
	"WaitFor":        {"condition ref:<ref>"},
	"WaitForOutcome": {"condition ref:<ref>"},
	"Focus":          {"ref"},
	"Assert":         {"req.Ref"},
}

// ControllerRefFreeMethods names every Controller method that takes no element
// ref: it addresses a tab, a URL, a coordinate, a query or nothing at all, so
// there is no ref for a cross-origin frame to hide in. brw_click_xy is here on
// purpose — a coordinate in the top-level viewport is exactly how an agent acts
// inside a frame it cannot address by ref.
var ControllerRefFreeMethods = map[string]bool{
	"Open":            true,
	"OpenInGroup":     true,
	"OpenIncognito":   true,
	"CloseContext":    true,
	"ListTabs":        true,
	"ListTabGroups":   true,
	"FocusTab":        true,
	"CloseTab":        true,
	"GroupTabs":       true,
	"UngroupTabs":     true,
	"EmulateDevice":   true,
	"Read":            true,
	"ReadData":        true,
	"Snapshot":        true,
	"Find":            true,
	"FindLive":        true,
	"ClickText":       true,
	"Navigate":        true,
	"NavigateTo":      true,
	"Press":           true,
	"Scroll":          true,
	"Screenshot":      true,
	"Evaluate":        true,
	"NetworkRequests": true,
	"NetworkCapture":  true,
	"ReplayRequest":   true,
	"Cookies":         true,
	"Cancel":          true,
	"Observe":         true,
	"ConsoleMessages": true,
	"Downloads":       true,
	"ClickXY":         true,
	"WindowBounds":    true,
	"ResizeWindow":    true,
	"GetTrace":        true,
	"ClearTrace":      true,
	"Notify":          true,
	// Optional transport capabilities (controller.go) that carry no element ref.
	"Dialog":           true,
	"Route":            true,
	"CheckRouteReplay": true,
	"Clipboard":        true,
	"KeyDown":          true,
	"KeyUp":            true,
	"PushState":        true,
	"SessionState":     true,
	"ReadWindow":       true,
	"ActiveTabID":      true,
	"DocumentIdentity": true,
}
