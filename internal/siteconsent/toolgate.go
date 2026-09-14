package siteconsent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The gate lives here, not in either server, because brw serves the same
// controller over MCP and over HTTP. A rule enforced on one surface is a silent
// bypass through the other, which is the shape of the hole the navigation
// guardrail's own comment warns about.
//
// Both surfaces name operations the same way (brw_open, brw_click, ...), so one
// table decides for both.

// ToolTarget says where a tool's origin comes from.
type ToolTarget int

const (
	// TargetURL: the origin is the destination in the call's own url argument.
	TargetURL ToolTarget = iota
	// TargetPage: the origin is whatever the tab is currently showing.
	TargetPage
)

// ToolRule is one tool's consent requirement.
type ToolRule struct {
	Scope  Scope
	Target ToolTarget
}

// ToolRules is the whole mapping from operation to consent requirement. It is a
// table for the same reason the risk classification is: a gate spread across
// twenty call sites is a gate with a hole in it, and nobody can review it.
//
// Where the two scopes are enforced is deliberate and is NOT symmetrical:
//
//   - read is enforced on the NAVIGATION that reaches an origin, not on each
//     later read of the page. An origin can only be looked at after brw has been
//     steered there, and every path that steers it is in this table, so gating
//     the arrival gates the reading. The alternative - resolving the live tab URL
//     before every snapshot - costs a transport round trip per read.
//   - act is enforced on the ACTION, against the tab's live URL, because between
//     the navigation and the click the page may have moved.
var ToolRules = map[string]ToolRule{
	// Navigation and daemon-side fetches: the destination is the origin.
	"brw_open":           {ScopeRead, TargetURL},
	"brw_open_incognito": {ScopeRead, TargetURL},
	"brw_navigate_to":    {ScopeRead, TargetURL},
	"brw_read_url":       {ScopeRead, TargetURL},
	"brw_replay_request": {ScopeRead, TargetURL},
	// Cookies are read for one URL's origin, which is a read of that site.
	"brw_cookies": {ScopeRead, TargetURL},
	// Authenticating hands an HTTP credential to an origin. That is not reading
	// it, so it needs the act scope.
	"brw_authenticate": {ScopeAct, TargetURL},

	// State-changing page actions, and script execution, which can do anything
	// an action can.
	"brw_click":       {ScopeAct, TargetPage},
	"brw_click_text":  {ScopeAct, TargetPage},
	"brw_click_xy":    {ScopeAct, TargetPage},
	"brw_type":        {ScopeAct, TargetPage},
	"brw_fill":        {ScopeAct, TargetPage},
	"brw_select":      {ScopeAct, TargetPage},
	"brw_press":       {ScopeAct, TargetPage},
	"brw_key_down":    {ScopeAct, TargetPage},
	"brw_key_up":      {ScopeAct, TargetPage},
	"brw_drag":        {ScopeAct, TargetPage},
	"brw_mouse_down":  {ScopeAct, TargetPage},
	"brw_mouse_up":    {ScopeAct, TargetPage},
	"brw_upload_file": {ScopeAct, TargetPage},
	"brw_evaluate":    {ScopeAct, TargetPage},
	"brw_commit":      {ScopeAct, TargetPage},
	"brw_pushstate":   {ScopeAct, TargetPage},
	"brw_navigate":    {ScopeAct, TargetPage},
	"brw_dialog":      {ScopeAct, TargetPage},
	"brw_clipboard":   {ScopeAct, TargetPage},
}

// SequenceTools are the operations whose payload is a list of steps, each of
// which is gated on its own.
var SequenceTools = map[string]bool{"brw_plan": true, "brw_batch": true}

// ActStepActions lists the plan/batch step actions that change page state. A
// sequence runner is a tool surface of its own, so it needs the same table
// rather than the same conditionals written again.
//
// Read-only steps (snapshot, find, read, wait, scroll, hover, screenshot) are
// deliberately absent: they change nothing, and the origin they read was already
// gated at the navigation that reached it.
var ActStepActions = map[string]bool{
	"click":       true,
	"click_text":  true,
	"type":        true,
	"fill":        true,
	"select":      true,
	"press":       true,
	"upload_file": true,
	"drag":        true,
	"commit":      true,
	"evaluate":    true,
	"navigate":    true,
}

// Probe reads the argument fields consent needs out of any call, without
// disturbing each tool's own decoding.
type Probe struct {
	URL   string      `json:"url"`
	Ref   string      `json:"ref"`
	Text  string      `json:"text"`
	Value string      `json:"value"`
	Query string      `json:"query"`
	Steps []StepProbe `json:"steps"`
}

// StepProbe is one plan/batch step's consent-relevant fields.
type StepProbe struct {
	Action string `json:"action"`
	URL    string `json:"url"`
	Ref    string `json:"ref"`
	Text   string `json:"text"`
	Value  string `json:"value"`
	Query  string `json:"query"`
}

// ParseProbe reads a call's arguments. Arguments that do not decode into the
// probe shape yield an empty probe: the call is still gated, and its own handler
// rejects the arguments afterwards.
func ParseProbe(args []byte) Probe {
	var probe Probe
	if len(args) > 0 {
		_ = json.Unmarshal(args, &probe)
	}
	return probe
}

// URLOriginCheck is one origin named by a call's own arguments, and the scope it
// has to be consented at.
type URLOriginCheck struct {
	URL   string
	Scope Scope
}

// URLOriginChecks lists the origins a call names directly: the destination it
// would steer the browser to, the URL the daemon would fetch itself, or the
// origin it would hand a credential to.
func URLOriginChecks(tool string, probe Probe) []URLOriginCheck {
	var checks []URLOriginCheck
	if rule, ok := ToolRules[tool]; ok && rule.Target == TargetURL && probe.URL != "" {
		checks = append(checks, URLOriginCheck{probe.URL, rule.Scope})
	}
	// brw_upload_file can name a URL to fetch the file FROM, which reaches the
	// network from the daemon rather than from the page. Its page-scoped rule
	// covers the upload itself; this covers the fetch.
	if tool == "brw_upload_file" && probe.URL != "" {
		checks = append(checks, URLOriginCheck{probe.URL, ScopeRead})
	}
	if SequenceTools[tool] {
		for _, step := range probe.Steps {
			if strings.EqualFold(step.Action, "open") && step.URL != "" {
				checks = append(checks, URLOriginCheck{step.URL, ScopeRead})
			}
		}
	}
	return checks
}

// PageAction pairs a classification request with the ref it came from, so the
// label can be filled in from the snapshot brw already returned.
type PageAction struct {
	Request ActionRequest
	Ref     string
}

// PageActions returns the classification requests for a call and whether it acts
// on the page at all.
func PageActions(tool string, probe Probe) ([]PageAction, bool) {
	if SequenceTools[tool] {
		var out []PageAction
		acts := false
		for _, step := range probe.Steps {
			if !ActStepActions[strings.ToLower(strings.TrimSpace(step.Action))] {
				continue
			}
			acts = true
			out = append(out, PageAction{
				Request: ActionRequest{
					Tool:   tool + " step " + strings.ToLower(step.Action),
					Label:  step.Query,
					Text:   step.Text,
					Fields: nonEmpty(step.Query),
				},
				Ref: step.Ref,
			})
		}
		return out, acts
	}
	rule, ok := ToolRules[tool]
	if !ok || rule.Target != TargetPage {
		return nil, false
	}
	action := PageAction{
		Request: ActionRequest{Tool: tool, Label: probe.Query, Text: probe.Text},
		Ref:     probe.Ref,
	}
	if tool == "brw_fill" || tool == "brw_type" {
		// For a fill it is the FIELD that carries the risk, not the button: the
		// query names the field. The typed VALUE is not classified - a card
		// number typed into a search box is not a personal-data submission, and
		// classifying the value would put it in an error string.
		action.Request.Fields = nonEmpty(probe.Query)
		action.Request.Text = ""
	}
	return []PageAction{action}, true
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// PageOriginFunc resolves the origin the tab a call targets is currently
// showing. It must fail rather than answer "" when it cannot tell: an action
// allowed because brw did not know where it was landing is the failure this
// whole surface exists to stop.
type PageOriginFunc func() (string, error)

// LabelFunc returns the accessible name brw last reported for a ref, or "" when
// it has never described that ref.
type LabelFunc func(ref string) string

// CheckTool is the single gate a call passes through, on every surface.
//
// It must run BEFORE the call is dispatched, so a refusal means nothing ran.
// A nil guard, or one with no store, passes everything.
func (g *Guard) CheckTool(tool string, args []byte, pageOrigin PageOriginFunc, label LabelFunc) error {
	if !g.Enabled() {
		return nil
	}
	probe := ParseProbe(args)
	for _, check := range URLOriginChecks(tool, probe) {
		if err := g.Authorize(check.URL, check.Scope); err != nil {
			return err
		}
	}
	actions, acts := PageActions(tool, probe)
	if !acts {
		return nil
	}
	if pageOrigin == nil {
		return fmt.Errorf("site consent cannot decide: this surface cannot report the tab's current origin, so %s has nothing to check against", tool)
	}
	origin, err := pageOrigin()
	if err != nil {
		return err
	}
	if err := g.Authorize(origin, ScopeAct); err != nil {
		return err
	}
	if !g.ConfirmActions() {
		return nil
	}
	for _, action := range actions {
		action.Request.Origin = origin
		if action.Request.Label == "" && action.Ref != "" && label != nil {
			action.Request.Label = label(action.Ref)
		}
		if err := g.CheckAction(action.Request); err != nil {
			return err
		}
	}
	return nil
}
