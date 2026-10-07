package siteconsent

import (
	"encoding/json"
	"strings"
)

// ToolTarget says where a tool's origin comes from.
type ToolTarget int

const (
	// TargetURL: the origin is the destination named in the call's own arguments.
	TargetURL ToolTarget = iota
	// TargetPage: the origin is whatever the tab is currently showing.
	TargetPage
)

// DestinationField is an argument a tool names its destination in, spelled the way that tool spells it.
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

// Escalation raises a rule's scope to act when one of the call's own arguments says the call writes.
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
	// Fields are the arguments a TargetURL tool's destination arrives in.
	Fields []DestinationField
	// Fetches name arguments carrying a URL the DAEMON retrieves itself rather than the page.
	Fetches []DestinationField
	// PageAlso decides, from the call's own arguments, whether the call ALSO reaches the tab's live origin.
	PageAlso func(Probe) bool
	// Escalate raises Scope to act for the calls its Field/Values name.
	Escalate *Escalation
	// ScriptCondition marks a tool whose "condition" argument can carry page script (brw's "fn:" predicates).
	ScriptCondition bool
}

// ToolRules is the whole mapping from operation to consent requirement.
var ToolRules = map[string]ToolRule{
	"brw_open":           {Scope: ScopeRead, Target: TargetURL, Fields: []DestinationField{FieldURL}},
	"brw_watch_page":     {Scope: ScopeRead, Target: TargetURL, Fields: []DestinationField{FieldURL}},
	"brw_open_incognito": {Scope: ScopeRead, Target: TargetURL, Fields: []DestinationField{FieldURL}},
	"brw_navigate_to":    {Scope: ScopeRead, Target: TargetURL, Fields: []DestinationField{FieldURL}},
	"brw_read_url":       {Scope: ScopeRead, Target: TargetURL, Fields: []DestinationField{FieldURL}},

	"brw_replay_request": {
		Scope: ScopeRead, Target: TargetURL, Fields: []DestinationField{FieldURL},
		Escalate: &Escalation{Field: "method", Values: []string{"post", "put", "patch", "delete"}},
	},

	"brw_cookies": {
		Scope: ScopeRead, Target: TargetURL,
		Fields:   []DestinationField{FieldURL, FieldDomain},
		PageAlso: cookiesReachTheTab,
		Escalate: &Escalation{Field: "action", Values: []string{"set", "delete", "import"}},
	},

	"brw_authenticate": {Scope: ScopeAct, Target: TargetURL, Fields: []DestinationField{FieldOrigin, FieldURL}},

	"brw_set_extra_headers": {Scope: ScopeAct, Target: TargetURL, Fields: []DestinationField{FieldOrigins}},

	"brw_init_script": {Scope: ScopeAct, Target: TargetPage},

	"brw_touch": {Scope: ScopeAct, Target: TargetPage},

	"brw_check": {Scope: ScopeAct, Target: TargetPage},

	"brw_state": {Scope: ScopeAct, Target: TargetURL, Fields: []DestinationField{FieldOrigins}},

	"brw_baseline":         {Scope: ScopeRead, Target: TargetPage},
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

	"brw_upload_file": {Scope: ScopeAct, Target: TargetPage, Fetches: []DestinationField{FieldURL}},

	"brw_storage": {
		Scope: ScopeRead, Target: TargetPage,
		Escalate: &Escalation{Field: "action", Values: []string{"set", "remove", "clear"}},
	},

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
	"brw_screenshot_save":    {Scope: ScopeRead, Target: TargetPage},
	"brw_screenshot":         {Scope: ScopeRead, Target: TargetPage},
	"brw_screenshot_element": {Scope: ScopeRead, Target: TargetPage},
	"brw_artifact_capture":   {Scope: ScopeRead, Target: TargetPage},
	"brw_scroll":             {Scope: ScopeRead, Target: TargetPage},
	"brw_profile":            {Scope: ScopeRead, Target: TargetPage},
	"brw_react":              {Scope: ScopeRead, Target: TargetPage},
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

	"brw_wait_for": {Scope: ScopeRead, Target: TargetPage, ScriptCondition: true},
}

// UngatedTools names every registered tool that needs no grant, with the reason it needs none.
var UngatedTools = map[string]string{
	"brw_approval_resume":        "dispatches the original tool through its full consent and approval gates",
	"brw_approval_status":        "reads only lifecycle metadata of an approval request; cannot authorize execution",
	"brw_page_watchers":          "manages browser-host registration metadata; the service checks consent before resuming",
	"brw_page_events":            "reads only registered URLs and previously captured digest/count signals; metadata remains available to deliver refused/unavailable after consent revocation, without reading any page",
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
	"brw_skill":                  "returns brw's own operating manual out of the daemon binary; it drives no browser and reaches no site",
	"brw_tools":                  "searches brw's own tool catalogue and discloses more of it; it drives no tab and names no origin",
	"brw_notify":                 "raises a desktop notification on this machine",
	"brw_window_bounds":          "reports the OS window geometry, not page content",
	"brw_window_resize":          "moves the OS window",
	"brw_emulate_device":         "overrides viewport metrics inside the renderer; it reads nothing and submits nothing",
	"brw_emulate_media":          "overrides the media features the renderer reports; it reads nothing and submits nothing",
	"brw_set_geolocation":        "overrides what navigator.geolocation reports to a tab",
	"brw_set_network_conditions": "throttles or disconnects a tab's network",
	"brw_set_user_agent":         "overrides what the tab calls itself",
	"brw_set_locale":             "overrides the language, time zone and calendar the renderer reports; it reads nothing and submits nothing",
	"brw_artifact_info":          "reads metadata for an already-captured artifact, never the live page",
	"brw_artifact_read":          "reads an already-captured artifact, never the live page",
	"brw_artifact_search":        "searches an already-captured artifact, never the live page",
	"brw_artifact_delete":        "deletes an already-captured artifact",
	"brw_recipe_search":          "queries the configured recipe provider; it drives no tab and reaches no site the agent named",
}

// SequenceTools are the operations whose payload is a list of steps, each of which is gated on its own.
var SequenceTools = map[string]bool{"brw_plan": true, "brw_batch": true}

// StepClass says what one plan/batch step needs from consent.
type StepClass int

const (
	// StepPageRead reads the page the sequence is on.
	StepPageRead StepClass = iota
	// StepAct changes the page the sequence is on.
	StepAct
	// StepNavigate steers the sequence's working tab to the step's own url, so every step after it lands on that destination.
	StepNavigate
	// StepRetarget moves the sequence to a tab named by id, whose origin the arguments do not carry.
	StepRetarget
)

// StepActions classifies every plan and batch step verb both runners implement.
var StepActions = map[string]StepClass{
	"click":      StepAct,
	"click_text": StepAct,
	"type":       StepAct,
	"fill":       StepAct,
	"select":     StepAct,
	"press":      StepAct,

	"find_act":       StepAct,
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

func (e *OriginEntry) UnmarshalJSON(data []byte) error {
	var bare string
	if err := json.Unmarshal(data, &bare); err == nil {
		e.Origin = bare
		return nil
	}
	var object struct {
		Origin string `json:"origin"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	e.Origin = object.Origin
	return nil
}

// Probe reads the argument fields consent needs out of any call, without disturbing each tool's own decoding.
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

// ParseProbe reads a call's arguments.
func ParseProbe(args []byte) Probe {
	var probe Probe
	if len(args) > 0 {
		_ = json.Unmarshal(args, &probe)
	}
	return probe
}

func (p Probe) destinations(field DestinationField) []string {
	switch field {
	case FieldURL:
		return nonEmpty(p.URL)
	case FieldOrigin:
		return nonEmpty(p.Origin)
	case FieldDomain:

		return nonEmpty(strings.TrimPrefix(strings.TrimSpace(p.Domain), "."))
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

func isScript(condition string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(condition)), "fn:")
}

// OriginCheck is one origin a call has to be consented for, and the scope it needs there.
type OriginCheck struct {
	URL     string
	FromTab bool
	TabID   string
	Scope   Scope
	// Actions are the high-risk classification requests that run on this origin.
	Actions []PageAction
}

// PageAction pairs a classification request with the ref it came from, so the label can be filled in from the snapshot brw already returned.
type PageAction struct {
	Request ActionRequest
	Ref     string
	// FieldLabel says the ref's recovered label names a FIELD this action writes to, not a control it presses.
	FieldLabel bool
}

// Checks returns everything one call has to satisfy before it is dispatched.
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
		for _, destination := range probe.destinations(field) {
			checks = append(checks, OriginCheck{URL: destination, Scope: ScopeRead})
		}
	}
	scope := rule.Scope
	if probe.writes(rule.Escalate) || (rule.ScriptCondition && isScript(probe.Condition)) {
		scope = ScopeAct
	}
	if rule.Target == TargetURL {
		for _, field := range rule.Fields {
			for _, destination := range probe.destinations(field) {
				checks = append(checks, OriginCheck{URL: destination, Scope: scope})
			}
		}
		if rule.PageAlso == nil || !rule.PageAlso(probe) {
			return checks, nil
		}
	}
	check := OriginCheck{FromTab: true, Scope: scope}
	if scope == ScopeAct {
		check.Actions = []PageAction{toolAction(tool, probe)}
	}
	return append(checks, check), nil
}

func toolAction(tool string, probe Probe) PageAction {
	action := PageAction{
		Request: ActionRequest{Tool: tool, Label: probe.Query, Text: probe.Text},
		Ref:     probe.Ref,
	}
	if tool == "brw_fill" || tool == "brw_type" {
		action.Request.Fields = nonEmpty(probe.Query)
		action.Request.Text = ""
		action.FieldLabel = true
	}
	return action
}

func sequenceChecks(tool string, probe Probe) ([]OriginCheck, error) {
	segments := []OriginCheck{{FromTab: true}}
	current := func() *OriginCheck { return &segments[len(segments)-1] }

	undecidable := map[int]bool{}
	deferred := deferredStepActions(probe)
	classify := func(index int, action PageAction) {
		if deferred[index] {
			return
		}
		current().Actions = append(current().Actions, action)
	}
	for index, step := range probe.Steps {
		verb := strings.ToLower(strings.TrimSpace(step.Action))
		class, known := StepActions[verb]
		if !known {
			return nil, &CannotDecideError{Tool: tool, Reason: "step " + clip(verb) + " is not a verb site consent classifies"}
		}
		switch class {
		case StepAct:
			raise(current(), ScopeAct)
			classify(index, stepAction(tool, verb, step))
		case StepPageRead:
			if verb == "wait" && isScript(step.Condition) {
				raise(current(), ScopeAct)
				classify(index, stepAction(tool, verb, step))
				continue
			}
			raise(current(), ScopeRead)
		case StepNavigate:
			if step.URL != "" {
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

func deferredStepActions(probe Probe) map[int]bool {
	deferred := map[int]bool{}
	acted := false
	for index, step := range probe.Steps {
		verb := strings.ToLower(strings.TrimSpace(step.Action))
		class, known := StepActions[verb]
		if !known {
			continue
		}
		scripted := class == StepPageRead && verb == "wait" && isScript(step.Condition)
		switch {
		case class == StepNavigate || class == StepRetarget:

			acted = false
		case class == StepAct || scripted:
			if acted {
				deferred[index] = true
			}
			acted = true
		}
	}
	return deferred
}

// StepGate re-checks the steps of ONE plan or batch call as they run.
type StepGate struct {
	guard   *Guard
	tool    string
	confirm map[int]bool
}

// NewStepGate builds the runtime half of a sequence's gate from the same arguments its preflight read.
func (g *Guard) NewStepGate(tool string, args []byte) *StepGate {
	if !g.Enabled() || !SequenceTools[tool] {
		return nil
	}
	return &StepGate{guard: g, tool: tool, confirm: deferredStepActions(ParseProbe(args))}
}

// Check re-gates the step at index against the origin the tab is showing now.
func (s *StepGate) Check(index int, step StepProbe, pageOrigin PageOriginFunc, label LabelFunc) error {
	if s == nil {
		return nil
	}
	checks, err := Checks(s.tool, Probe{Steps: []StepProbe{step}})
	if err != nil {
		return err
	}
	if !s.confirm[index] {
		for i := range checks {
			checks[i].Actions = nil
		}
	}
	return s.guard.runChecks(s.tool, checks, pageOrigin, label)
}

func raise(segment *OriginCheck, scope Scope) {
	if segment.Scope == "" || (scope == ScopeAct && segment.Scope == ScopeRead) {
		segment.Scope = scope
	}
}

func stepAction(tool, verb string, step StepProbe) PageAction {
	action := PageAction{
		Request: ActionRequest{Tool: tool + " step " + verb, Text: step.Text},
		Ref:     step.Ref,
	}
	if verb == "fill" || verb == "type" {
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

// PageOriginFunc resolves the origin a tab is currently showing.
type PageOriginFunc func(tabID string) (string, error)

// LabelFunc returns the accessible name brw last reported for a ref, or "" when it has never described that ref.
type LabelFunc func(ref string) string

// CheckTool is the single gate a call passes through, on every surface.
func (g *Guard) CheckTool(tool string, args []byte, pageOrigin PageOriginFunc, label LabelFunc) error {
	if !g.Enabled() {
		return nil
	}
	checks, err := Checks(tool, ParseProbe(args))
	if err != nil {
		return err
	}
	return g.runChecks(tool, checks, pageOrigin, label)
}

func (g *Guard) runChecks(tool string, checks []OriginCheck, pageOrigin PageOriginFunc, label LabelFunc) error {
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
