package recipe

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Don-Works/brw/internal/actions"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// TraceObservation is the page state brw recorded on one side of a traced action: the elements it could see and the URL it was on.
type TraceObservation struct {
	URL       string             `json:"url"`
	Elements  []snapshot.Element `json:"elements,omitempty"`
	Downloads []TraceDownload    `json:"downloads,omitempty"`
}

func (o *TraceObservation) empty() bool {
	return o == nil || (strings.TrimSpace(o.URL) == "" && len(o.Elements) == 0 && len(o.Downloads) == 0)
}

func (o *TraceObservation) elements() []snapshot.Element {
	if o == nil {
		return nil
	}
	return o.Elements
}

func (o *TraceObservation) pageURL() string {
	if o == nil {
		return ""
	}
	return strings.TrimSpace(o.URL)
}

// TraceDownload is one download the recording saw.
type TraceDownload struct {
	Filename  string `json:"filename"`
	SHA256    string `json:"sha256,omitempty"`
	Bytes     *int64 `json:"bytes,omitempty"`
	Completed bool   `json:"completed"`
}

// TraceStep is one entry of a scoped trace buffer: the action brw recorded, plus the observations taken either side of it.
type TraceStep struct {
	TraceAction
	Before *TraceObservation `json:"before,omitempty"`
	After  *TraceObservation `json:"after,omitempty"`
}

// WriteDeclaration marks one traced action as committing an external write.
type WriteDeclaration struct {
	// Verify re-reads remote state after the write lands.
	Verify Assertion `json:"verify"`
	// Nonce names the site's own duplicate-suppression field, when it exposes one.
	Nonce *SiteIdempotency `json:"site_idempotency,omitempty"`
}

// CompileOptions carries everything the recording cannot supply.
type CompileOptions struct {
	ID          string                   `json:"id"`
	Version     string                   `json:"version"`
	Name        string                   `json:"name"`
	Description string                   `json:"description"`
	Intents     []string                 `json:"intents"`
	Origins     []string                 `json:"origins"`
	Risk        string                   `json:"risk"`
	Writes      map[int]WriteDeclaration `json:"writes,omitempty"`
	// CaptureOnFailure asks the browser host for a failure evidence bundle.
	CaptureOnFailure bool `json:"capture_on_failure,omitempty"`
}

// CompiledTarget records how one step's element identity was re-derived, so a reviewer can see the evidence rather than trust the output.
type CompiledTarget struct {
	StepID string `json:"step_id"`
	Role   string `json:"role"`
	Name   string `json:"name"`
	Origin string `json:"origin"`
	// Ordinal is the acted element's position among the candidates the derived target ranked, and Candidates is how many there were.
	Ordinal    int `json:"ordinal"`
	Candidates int `json:"candidates"`
}

// CompileResult is a compiled draft plus the review material that justifies it.
type CompileResult struct {
	Recipe  Recipe           `json:"recipe"`
	Targets []CompiledTarget `json:"targets"`
	// Review is the diff-able body.
	Review string `json:"review"`
}

// CompileError names the trace step compilation stopped on.
type CompileError struct {
	StepIndex int
	Action    string
	Reason    string
}

func (e CompileError) Error() string {
	action := strings.TrimSpace(e.Action)
	if action == "" {
		action = "unnamed action"
	}
	return fmt.Sprintf("trace step %d (%s): %s", e.StepIndex, action, e.Reason)
}

var coordinateActions = map[string]bool{
	"click_xy":     true,
	"click_button": true,
	"drag":         true,
	"mouse_down":   true,
	"mouse_up":     true,
	"mouse_move":   true,
}

var compilableActions = map[string]string{
	"click":       "click",
	"fill":        "fill",
	"type":        "type",
	"select":      "select",
	"press":       "press",
	"navigate_to": "navigate_to",
}

var literalValueActions = map[string]bool{
	"click":       false,
	"fill":        true,
	"type":        true,
	"select":      true,
	"press":       true,
	"navigate_to": false,
}

var credentialNamePattern = regexp.MustCompile(`(?i)pass\s?word|passcode|passphrase|\bpin\b|\botp\b|one[- ]time code|security code|\bcvv\b|card number|api[ _-]?key|credential|\bsecret\b|recovery code`)

const (
	postconditionTimeoutMS = 15_000
	downloadTimeoutMS      = 60_000
)

// Compile turns a scoped trace buffer into a reviewed, immutable-ready recipe.
func Compile(steps []TraceStep, opts CompileOptions) (CompileResult, error) {
	compiler, err := newCompiler(opts)
	if err != nil {
		return CompileResult{}, err
	}
	if err := compiler.run(steps); err != nil {
		return CompileResult{}, err
	}
	return compiler.finish()
}

type compiler struct {
	opts    CompileOptions
	origins []string
	steps   []Step
	inputs  map[string]Input
	targets []CompiledTarget

	declaredWrites map[int]bool
}

func newCompiler(opts CompileOptions) (*compiler, error) {
	var problems []error
	if !recipeIDPattern.MatchString(opts.ID) {
		problems = append(problems, errors.New("compile requires a dotted lowercase id, e.g. example.billing.pay-invoice"))
	}
	if !versionPattern.MatchString(opts.Version) {
		problems = append(problems, errors.New("compile requires a semantic version major.minor.patch"))
	}
	if strings.TrimSpace(opts.Name) == "" || strings.TrimSpace(opts.Description) == "" {
		problems = append(problems, errors.New("compile requires a name and a description"))
	}
	if len(opts.Intents) == 0 {
		problems = append(problems, errors.New("compile requires at least one intent"))
	}
	if opts.Risk != "read_only" && opts.Risk != "external_write" {
		problems = append(problems, errors.New("compile requires risk read_only or external_write"))
	}
	if len(opts.Origins) == 0 {
		problems = append(problems, errors.New("compile requires an explicit origin allowlist; inferring it from the trace would make the cross-origin check meaningless"))
	}
	origins := make([]string, 0, len(opts.Origins))
	for _, origin := range opts.Origins {
		origin = strings.TrimSpace(origin)
		if err := validateOrigin(origin); err != nil {
			problems = append(problems, err)
			continue
		}
		if !slices.Contains(origins, origin) {
			origins = append(origins, origin)
		}
	}
	for index, declaration := range opts.Writes {
		if index < 1 {
			problems = append(problems, fmt.Errorf("write declaration index %d is not a 1-based trace step", index))
		}
		if declaration.Nonce != nil {
			if err := validateSiteIdempotency(*declaration.Nonce); err != nil {
				problems = append(problems, fmt.Errorf("write declaration %d: %w", index, err))
			}
		}
	}
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	return &compiler{
		opts:           opts,
		origins:        origins,
		inputs:         map[string]Input{},
		declaredWrites: map[int]bool{},
	}, nil
}

func (c *compiler) run(steps []TraceStep) error {
	var previousAfter *TraceObservation
	compiled := 0
	for index := range steps {
		step := steps[index]
		position := index + 1
		action := strings.TrimSpace(step.Action)
		fail := func(reason string) error {
			return CompileError{StepIndex: position, Action: action, Reason: reason}
		}
		if action == "" {
			return fail("recorded entry has no action")
		}
		if coordinateActions[action] {
			return fail("coordinate-driven action; a recipe addresses elements semantically, so last run's pixel position cannot be compiled")
		}
		if browser.IsObservationAction(action) {

			if !step.After.empty() {
				previousAfter = step.After
			}
			continue
		}
		verb, ok := compilableActions[action]
		if !ok {
			return fail("no deterministic recipe action expresses this; re-record the flow using ref-addressed actions")
		}
		if !step.OK {
			return fail("recorded action failed, so the flow it belongs to was never observed to work")
		}
		if step.After.empty() {
			return fail("post-action observation is empty, so nothing the action did was observed and no postcondition can be inferred")
		}
		before := step.Before
		if before.empty() {
			before = previousAfter
		}
		if err := c.checkOrigins(position, action, before, step); err != nil {
			return err
		}
		if err := c.compileStep(position, verb, step, before, compiled); err != nil {
			return err
		}
		compiled++
		previousAfter = step.After
	}
	if compiled == 0 {
		return errors.New("trace contains no compilable actions")
	}
	unapplied := make([]int, 0, len(c.opts.Writes))
	for index := range c.opts.Writes {
		if !c.declaredWrites[index] {
			unapplied = append(unapplied, index)
		}
	}
	if len(unapplied) > 0 {
		slices.Sort(unapplied)
		return fmt.Errorf("write declarations name trace steps %v, which the trace does not contain as compilable actions", unapplied)
	}
	return nil
}

func (c *compiler) checkOrigins(position int, action string, before *TraceObservation, step TraceStep) error {
	candidates := []string{before.pageURL(), step.After.pageURL()}
	if action == "navigate_to" {
		candidates = append(candidates, strings.TrimSpace(step.URL))
	}
	for _, raw := range candidates {
		if raw == "" {
			continue
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return CompileError{StepIndex: position, Action: action, Reason: fmt.Sprintf("observed location %q is not an absolute URL", raw)}
		}
		origin := parsed.Scheme + "://" + parsed.Host
		if !slices.Contains(c.origins, origin) {
			return CompileError{
				StepIndex: position, Action: action,
				Reason: fmt.Sprintf("navigation to origin %s is not in the declared origin list %s", origin, strings.Join(c.origins, ", ")),
			}
		}
	}
	return nil
}

func (c *compiler) compileStep(position int, verb string, traced TraceStep, before *TraceObservation, compiled int) error {
	fail := func(reason string) error {
		return CompileError{StepIndex: position, Action: traced.Action, Reason: reason}
	}
	declaration, isWrite := c.opts.Writes[position]
	if isWrite {
		c.declaredWrites[position] = true
		if c.opts.Risk != "external_write" {
			return fail("declared as an external write, but the recipe risk is read_only")
		}
	}

	step := Step{ID: fmt.Sprintf("s%d", compiled+1), Action: verb}

	var observed snapshot.Element
	if verb != "navigate_to" {
		if before.empty() {
			return fail("nothing was observed of the page this action was aimed at, so its element identity cannot be re-derived; record the page state before each action")
		}
		if err := c.checkCredentialField(position, traced, before); err != nil {
			return err
		}
		target, record, element, err := c.deriveTarget(position, traced, before)
		if err != nil {
			return err
		}
		record.StepID = step.ID
		record.Origin = originOf(before.pageURL())
		step.Target = target
		observed = element
		c.targets = append(c.targets, record)
	}

	switch verb {
	case "navigate_to":
		target := strings.TrimSpace(traced.URL)
		if target == "" {
			target = traced.After.pageURL()
		}
		if target == "" {
			return fail("navigation recorded no URL")
		}
		step.URL = target
	case "fill", "type", "select":
		literal := strings.TrimSpace(traced.Text)
		if literal == "" {
			literal = strings.TrimSpace(traced.Value)
		}
		if literal == "" {
			return fail("recorded action captured no value, so the compiled step would clear the field instead of filling it")
		}

		step.Value = "${input:" + c.declareInput(step.Target, observed) + "}"
	case "press":
		key := strings.TrimSpace(traced.Text)
		if key == "" {
			key = strings.TrimSpace(traced.Value)
		}
		if key == "" {
			return fail("recorded key press captured no key")
		}
		if !actions.IsCommandKey(key) {

			return fail("recorded key press is a literal character rather than a key that issues a command such as Enter, Tab or ctrl+a; a value entered one keystroke at a time is data, and a recipe carries inputs instead")
		}
		step.Key = key
	}

	postcondition, assertion, err := c.inferPostcondition(before, traced.After, isWrite)
	if err != nil {
		return fail(err.Error())
	}
	step.Postcondition = postcondition
	if isWrite {
		step.Effect = "external_write"
		step.IdempotencyKey = c.writeIdempotencyKey(step.ID)
		step.SiteIdempotency = cloneSiteIdempotency(declaration.Nonce)
	} else {
		step.Effect = "read"
	}
	c.steps = append(c.steps, step)

	if assertion != nil {
		c.steps = append(c.steps, Step{ID: step.ID + "_evidence", Action: "assert", Assert: assertion})
	}
	if isWrite {
		verification := *cloneAssertion(&declaration.Verify)
		if err := validateAssertion(verification, c.inputs); err != nil {
			return fail(fmt.Sprintf("declared verification is not a usable assertion: %v", err))
		}

		c.steps = append(c.steps, Step{
			ID: step.ID + "_verify", Action: "assert", Assert: &verification, Verifies: step.ID,
		})
	}
	return nil
}

func (c *compiler) writeIdempotencyKey(stepID string) string {
	key := c.opts.ID + ":" + stepID
	for _, name := range sortedInputNames(c.inputs) {
		key += ":${input:" + name + "}"
	}
	return key
}

func (c *compiler) checkCredentialField(position int, traced TraceStep, before *TraceObservation) error {
	carries, classified := literalValueActions[traced.Action]
	if !classified {
		return CompileError{
			StepIndex: position, Action: traced.Action,
			Reason: "no classification says whether this action carried a typed value into the page, so brw cannot tell whether it was a credential field; refusing rather than guessing",
		}
	}
	if !carries {
		return nil
	}
	reason := ""
	switch {
	case traced.Redacted:
		reason = "brw withheld the recorded value because the field is credential-bearing"
	case credentialNamePattern.MatchString(traced.Name):
		reason = fmt.Sprintf("the field's accessible name %q reads as a credential field", traced.Name)
	}
	if reason == "" {
		for _, element := range before.elements() {
			if element.Ref != traced.Ref {
				continue
			}
			if element.Sensitive || strings.EqualFold(element.Type, "password") {
				reason = "the observed field is marked sensitive"
			} else if credentialNamePattern.MatchString(element.Name) {
				reason = fmt.Sprintf("the observed field's accessible name %q reads as a credential field", element.Name)
			}
			break
		}
	}
	if reason == "" {
		return nil
	}
	return CompileError{
		StepIndex: position, Action: traced.Action,
		Reason: "writes to a credential field: " + reason + "; brw is not a secret store, so this flow cannot become a recipe",
	}
}

func (c *compiler) deriveTarget(position int, traced TraceStep, before *TraceObservation) (*Target, CompiledTarget, snapshot.Element, error) {
	fail := func(reason string) error {
		return CompileError{StepIndex: position, Action: traced.Action, Reason: reason}
	}
	role := strings.TrimSpace(traced.Role)
	name := strings.TrimSpace(traced.Name)
	observed, found := findObservedElement(before.elements(), traced.Ref)
	if role == "" && found {
		role = observed.Role
	}
	if role == "" {
		return nil, CompiledTarget{}, observed, fail("recorded action carries no element role, so no semantic target can be derived")
	}
	if name == "" && found {
		name = observed.Name
	}
	target := &Target{Role: role}
	switch {
	case name != "" && traced.NameIsVisibleText:
		target.Name = name
	case name != "":
		target.NameContains = name
	case found && observed.TestID != "":
		target.TestID = observed.TestID
	case found && stableHref(observed.Href) != "":
		target.HrefContains = stableHref(observed.Href)
	default:
		return nil, CompiledTarget{}, observed, fail("the acted element had no accessible name, test id or href to identify it by")
	}
	if err := validateTarget(*target); err != nil {
		return nil, CompiledTarget{}, observed, fail(err.Error())
	}

	candidates := snapshot.RankTargetCandidates(before.elements(), targetCriteria(*target))
	if len(candidates) == 0 {
		return nil, CompiledTarget{}, observed, fail(fmt.Sprintf(
			"re-derived target %s matched no element in the observation the action was aimed at", describeTarget(*target)))
	}
	if len(candidates) > 1 {
		if narrowed, ok := narrowTarget(*target, observed, before.elements()); ok {
			target = &narrowed
			candidates = snapshot.RankTargetCandidates(before.elements(), targetCriteria(narrowed))
		}
	}
	if len(candidates) > 1 {
		return nil, CompiledTarget{}, observed, fail(fmt.Sprintf(
			"ambiguous target: %s matched %d elements, and a recipe that acts on one of several look-alike elements acts on the wrong one sooner or later",
			describeTarget(*target), len(candidates)))
	}
	ordinal := 0
	for index, candidate := range candidates {
		if candidate.Ref == traced.Ref {
			ordinal = index + 1
			break
		}
	}
	if ordinal == 0 {
		return nil, CompiledTarget{}, observed, fail(fmt.Sprintf(
			"re-derived target %s does not name the element the recording acted on", describeTarget(*target)))
	}
	return target, CompiledTarget{
		Role:       target.Role,
		Name:       targetNameForMessage(*target),
		Ordinal:    ordinal,
		Candidates: len(candidates),
	}, observed, nil
}

func narrowTarget(target Target, observed snapshot.Element, elements []snapshot.Element) (Target, bool) {
	if observed.TestID != "" {
		narrowed := target
		narrowed.TestID = observed.TestID
		if len(snapshot.RankTargetCandidates(elements, targetCriteria(narrowed))) == 1 {
			return narrowed, true
		}
	}
	if href := stableHref(observed.Href); href != "" {
		narrowed := target
		narrowed.HrefContains = href
		if len(snapshot.RankTargetCandidates(elements, targetCriteria(narrowed))) == 1 {
			return narrowed, true
		}
	}
	return target, false
}

func stableHref(href string) string {
	trimmed := strings.TrimSpace(href)
	if index := strings.IndexAny(trimmed, "?#"); index >= 0 {
		trimmed = trimmed[:index]
	}
	return trimmed
}

func findObservedElement(elements []snapshot.Element, ref string) (snapshot.Element, bool) {
	if strings.TrimSpace(ref) == "" {
		return snapshot.Element{}, false
	}
	for _, element := range elements {
		if element.Ref == ref {
			return element, true
		}
	}
	return snapshot.Element{}, false
}

func (c *compiler) inferPostcondition(before, after *TraceObservation, isWrite bool) (*Event, *Assertion, error) {
	if url := after.pageURL(); url != "" && url != before.pageURL() {
		assertion := &Assertion{Kind: browser.AssertionURL, Mode: browser.AssertModeExact, Expected: url}
		return &Event{Kind: "url.matches", Match: url, TimeoutMS: postconditionTimeoutMS}, assertion, nil
	}
	if target, ok := introducedElement(before, after); ok {

		count := 1
		asserted := target
		return &Event{Kind: "element.visible", Target: &target, TimeoutMS: postconditionTimeoutMS},
			&Assertion{Kind: browser.AssertionElementCount, Target: &asserted, Count: &count}, nil
	}
	if download, ok := newCompletedDownload(before, after); ok {
		if isWrite {
			return nil, nil, errors.New("a download is the only thing observed after this declared write; a download cannot be re-checked on a later run, so it cannot prove the write committed")
		}
		event := &Event{Kind: "download.completed", Match: download.Filename, TimeoutMS: downloadTimeoutMS}
		if download.SHA256 == "" && download.Bytes == nil {
			return event, nil, nil
		}
		return event, &Assertion{
			Kind:     browser.AssertionDownload,
			Filename: download.Filename,
			SHA256:   download.SHA256,
			Bytes:    download.Bytes,
		}, nil
	}
	if isWrite {
		return nil, nil, errors.New("the observation after this declared write shows no durable change — no new URL and no new element — so nothing can prove it committed")
	}
	return nil, nil, nil
}

func introducedElement(before, after *TraceObservation) (Target, bool) {
	existing := map[string]bool{}
	for _, element := range before.elements() {
		existing[element.Role+"\x00"+element.Name] = true
	}
	var best Target
	found := false
	for _, element := range after.elements() {
		name := strings.TrimSpace(element.Name)
		if name == "" || !element.Visible || existing[element.Role+"\x00"+element.Name] {
			continue
		}
		candidate := Target{Role: element.Role, Name: name}
		if !element.NameIsVisibleText {
			candidate = Target{Role: element.Role, NameContains: name}
		}
		if validateTarget(candidate) != nil {
			continue
		}
		if len(snapshot.RankTargetCandidates(after.elements(), targetCriteria(candidate))) != 1 {
			continue
		}
		best, found = candidate, true
		break
	}
	return best, found
}

func newCompletedDownload(before, after *TraceObservation) (TraceDownload, bool) {
	existing := map[string]bool{}
	if before != nil {
		for _, download := range before.Downloads {
			existing[download.Filename] = true
		}
	}
	for _, download := range after.Downloads {
		if !download.Completed || strings.TrimSpace(download.Filename) == "" || existing[download.Filename] {
			continue
		}
		return download, true
	}
	return TraceDownload{}, false
}

func (c *compiler) declareInput(target *Target, observed snapshot.Element) string {
	label := ""
	if target != nil {
		label = targetNameForMessage(*target)
	}
	if label == "" {
		label = observed.Name
	}
	if label == "" && target != nil {
		label = target.Role
	}
	base := slugifyInputName(label)
	name := base
	for suffix := 2; ; suffix++ {
		if _, taken := c.inputs[name]; !taken {
			break
		}
		name = fmt.Sprintf("%s_%d", base, suffix)
	}
	description := "value for the " + observedRole(target, observed) + " " + strconv.Quote(label)
	if len(description) > 500 {
		description = description[:500]
	}
	c.inputs[name] = Input{Required: true, Description: description}
	return name
}

func observedRole(target *Target, observed snapshot.Element) string {
	if target != nil && target.Role != "" {
		return target.Role
	}
	if observed.Role != "" {
		return observed.Role
	}
	return "field"
}

func slugifyInputName(label string) string {
	var out strings.Builder
	previousUnderscore := false
	for _, letter := range strings.ToLower(strings.TrimSpace(label)) {
		switch {
		case letter >= 'a' && letter <= 'z', letter >= '0' && letter <= '9':
			out.WriteRune(letter)
			previousUnderscore = false
		default:
			if !previousUnderscore && out.Len() > 0 {
				out.WriteByte('_')
				previousUnderscore = true
			}
		}
		if out.Len() >= 40 {
			break
		}
	}
	name := strings.Trim(out.String(), "_")
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		name = "value_" + name
		name = strings.TrimRight(name, "_")
	}
	if len(name) > 48 {
		name = strings.TrimRight(name[:48], "_")
	}
	return name
}

func (c *compiler) finish() (CompileResult, error) {
	value := Recipe{
		SchemaVersion:    SchemaVersion,
		ID:               c.opts.ID,
		Version:          c.opts.Version,
		Name:             strings.TrimSpace(c.opts.Name),
		Description:      strings.TrimSpace(c.opts.Description),
		Intents:          append([]string(nil), c.opts.Intents...),
		Origins:          c.origins,
		Risk:             c.opts.Risk,
		Steps:            c.steps,
		Metadata:         map[string]string{"compiled_from": "brw_trace"},
		CaptureOnFailure: c.opts.CaptureOnFailure,
	}
	if len(c.inputs) > 0 {
		value.Inputs = c.inputs
	}
	if err := Validate(value); err != nil {
		return CompileResult{}, fmt.Errorf("compiled recipe is not valid: %w", err)
	}
	if err := RequireWriteVerification(value); err != nil {
		return CompileResult{}, err
	}
	return CompileResult{Recipe: value, Targets: c.targets, Review: ReviewBody(value, c.targets)}, nil
}

func describeTarget(target Target) string {
	description := "role " + strconv.Quote(target.Role)
	if name := targetNameForMessage(target); name != "" {
		description += " named " + strconv.Quote(name)
	}
	if target.TestID != "" {
		description += " test id " + strconv.Quote(target.TestID)
	}
	if target.HrefContains != "" {
		description += " href " + strconv.Quote(target.HrefContains)
	}
	return description
}

func originOf(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

func sortedInputNames(inputs map[string]Input) []string { return slices.Sorted(maps.Keys(inputs)) }
