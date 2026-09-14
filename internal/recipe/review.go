package recipe

import (
	"fmt"
	"strconv"
	"strings"
)

// ReviewBody renders a recipe as the text a human reviews before it is
// published.
//
// JSON is what the runner executes and what the digest covers; it is not what a
// reviewer can read a change out of. Reordering two keys in a 200-line document
// produces a diff that looks like a rewrite, and a postcondition buried three
// levels down reads as punctuation. This renders one fact per line, in a fixed
// order with no map iteration anywhere, so recompiling an unchanged trace
// produces a byte-identical body and a changed trace produces a diff whose size
// matches the size of the change.
//
// It is a rendering, never an input: nothing parses it back.
func ReviewBody(value Recipe, targets []CompiledTarget) string {
	byStep := make(map[string]CompiledTarget, len(targets))
	for _, target := range targets {
		byStep[target.StepID] = target
	}

	var body strings.Builder
	fmt.Fprintf(&body, "recipe %s@%s\n", value.ID, value.Version)
	fmt.Fprintf(&body, "name %s\n", singleLine(value.Name))
	fmt.Fprintf(&body, "description %s\n", singleLine(value.Description))
	fmt.Fprintf(&body, "risk %s\n", value.Risk)
	if value.CaptureOnFailure {
		body.WriteString("capture_on_failure yes\n")
	}
	for _, origin := range value.Origins {
		fmt.Fprintf(&body, "origin %s\n", origin)
	}
	for _, intent := range value.Intents {
		fmt.Fprintf(&body, "intent %s\n", singleLine(intent))
	}
	for _, name := range sortedInputNames(value.Inputs) {
		input := value.Inputs[name]
		requirement := "optional"
		if input.Required {
			requirement = "required"
		}
		fmt.Fprintf(&body, "input %s %s", name, requirement)
		if input.Secret {
			body.WriteString(" secret")
		}
		if description := singleLine(input.Description); description != "" {
			fmt.Fprintf(&body, " — %s", description)
		}
		body.WriteByte('\n')
	}
	for index, step := range value.Steps {
		writeReviewStep(&body, index+1, step, byStep[step.ID])
	}
	return body.String()
}

func writeReviewStep(body *strings.Builder, position int, step Step, derived CompiledTarget) {
	fmt.Fprintf(body, "step %d %s %s\n", position, step.ID, step.Action)
	if step.Target != nil {
		fmt.Fprintf(body, "  target %s\n", describeTarget(*step.Target))
	}
	if derived.Ordinal > 0 {
		origin := derived.Origin
		if origin == "" {
			origin = "unrecorded origin"
		}
		fmt.Fprintf(body, "  derived candidate %d of %d on %s\n", derived.Ordinal, derived.Candidates, origin)
	}
	if step.URL != "" {
		fmt.Fprintf(body, "  url %s\n", step.URL)
	}
	if step.Value != "" {
		fmt.Fprintf(body, "  value %s\n", step.Value)
	}
	if step.Key != "" {
		fmt.Fprintf(body, "  key %s\n", step.Key)
	}
	if step.TimerMS != 0 {
		fmt.Fprintf(body, "  timer %dms\n", step.TimerMS)
	}
	if step.Effect != "" {
		fmt.Fprintf(body, "  effect %s\n", step.Effect)
	}
	if step.MaxAttempts != 0 {
		fmt.Fprintf(body, "  max_attempts %d\n", step.MaxAttempts)
	}
	if step.IdempotencyKey != "" {
		fmt.Fprintf(body, "  idempotency_key %s\n", step.IdempotencyKey)
	}
	if step.SiteIdempotency != nil {
		fmt.Fprintf(body, "  site_idempotency %s at %s\n", step.SiteIdempotency.Kind, describeTarget(*step.SiteIdempotency.Target))
	}
	if step.Event != nil {
		fmt.Fprintf(body, "  wait %s\n", describeEvent(*step.Event))
	}
	if step.Postcondition != nil {
		fmt.Fprintf(body, "  postcondition %s\n", describeEvent(*step.Postcondition))
	}
	if step.Assert != nil {
		fmt.Fprintf(body, "  assert %s\n", describeAssertion(*step.Assert))
	}
	if step.Capture != nil {
		fmt.Fprintf(body, "  capture %s\n", step.Capture.Kind)
	}
}

func describeEvent(event Event) string {
	description := event.Kind
	if event.Match != "" {
		description += " " + strconv.Quote(event.Match)
	}
	if event.Target != nil {
		description += " on " + describeTarget(*event.Target)
	}
	return fmt.Sprintf("%s within %dms", description, event.TimeoutMS)
}

func describeAssertion(assertion Assertion) string {
	parts := []string{assertion.Kind}
	if assertion.Mode != "" {
		parts = append(parts, assertion.Mode)
	}
	if assertion.Expected != "" {
		parts = append(parts, strconv.Quote(assertion.Expected))
	}
	if assertion.Attribute != "" {
		parts = append(parts, "attribute "+strconv.Quote(assertion.Attribute))
	}
	if assertion.Status != 0 {
		parts = append(parts, "status "+strconv.Itoa(assertion.Status))
	}
	if assertion.State != "" {
		state := assertion.State
		if assertion.Negate {
			state = "not " + state
		}
		parts = append(parts, state)
	}
	if assertion.Count != nil {
		parts = append(parts, "count "+strconv.Itoa(*assertion.Count))
	}
	if assertion.Min != nil {
		parts = append(parts, "min "+strconv.Itoa(*assertion.Min))
	}
	if assertion.Max != nil {
		parts = append(parts, "max "+strconv.Itoa(*assertion.Max))
	}
	if assertion.Filename != "" {
		parts = append(parts, "file "+strconv.Quote(assertion.Filename))
	}
	if assertion.SHA256 != "" {
		parts = append(parts, "sha256 "+assertion.SHA256)
	}
	if assertion.Bytes != nil {
		parts = append(parts, "bytes "+strconv.FormatInt(*assertion.Bytes, 10))
	}
	if assertion.Target != nil {
		parts = append(parts, "on "+describeTarget(*assertion.Target))
	}
	return strings.Join(parts, " ")
}

// singleLine keeps one reviewed fact on one reviewed line. A description
// carrying a newline would otherwise turn into two lines that look like two
// facts, one of them unlabelled.
func singleLine(value string) string {
	replacer := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ")
	return strings.TrimSpace(replacer.Replace(value))
}
