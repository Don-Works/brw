package siteconsent

import (
	"encoding/json"
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
	// TargetURL: the origin is the destination named in the call's own
	// arguments.
	TargetURL ToolTarget = iota
	// TargetPage: the origin is whatever the tab is currently showing.
	TargetPage
)

// DestinationField is an argument a tool names its destination in, spelled the
// way that tool spells it. Naming the field is what keeps the table honest: a
// gate that reads only "url" never fires for a tool addressed by "origin" or
// "domain", and a rule whose declared field the probe cannot read is a row that
// looks like enforcement and is not.
type DestinationField string

const (
	// FieldURL is an absolute (or bare-host) URL argument.
	FieldURL DestinationField = "url"
	// FieldOrigin is a scheme://host[:port] argument.
	FieldOrigin DestinationField = "origin"
	// FieldDomain is a bare host, the way a cookie names one.
	FieldDomain DestinationField = "domain"
	// FieldOrigins is a list of entries each carrying an origin.
	FieldOrigins DestinationField = "origins"
)

// Escalation raises a rule's scope to act when one of the call's own arguments
// says the call writes. One tool is often both halves: listing a site's cookies
// reads it, writing one does not, and both arrive as brw_cookies.
type Escalation struct {
	// Field is the argument that decides, either "action" or "method".
	Field string
	// Values are the lowercase values that mean "this writes".
	Values []string
}

// ToolRule is one tool's consent requirement.
type ToolRule struct {
	// Scope is what this tool needs at its target origin.
	Scope Scope
	// Target says which origin that is.
	Target ToolTarget
	// Fields are the arguments a TargetURL tool's destination arrives in. At
	// least one is required for a TargetURL rule; without one the rule never
	// produces a check.
	Fields []DestinationField
	// Fetches name arguments carrying a URL the DAEMON retrieves itself rather
	// than the page. Those always need read, whatever the tool's own scope is.
	Fetches []DestinationField
	// PageWhenUnaddressed makes a TargetURL call whose destination arguments are
	// all absent fall back to the tab's live origin. Without it, leaving the
	// optional argument out is the whole bypass.
	PageWhenUnaddressed bool
	// Escalate raises Scope to act for the calls its Field/Values name.
	Escalate *Escalation
	// ScriptCondition marks a tool whose "condition" argument can carry page
	// script (brw's "fn:" predicates). Script is an action: it does anything
	// brw_evaluate does, so a scripted condition needs act.
	ScriptCondition bool
}

// ToolRules is the whole mapping from operation to consent requirement. It is a
// table for the same reason the risk classification is: a gate spread across
// twenty call sites is a gate with a hole in it, and nobody can review it.
//
// The table is exhaustive over the tool catalogue by construction. Every
// registered tool is either here or in UngatedTools with the reason it needs no
// grant, and TestEveryToolIsClassifiedForConsent fails on one that is in
// neither - a new tool cannot ship ungated by being forgotten.
//
// Where the two scopes are enforced:
//
//   - read is enforced on the NAVIGATION that reaches an origin AND on the
//     reads of the page once there. The navigation check alone was not enough:
//     on the extension bridge brw attaches to a Chrome the user is already
//     driving, so tabs exist that brw never opened, and a server redirect
//     produces a document brw never asked for. Both are pages no grant was ever
//     given for, and reading them is what this scope exists to gate.
//   - act is enforced on the ACTION, against the tab's live URL, because
//     between the navigation and the click the page may have moved.
var ToolRules = map[string]ToolRule{
	// Navigation and daemon-side fetches: the destination is the origin.
	"brw_open":           {Scope: ScopeRead, Target: TargetURL, Fields: []DestinationField{FieldURL}},
	"brw_open_incognito": {Scope: ScopeRead, Target: TargetURL, Fields: []DestinationField{FieldURL}},
	"brw_navigate_to":    {Scope: ScopeRead, Target: TargetURL, Fields: []DestinationField{FieldURL}},
	"brw_read_url":       {Scope: ScopeRead, Target: TargetURL, Fields: []DestinationField{FieldURL}},
	// Replaying a request re-executes it against the origin. A GET reads it; a
	// POST or a DELETE changes it, and the method is in the call's arguments.
	"brw_replay_request": {
		Scope: ScopeRead, Target: TargetURL, Fields: []DestinationField{FieldURL},
		Escalate: &Escalation{Field: "method", Values: []string{"post", "put", "patch", "delete"}},
	},
	// Cookies are addressed by url OR by bare domain, and with neither by the
	// tab's own URL. All three are the same site, so all three are checked.
	// Writing or deleting one is not a read of the site.
	"brw_cookies": {
		Scope: ScopeRead, Target: TargetURL,
		Fields:              []DestinationField{FieldURL, FieldDomain},
		PageWhenUnaddressed: true,
		Escalate:            &Escalation{Field: "action", Values: []string{"set", "delete"}},
	},
	// Authenticating hands an HTTP credential to an origin. That is not reading
	// it, so it needs the act scope. The required argument is origin; url is
	// optional, and gating only url gated nothing.
	"brw_authenticate": {Scope: ScopeAct, Target: TargetURL, Fields: []DestinationField{FieldOrigin, FieldURL}},
	// Extra headers are the same shape as authenticating: an Authorization
	// header bound to an origin is a credential handed to that origin.
	"brw_set_extra_headers": {Scope: ScopeAct, Target: TargetURL, Fields: []DestinationField{FieldOrigins}},

	// State-changing page actions, and script execution, which can do anything
	// an action can.
	"brw_click":            {Scope: ScopeAct, Target: TargetPage},
	"brw_click_text":       {Scope: ScopeAct, Target: TargetPage},
	"brw_click_xy":         {Scope: ScopeAct, Target: TargetPage},
	"brw_type":             {Scope: ScopeAct, Target: TargetPage},
	"brw_fill":             {Scope: ScopeAct, Target: TargetPage},
	"brw_select":           {Scope: ScopeAct, Target: TargetPage},
	"brw_press":            {Scope: ScopeAct, Target: TargetPage},
	"brw_key_down":         {Scope: ScopeAct, Target: TargetPage},
	"brw_key_up":           {Scope: ScopeAct, Target: TargetPage},
	"brw_drag":             {Scope: ScopeAct, Target: TargetPage},
	"brw_mouse_down":       {Scope: ScopeAct, Target: TargetPage},
	"brw_mouse_up":         {Scope: ScopeAct, Target: TargetPage},
	"brw_focus":            {Scope: ScopeAct, Target: TargetPage},
	"brw_evaluate":         {Scope: ScopeAct, Target: TargetPage},
	"brw_commit":           {Scope: ScopeAct, Target: TargetPage},
	"brw_pushstate":        {Scope: ScopeAct, Target: TargetPage},
	"brw_navigate":         {Scope: ScopeAct, Target: TargetPage},
	"brw_dialog":           {Scope: ScopeAct, Target: TargetPage},
	"brw_clipboard":        {Scope: ScopeAct, Target: TargetPage},
	"brw_highlight":        {Scope: ScopeAct, Target: TargetPage},
	"brw_route":            {Scope: ScopeAct, Target: TargetPage},
	"brw_network_capture":  {Scope: ScopeAct, Target: TargetPage},
	"brw_call_page_tool":   {Scope: ScopeAct, Target: TargetPage},
	"brw_page_tool_cancel": {Scope: ScopeAct, Target: TargetPage},
	"brw_recipe_run":       {Scope: ScopeAct, Target: TargetPage},
	// brw_upload_file acts on the page AND can name a URL to fetch the file
	// FROM, which reaches the network from the daemon rather than the page.
	"brw_upload_file": {Scope: ScopeAct, Target: TargetPage, Fetches: []DestinationField{FieldURL}},
	// Web storage for the current origin: reading it is a read of the site,
	// writing it is not.
	"brw_storage": {
		Scope: ScopeRead, Target: TargetPage,
		Escalate: &Escalation{Field: "action", Values: []string{"set", "remove", "clear"}},
	},

	// Reads of whatever the tab is showing. Gated against the LIVE origin, not
	// against the navigation that reached it.
	"brw_read":               {Scope: ScopeRead, Target: TargetPage},
	"brw_read_data":          {Scope: ScopeRead, Target: TargetPage},
	"brw_snapshot":           {Scope: ScopeRead, Target: TargetPage},
	"brw_find":               {Scope: ScopeRead, Target: TargetPage},
	"brw_get":                {Scope: ScopeRead, Target: TargetPage},
	"brw_observe":            {Scope: ScopeRead, Target: TargetPage},
	"brw_diff":               {Scope: ScopeRead, Target: TargetPage},
	"brw_frame":              {Scope: ScopeRead, Target: TargetPage},
	"brw_console":            {Scope: ScopeRead, Target: TargetPage},
	"brw_network_requests":   {Scope: ScopeRead, Target: TargetPage},
	"brw_screenshot":         {Scope: ScopeRead, Target: TargetPage},
	"brw_screenshot_element": {Scope: ScopeRead, Target: TargetPage},
	"brw_artifact_capture":   {Scope: ScopeRead, Target: TargetPage},
	"brw_scroll":             {Scope: ScopeRead, Target: TargetPage},
	"brw_hover":              {Scope: ScopeRead, Target: TargetPage},
	"brw_vitals":             {Scope: ScopeRead, Target: TargetPage},
	"brw_a11y_audit":         {Scope: ScopeRead, Target: TargetPage},
	"brw_page_tools":         {Scope: ScopeRead, Target: TargetPage},
	"brw_page_tool_result":   {Scope: ScopeRead, Target: TargetPage},
	"brw_assert":             {Scope: ScopeRead, Target: TargetPage},
	"brw_assert_visible":     {Scope: ScopeRead, Target: TargetPage},
	"brw_assert_hidden":      {Scope: ScopeRead, Target: TargetPage},
	"brw_assert_text":        {Scope: ScopeRead, Target: TargetPage},
	"brw_assert_value":       {Scope: ScopeRead, Target: TargetPage},
	// A wait reads the page, unless its condition is an "fn:" predicate, which
	// is script.
	"brw_wait_for": {Scope: ScopeRead, Target: TargetPage, ScriptCondition: true},
}

// UngatedTools names every registered tool that needs no grant, with the reason
// it needs none. It is not documentation: TestEveryToolIsClassifiedForConsent
// requires membership here or in ToolRules, so the reason is what a reviewer
// argues with when a tool is put in the wrong half.
var UngatedTools = map[string]string{
	"brw_list_tabs":              "lists targets and their URLs, which is what a user reads before granting anything; it reads no page content",
	"brw_list_tab_groups":        "reads Chrome's own tab-group metadata, not any page",
	"brw_group_tabs":             "moves tabs between Chrome groups; tab-strip organisation touches no site",
	"brw_ungroup_tabs":           "moves tabs between Chrome groups; tab-strip organisation touches no site",
	"brw_focus_tab":              "chooses which tab later calls address; every one of those calls is gated against that tab's own origin",
	"brw_close_tab":              "closes a target; it reads nothing and changes nothing on the site",
	"brw_close_context":          "disposes an incognito context and its storage; it reads nothing from any site",
	"brw_cancel":                 "stops brw's own in-flight operations",
	"brw_trace":                  "returns brw's own record of what it did",
	"brw_clear_trace":            "clears brw's own record of what it did",
	"brw_downloads":              "reports brw's own download bookkeeping",
	"brw_set_download_path":      "chooses where brw writes completed downloads on this machine",
	"brw_identity":               "reports which browser profile this brw drives",
	"brw_notify":                 "raises a desktop notification on this machine",
	"brw_window_bounds":          "reports the OS window geometry, not page content",
	"brw_window_resize":          "moves the OS window",
	"brw_emulate_device":         "overrides viewport metrics inside the renderer; it reads nothing and submits nothing",
	"brw_emulate_media":          "overrides the media features the renderer reports; it reads nothing and submits nothing",
	"brw_set_geolocation":        "overrides what navigator.geolocation reports to a tab",
	"brw_set_network_conditions": "throttles or disconnects a tab's network",
	"brw_set_user_agent":         "overrides what the tab calls itself",
	"brw_artifact_info":          "reads metadata for an already-captured artifact, never the live page",
	"brw_artifact_read":          "reads an already-captured artifact, never the live page",
	"brw_artifact_search":        "searches an already-captured artifact, never the live page",
	"brw_artifact_delete":        "deletes an already-captured artifact",
	"brw_recipe_search":          "queries the configured recipe provider; it drives no tab and reaches no site the agent named",
}

// SequenceTools are the operations whose payload is a list of steps, each of
// which is gated on its own.
var SequenceTools = map[string]bool{"brw_plan": true, "brw_batch": true}

// StepClass says what one plan/batch step needs from consent.
type StepClass int

const (
	// StepPageRead reads the page the sequence is on.
	StepPageRead StepClass = iota
	// StepAct changes the page the sequence is on.
	StepAct
	// StepNavigate steers the sequence's working tab to the step's own url, so
	// every step after it lands on that destination.
	StepNavigate
	// StepRetarget moves the sequence to a tab named by id, whose origin the
	// arguments do not carry.
	StepRetarget
)

// StepActions classifies every plan and batch step verb both runners implement.
//
// A sequence runner is a tool surface of its own, so it needs the same table
// rather than the same conditionals written again - and the table has to be
// complete, because a verb missing from it is a verb the gate does not decide.
// That is how a navigate_to step reached an un-granted origin while the gate
// inspected only "open". TestEveryPlanAndBatchStepActionIsClassified reads the
// case labels out of both backends' step switches and fails on any verb this map
// does not name, and on any name here that no runner implements.
var StepActions = map[string]StepClass{
	"click":          StepAct,
	"click_text":     StepAct,
	"type":           StepAct,
	"fill":           StepAct,
	"select":         StepAct,
	"press":          StepAct,
	"read":           StepPageRead,
	"snapshot":       StepPageRead,
	"scroll":         StepPageRead,
	"hover":          StepPageRead,
	"wait":           StepPageRead,
	"assert":         StepPageRead,
	"assert_visible": StepPageRead,
	"assert_hidden":  StepPageRead,
	"assert_text":    StepPageRead,
	"assert_value":   StepPageRead,
	"open":           StepNavigate,
	"navigate_to":    StepNavigate,
	"focus_tab":      StepRetarget,
}

// OriginEntry is one entry of a tool's origins list.
type OriginEntry struct {
	Origin string `json:"origin"`
}

// Probe reads the argument fields consent needs out of any call, without
// disturbing each tool's own decoding.
type Probe struct {
	URL       string        `json:"url"`
	Origin    string        `json:"origin"`
	Domain    string        `json:"domain"`
	Origins   []OriginEntry `json:"origins"`
	Action    string        `json:"action"`
	Method    string        `json:"method"`
	Condition string        `json:"condition"`
	Ref       string        `json:"ref"`
	Text      string        `json:"text"`
	Value     string        `json:"value"`
	Query     string        `json:"query"`
	Steps     []StepProbe   `json:"steps"`
}

// StepProbe is one plan/batch step's consent-relevant fields.
type StepProbe struct {
	Action    string `json:"action"`
	URL       string `json:"url"`
	ID        string `json:"id"`
	Ref       string `json:"ref"`
	Text      string `json:"text"`
	Value     string `json:"value"`
	Query     string `json:"query"`
	Condition string `json:"condition"`
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

// destinations returns the origins a probe carries in one named field.
func (p Probe) destinations(field DestinationField) []string {
	switch field {
	case FieldURL:
		return nonEmpty(p.URL)
	case FieldOrigin:
		return nonEmpty(p.Origin)
	case FieldDomain:
		return nonEmpty(p.Domain)
	case FieldOrigins:
		out := make([]string, 0, len(p.Origins))
		for _, entry := range p.Origins {
			out = append(out, entry.Origin)
		}
		return nonEmpty(out...)
	default:
		return nil
	}
}

// writes reports whether an escalation's argument says this call writes.
func (p Probe) writes(escalate *Escalation) bool {
	if escalate == nil {
		return false
	}
	var value string
	switch escalate.Field {
	case "action":
		value = p.Action
	case "method":
		value = p.Method
	}
	value = strings.ToLower(strings.TrimSpace(value))
	for _, want := range escalate.Values {
		if value == want {
			return true
		}
	}
	return false
}

// isScript reports whether a wait condition is one of brw's "fn:" predicates,
// which run in the page.
func isScript(condition string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(condition)), "fn:")
}

// OriginCheck is one origin a call has to be consented for, and the scope it
// needs there.
//
// An origin the call's own arguments name is carried in URL. One that only the
// live browser can answer for is carried as FromTab, and is resolved at check
// time: TabID names the tab when the call moves to one, and is empty for the tab
// the call already targets.
type OriginCheck struct {
	URL     string
	FromTab bool
	TabID   string
	Scope   Scope
	// Actions are the high-risk classification requests that run on this origin.
	Actions []PageAction
}

// PageAction pairs a classification request with the ref it came from, so the
// label can be filled in from the snapshot brw already returned.
type PageAction struct {
	Request ActionRequest
	Ref     string
	// FieldLabel says the ref's recovered label names a FIELD this action
	// writes to, not a control it presses. Personal data is classified from
	// field labels, so a fill addressed by a bare ref is classified from
	// nothing unless its label lands in Fields.
	FieldLabel bool
}

// Checks returns everything one call has to satisfy before it is dispatched.
//
// It returns an error rather than an empty list when the arguments do not say
// which origin the call lands on. Refusing there is the same rule the live-origin
// resolution follows: an action allowed because brw did not know where it was
// landing is the failure this whole surface exists to stop.
func Checks(tool string, probe Probe) ([]OriginCheck, error) {
	if SequenceTools[tool] {
		return sequenceChecks(tool, probe)
	}
	rule, known := ToolRules[tool]
	if !known {
		return nil, nil
	}
	var checks []OriginCheck
	for _, field := range rule.Fetches {
		// The daemon retrieves these itself, so they are a read of that origin
		// whatever the tool does with the result afterwards.
		for _, destination := range probe.destinations(field) {
			checks = append(checks, OriginCheck{URL: destination, Scope: ScopeRead})
		}
	}
	scope := rule.Scope
	if probe.writes(rule.Escalate) || (rule.ScriptCondition && isScript(probe.Condition)) {
		scope = ScopeAct
	}
	if rule.Target == TargetURL {
		named := 0
		for _, field := range rule.Fields {
			for _, destination := range probe.destinations(field) {
				named++
				checks = append(checks, OriginCheck{URL: destination, Scope: scope})
			}
		}
		if named > 0 || !rule.PageWhenUnaddressed {
			return checks, nil
		}
	}
	check := OriginCheck{FromTab: true, Scope: scope}
	if scope == ScopeAct {
		check.Actions = []PageAction{toolAction(tool, probe)}
	}
	return append(checks, check), nil
}

// toolAction builds the classification request for a single-tool page action.
func toolAction(tool string, probe Probe) PageAction {
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
		action.FieldLabel = true
	}
	return action
}

// sequenceChecks walks a plan or batch in step order.
//
// A sequence is gated once, before any step runs, but the steps do not all land
// on one origin: open and navigate_to move the working tab, and focus_tab moves
// to another tab entirely. So the walk keeps segments - a run of steps sharing
// one destination - and each segment carries the widest scope its steps need. A
// segment whose destination the steps named is checked by name; one that only
// the browser can answer for is resolved from the tab.
func sequenceChecks(tool string, probe Probe) ([]OriginCheck, error) {
	// The first segment is the tab the call already targets.
	segments := []OriginCheck{{FromTab: true}}
	current := func() *OriginCheck { return &segments[len(segments)-1] }
	// undecidable records a segment brw cannot resolve, so it is refused only if
	// a later step actually needs that origin.
	undecidable := map[int]bool{}
	for _, step := range probe.Steps {
		verb := strings.ToLower(strings.TrimSpace(step.Action))
		class, known := StepActions[verb]
		if !known {
			// An unknown verb is a step this table did not decide. The runner
			// rejects it too, but the gate must not be the thing that lets it
			// through on the way.
			return nil, &CannotDecideError{Tool: tool, Reason: "step " + clip(verb) + " is not a verb site consent classifies"}
		}
		switch class {
		case StepAct:
			raise(current(), ScopeAct)
			current().Actions = append(current().Actions, stepAction(tool, verb, step))
		case StepPageRead:
			if verb == "wait" && isScript(step.Condition) {
				// An "fn:" wait condition runs in the page, so it is script, not
				// a read.
				raise(current(), ScopeAct)
				current().Actions = append(current().Actions, stepAction(tool, verb, step))
				continue
			}
			raise(current(), ScopeRead)
		case StepNavigate:
			if step.URL != "" {
				// Arriving is a read of the destination; what the following
				// steps do there is the next segment's business.
				segments = append(segments, OriginCheck{URL: step.URL, Scope: ScopeRead})
			}
			segments = append(segments, OriginCheck{URL: step.URL})
			if step.URL == "" {
				undecidable[len(segments)-1] = true
			}
		case StepRetarget:
			segments = append(segments, OriginCheck{FromTab: true, TabID: step.ID})
			if step.ID == "" {
				undecidable[len(segments)-1] = true
			}
		}
	}
	checks := make([]OriginCheck, 0, len(segments))
	for index, segment := range segments {
		if segment.Scope == "" {
			continue
		}
		if undecidable[index] {
			return nil, &CannotDecideError{Tool: tool, Reason: "a step moves the sequence to a tab it does not name, so the origin the steps after it land on cannot be resolved; run those steps as their own call"}
		}
		checks = append(checks, segment)
	}
	// Origins the arguments name are checked first: they cost no round trip, so
	// a sequence naming an un-granted destination is refused without asking the
	// browser anything.
	sorted := make([]OriginCheck, 0, len(checks))
	for _, check := range checks {
		if !check.FromTab {
			sorted = append(sorted, check)
		}
	}
	for _, check := range checks {
		if check.FromTab {
			sorted = append(sorted, check)
		}
	}
	return sorted, nil
}

// raise widens a segment's scope, never narrows it.
func raise(segment *OriginCheck, scope Scope) {
	if segment.Scope == "" || (scope == ScopeAct && segment.Scope == ScopeRead) {
		segment.Scope = scope
	}
}

// stepAction builds the classification request for one acting step.
func stepAction(tool, verb string, step StepProbe) PageAction {
	action := PageAction{
		Request: ActionRequest{Tool: tool + " step " + verb, Text: step.Text},
		Ref:     step.Ref,
	}
	if verb == "fill" || verb == "type" {
		// Same exemption as the single-tool path: a fill step's text IS the
		// typed value, and echoing it into a refusal would put a card number in
		// an error string. The field is named by the ref, whose label the
		// snapshot brw already returned carries.
		action.Request.Text = ""
		action.FieldLabel = true
	}
	return action
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

// PageOriginFunc resolves the origin a tab is currently showing. An empty tabID
// means the tab the call targets. It must fail rather than answer "" when it
// cannot tell: an action allowed because brw did not know where it was landing
// is the failure this whole surface exists to stop.
type PageOriginFunc func(tabID string) (string, error)

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
	checks, err := Checks(tool, ParseProbe(args))
	if err != nil {
		return err
	}
	for _, check := range checks {
		origin := check.URL
		if check.FromTab {
			if pageOrigin == nil {
				return &CannotDecideError{Tool: tool, Reason: "this surface cannot report the tab's current origin, so there is nothing to check against"}
			}
			resolved, err := pageOrigin(check.TabID)
			if err != nil {
				return err
			}
			origin = resolved
		}
		if err := g.Authorize(origin, check.Scope); err != nil {
			return err
		}
		if !g.ConfirmActions() {
			continue
		}
		for _, action := range check.Actions {
			action.Request.Origin = origin
			if action.Ref != "" && label != nil {
				if name := label(action.Ref); name != "" {
					if action.FieldLabel {
						if len(action.Request.Fields) == 0 {
							action.Request.Fields = nonEmpty(name)
							action.Request.Label = name
						}
					} else if action.Request.Label == "" {
						action.Request.Label = name
					}
				}
			}
			if err := g.CheckAction(action.Request); err != nil {
				return err
			}
		}
	}
	return nil
}
