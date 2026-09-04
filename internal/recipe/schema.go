// Package recipe defines brw's public, deterministic recipe ABI. The package
// contains no recipe corpus: executable recipes are fetched from an operator-
// controlled Provider and pinned by immutable id, version, and digest.
package recipe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

const SchemaVersion = 1

type Recipe struct {
	SchemaVersion int               `json:"schema_version"`
	ID            string            `json:"id"`
	Version       string            `json:"version"`
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	Intents       []string          `json:"intents"`
	Origins       []string          `json:"origins"`
	Risk          string            `json:"risk"`
	Inputs        map[string]Input  `json:"inputs,omitempty"`
	Steps         []Step            `json:"steps"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

type Input struct {
	Secret      bool   `json:"secret,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
}

type Step struct {
	ID             string       `json:"id"`
	Action         string       `json:"action"`
	Target         *Target      `json:"target,omitempty"`
	Value          string       `json:"value,omitempty"`
	Key            string       `json:"key,omitempty"`
	URL            string       `json:"url,omitempty"`
	Event          *Event       `json:"event,omitempty"`
	TimerMS        int          `json:"timer_ms,omitempty"`
	Capture        *CaptureSpec `json:"capture,omitempty"`
	Effect         string       `json:"effect,omitempty"`
	MaxAttempts    int          `json:"max_attempts,omitempty"`
	IdempotencyKey string       `json:"idempotency_key,omitempty"`
	Postcondition  *Event       `json:"postcondition,omitempty"`
}

// Target is resolved immediately before every action. Observation refs are
// intentionally absent because their lifetime is one page state, not a recipe.
type Target struct {
	Role         string `json:"role"`
	Name         string `json:"name,omitempty"`
	NameContains string `json:"name_contains,omitempty"`
	TestID       string `json:"test_id,omitempty"`
	HrefContains string `json:"href_contains,omitempty"`
	Visible      *bool  `json:"visible,omitempty"`
}

type Event struct {
	Kind      string  `json:"kind"`
	Match     string  `json:"match,omitempty"`
	Target    *Target `json:"target,omitempty"`
	TimeoutMS int     `json:"timeout_ms"`
}

type CaptureSpec struct {
	Kind   string  `json:"kind"`
	Target *Target `json:"target,omitempty"`
	// Ref is retained only so strict parsing can return a useful validation
	// error for old drafts. Persisted observation refs are never executable.
	Ref          string `json:"ref,omitempty"`
	Redaction    string `json:"redaction,omitempty"`
	TTLSeconds   int    `json:"ttl_seconds,omitempty"`
	DurationMS   int    `json:"duration_ms,omitempty"`
	FPS          int    `json:"fps,omitempty"`
	DownloadGUID string `json:"download_guid,omitempty"`
	Filename     string `json:"filename,omitempty"`
}

var (
	recipeIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)+$`)
	versionPattern  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][a-zA-Z0-9.-]+)?$`)
	stepIDPattern   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	inputPattern    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	refPattern      = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(?:e[0-9]+|f[0-9]+:e[0-9]+)(?:$|[^a-z0-9])`)
	secretPattern   = regexp.MustCompile(`(?i)(?:password|passwd|api[_-]?key|bearer|secret|token)\s*[:=]\s*[^$][^\s]{5,}`)
	inputTemplate   = regexp.MustCompile(`\$\{input:([a-z][a-z0-9_-]{0,63})\}`)
)

func Parse(data []byte) (Recipe, error) {
	if len(data) > 1<<20 {
		return Recipe{}, errors.New("recipe exceeds 1 MiB")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var value Recipe
	if err := dec.Decode(&value); err != nil {
		return Recipe{}, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Recipe{}, errors.New("recipe contains trailing JSON")
		}
		return Recipe{}, fmt.Errorf("recipe contains trailing JSON: %w", err)
	}
	if err := Validate(value); err != nil {
		return Recipe{}, err
	}
	return value, nil
}

func Validate(value Recipe) error {
	var problems []error
	if value.SchemaVersion != SchemaVersion {
		problems = append(problems, fmt.Errorf("schema_version must be %d", SchemaVersion))
	}
	if !recipeIDPattern.MatchString(value.ID) {
		problems = append(problems, errors.New("id must be a namespaced lowercase identifier"))
	}
	if !versionPattern.MatchString(value.Version) {
		problems = append(problems, errors.New("version must be semantic x.y.z"))
	}
	if strings.TrimSpace(value.Name) == "" || strings.TrimSpace(value.Description) == "" {
		problems = append(problems, errors.New("name and description are required"))
	}
	if len(value.Name) > 200 || len(value.Description) > 2000 {
		problems = append(problems, errors.New("name or description is too long"))
	}
	if len(value.Intents) < 1 || len(value.Intents) > 32 {
		problems = append(problems, errors.New("one to 32 intents are required"))
	}
	if len(value.Origins) == 0 || len(value.Origins) > 32 {
		problems = append(problems, errors.New("one to 32 exact origins are required"))
	}
	for _, origin := range value.Origins {
		if err := validateOrigin(origin); err != nil {
			problems = append(problems, err)
		}
	}
	if value.Risk != "read_only" && value.Risk != "external_write" {
		problems = append(problems, errors.New("risk must be read_only or external_write"))
	}
	if len(value.Inputs) > 64 {
		problems = append(problems, errors.New("at most 64 inputs are allowed"))
	}
	for name := range value.Inputs {
		if !inputPattern.MatchString(name) {
			problems = append(problems, fmt.Errorf("invalid input name %q", name))
		}
		if len(value.Inputs[name].Description) > 500 {
			problems = append(problems, fmt.Errorf("input %q description is too long", name))
		}
	}
	if len(value.Steps) < 1 || len(value.Steps) > 500 {
		problems = append(problems, errors.New("one to 500 bounded steps are required"))
	}
	seen := map[string]bool{}
	for index, step := range value.Steps {
		if err := validateStep(value, step, seen); err != nil {
			problems = append(problems, fmt.Errorf("step %d: %w", index+1, err))
		}
		seen[step.ID] = true
	}
	encoded, _ := json.Marshal(value)
	if len(encoded) > 1<<20 {
		problems = append(problems, errors.New("recipe exceeds 1 MiB"))
	}
	if refPattern.Match(encoded) {
		problems = append(problems, errors.New("persisted observation refs are forbidden; use a semantic target"))
	}
	if secretPattern.Match(encoded) {
		problems = append(problems, errors.New("recipe appears to contain a literal secret"))
	}
	return errors.Join(problems...)
}

func validateStep(recipe Recipe, step Step, seen map[string]bool) error {
	var problems []error
	if !stepIDPattern.MatchString(step.ID) || seen[step.ID] {
		problems = append(problems, errors.New("step id must be unique and stable"))
	}
	allowed := []string{"click", "fill", "type", "select", "press", "navigate_to", "wait_event", "timer", "capture"}
	if !slices.Contains(allowed, step.Action) {
		problems = append(problems, fmt.Errorf("unsupported action %q", step.Action))
	}
	switch step.Action {
	case "click", "fill", "type", "select":
		if step.Target == nil {
			problems = append(problems, errors.New("semantic target is required"))
		} else if err := validateTarget(*step.Target); err != nil {
			problems = append(problems, err)
		} else if err := validateTargetTemplates(*step.Target, recipe.Inputs); err != nil {
			problems = append(problems, err)
		}
		if step.Action != "click" {
			problems = append(problems, validateTemplates(step.Value, recipe.Inputs))
		}
	case "press":
		if strings.TrimSpace(step.Key) == "" {
			problems = append(problems, errors.New("press key is required"))
		}
		if len(step.Key) > 100 {
			problems = append(problems, errors.New("press key is too long"))
		}
		if step.Target == nil {
			problems = append(problems, errors.New("press requires a semantic focus target"))
		} else if err := validateTarget(*step.Target); err != nil {
			problems = append(problems, err)
		} else if err := validateTargetTemplates(*step.Target, recipe.Inputs); err != nil {
			problems = append(problems, err)
		}
	case "navigate_to":
		if err := validateNavigationURL(step.URL, recipe.Origins, recipe.Inputs); err != nil {
			problems = append(problems, err)
		}
	case "wait_event":
		if step.Event == nil {
			problems = append(problems, errors.New("event is required"))
		} else if err := validateEvent(*step.Event, recipe.Inputs); err != nil {
			problems = append(problems, err)
		} else if slices.Contains([]string{"download.completed", "tab.opened", "network.response"}, step.Event.Kind) {
			problems = append(problems, errors.New("transient browser events must be a postcondition on the action that causes them so observation can be armed first"))
		}
	case "timer":
		if step.TimerMS < 1 || step.TimerMS > 60_000 {
			problems = append(problems, errors.New("timer_ms must be between 1 and 60000"))
		}
	case "capture":
		if step.Capture == nil {
			problems = append(problems, errors.New("capture spec is required"))
		} else if err := validateCapture(*step.Capture, recipe.Inputs); err != nil {
			problems = append(problems, err)
		}
	}
	actuation := slices.Contains([]string{"click", "fill", "type", "select", "press", "navigate_to"}, step.Action)
	if actuation && step.Effect == "" {
		problems = append(problems, errors.New("browser actions must explicitly declare effect read or external_write"))
	}
	if !actuation && (step.Effect != "" || step.MaxAttempts != 0 || step.IdempotencyKey != "" || step.Postcondition != nil) {
		problems = append(problems, errors.New("effect, retries, idempotency_key and postcondition are only valid on browser actions"))
	}
	if !slices.Contains([]string{"click", "fill", "type", "select", "press"}, step.Action) && step.Target != nil {
		problems = append(problems, errors.New("target is not valid for this action"))
	}
	if !slices.Contains([]string{"fill", "type", "select"}, step.Action) && step.Value != "" {
		problems = append(problems, errors.New("value is not valid for this action"))
	}
	if step.Action != "press" && step.Key != "" {
		problems = append(problems, errors.New("key is only valid for press"))
	}
	if step.Action != "navigate_to" && step.URL != "" {
		problems = append(problems, errors.New("url is only valid for navigate_to"))
	}
	if step.Action != "wait_event" && step.Event != nil {
		problems = append(problems, errors.New("event is only valid for wait_event"))
	}
	if step.Action != "timer" && step.TimerMS != 0 {
		problems = append(problems, errors.New("timer_ms is only valid for timer"))
	}
	if step.Action != "capture" && step.Capture != nil {
		problems = append(problems, errors.New("capture is only valid for capture"))
	}
	if step.Effect != "" && step.Effect != "read" && step.Effect != "external_write" {
		problems = append(problems, errors.New("effect must be read or external_write"))
	}
	if step.MaxAttempts < 0 {
		problems = append(problems, errors.New("max_attempts must be one to three when set"))
	}
	attempts := max(1, step.MaxAttempts)
	if attempts > 3 {
		problems = append(problems, errors.New("max_attempts must be one to three"))
	}
	if step.Postcondition != nil {
		if err := validateEvent(*step.Postcondition, recipe.Inputs); err != nil {
			problems = append(problems, fmt.Errorf("invalid postcondition: %w", err))
		}
	}
	if step.IdempotencyKey != "" {
		if err := validateTemplates(step.IdempotencyKey, recipe.Inputs); err != nil {
			problems = append(problems, fmt.Errorf("invalid idempotency_key: %w", err))
		}
	}
	if step.Effect == "external_write" || attempts > 1 {
		if step.Effect == "external_write" && recipe.Risk != "external_write" {
			problems = append(problems, errors.New("external_write step requires recipe risk external_write"))
		}
		if strings.TrimSpace(step.IdempotencyKey) == "" || step.Postcondition == nil {
			problems = append(problems, errors.New("retries/writes require idempotency_key and postcondition"))
		}
	}
	if step.Effect == "external_write" && attempts > 1 {
		problems = append(problems, errors.New("external_write actions may not be automatically retried; use the postcondition to reconcile an ambiguous acknowledgement"))
	}
	if step.Effect == "external_write" && step.Postcondition != nil && step.Postcondition.Kind == "page.ready" {
		problems = append(problems, errors.New("page.ready cannot prove an external write; use a durable state-specific postcondition"))
	}
	if step.Effect == "external_write" && step.Postcondition != nil && slices.Contains([]string{"network.response", "download.completed", "tab.opened"}, step.Postcondition.Kind) {
		problems = append(problems, errors.New("external_write requires a durable state postcondition that can be preflighted on rerun; transient events cannot prevent duplicate writes"))
	}
	return errors.Join(problems...)
}

func validateTarget(target Target) error {
	if strings.TrimSpace(target.Role) == "" {
		return errors.New("target role is required")
	}
	selectors := 0
	for _, value := range []string{target.Name, target.NameContains, target.TestID, target.HrefContains} {
		if strings.TrimSpace(value) != "" {
			selectors++
		}
	}
	if selectors == 0 {
		return errors.New("target needs an accessible name or stable attribute")
	}
	if target.Name != "" && target.NameContains != "" {
		return errors.New("target name and name_contains are mutually exclusive")
	}
	if len(target.Role) > 100 || len(target.Name) > 1000 || len(target.NameContains) > 1000 || len(target.TestID) > 500 || len(target.HrefContains) > 2000 {
		return errors.New("target field is too long")
	}
	return nil
}

func validateTargetTemplates(target Target, inputs map[string]Input) error {
	if err := validateTemplates(target.Role, inputs); err != nil {
		return fmt.Errorf("invalid target role: %w", err)
	}
	if inputTemplate.MatchString(target.Role) {
		return errors.New("target role may not interpolate inputs")
	}
	for name, value := range map[string]string{
		"name":          target.Name,
		"name_contains": target.NameContains,
		"test_id":       target.TestID,
		"href_contains": target.HrefContains,
	} {
		if err := validateTemplates(value, inputs); err != nil {
			return fmt.Errorf("invalid target %s: %w", name, err)
		}
	}
	return nil
}

func validateEvent(event Event, inputs map[string]Input) error {
	return validateEventValue(event, inputs, true)
}

// validateExpandedEvent checks the same bounded event shape after runtime
// interpolation without parsing the resulting user data as recipe syntax a
// second time. Expansion is deliberately single-pass.
func validateExpandedEvent(event Event) error {
	return validateEventValue(event, nil, false)
}

func validateEventValue(event Event, inputs map[string]Input, checkTemplates bool) error {
	allowed := []string{"page.ready", "url.matches", "text.present", "text.absent", "element.visible", "element.hidden", "element.value", "element.value_contains", "download.completed", "tab.opened", "network.response"}
	if !slices.Contains(allowed, event.Kind) {
		return fmt.Errorf("unsupported event %q", event.Kind)
	}
	if event.TimeoutMS < 1 || event.TimeoutMS > 120_000 {
		return errors.New("event timeout_ms must be between 1 and 120000")
	}
	if strings.HasPrefix(event.Kind, "element.") {
		if event.Target == nil {
			return errors.New("element event requires a target")
		}
		valueEvent := event.Kind == "element.value" || event.Kind == "element.value_contains"
		if !valueEvent && event.Match != "" {
			return errors.New("element event may not also set match")
		}
		if !valueEvent && event.Target.Visible != nil {
			return errors.New("element event target must not set visible; the event kind defines visibility")
		}
		if event.Kind == "element.value_contains" && strings.TrimSpace(event.Match) == "" {
			return errors.New("element.value_contains requires a non-empty match")
		}
		if len(event.Match) > 2000 {
			return errors.New("event match is too long")
		}
		if checkTemplates {
			if err := validateTemplates(event.Match, inputs); err != nil {
				return err
			}
		}
		if err := validateTarget(*event.Target); err != nil {
			return err
		}
		if checkTemplates {
			return validateTargetTemplates(*event.Target, inputs)
		}
		return nil
	}
	if event.Target != nil {
		return errors.New("target is only valid for element events")
	}
	if event.Kind == "page.ready" {
		if event.Match != "" {
			return errors.New("page.ready may not set match")
		}
		return nil
	}
	if strings.TrimSpace(event.Match) == "" {
		return errors.New("event match is required")
	}
	if len(event.Match) > 2000 {
		return errors.New("event match is too long")
	}
	if checkTemplates {
		if err := validateTemplates(event.Match, inputs); err != nil {
			return err
		}
	}
	return nil
}

func validateCapture(capture CaptureSpec, inputs map[string]Input) error {
	if !slices.Contains([]string{"text", "semantic_json", "screenshot", "pdf", "video", "download"}, capture.Kind) {
		return errors.New("unsupported capture kind")
	}
	if capture.TTLSeconds < 0 || capture.TTLSeconds > 7*24*60*60 {
		return errors.New("ttl_seconds must be zero or at most seven days")
	}
	if capture.Ref != "" {
		return errors.New("capture ref is ephemeral; use a semantic screenshot target")
	}
	if len(capture.Redaction) > 256 {
		return errors.New("capture redaction label is too long")
	}
	if capture.Kind == "screenshot" {
		if capture.Target != nil {
			if err := validateTarget(*capture.Target); err != nil {
				return fmt.Errorf("invalid screenshot target: %w", err)
			}
			if err := validateTargetTemplates(*capture.Target, inputs); err != nil {
				return fmt.Errorf("invalid screenshot target: %w", err)
			}
		}
	} else if capture.Target != nil {
		return errors.New("capture target is only valid for screenshots")
	}
	if capture.Kind == "video" {
		if capture.DurationMS < 100 || capture.DurationMS > 30_000 || capture.FPS < 1 || capture.FPS > 30 {
			return errors.New("video capture requires duration_ms 100..30000 and fps 1..30")
		}
		if (capture.DurationMS*capture.FPS+999)/1000 > 300 {
			return errors.New("video capture is limited to 300 frames; reduce duration_ms or fps")
		}
	} else if capture.DurationMS != 0 || capture.FPS != 0 {
		return errors.New("duration_ms/fps are only valid for video capture")
	}
	if capture.Kind == "download" {
		if (capture.DownloadGUID == "") == (capture.Filename == "") {
			return errors.New("download capture requires exactly one of download_guid or filename")
		}
		if len(capture.DownloadGUID) > 500 || len(capture.Filename) > 1000 {
			return errors.New("download identifier is too long")
		}
	} else if capture.DownloadGUID != "" || capture.Filename != "" {
		return errors.New("download_guid/filename are only valid for download capture")
	}
	return nil
}

func validateOrigin(raw string) error {
	if strings.ContainsAny(raw, "*? ") {
		return fmt.Errorf("origin %q must be exact", raw)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return fmt.Errorf("origin %q is invalid", raw)
	}
	if parsed.Scheme == "https" {
		return nil
	}
	host := parsed.Hostname()
	if parsed.Scheme == "http" && (host == "127.0.0.1" || host == "localhost" || host == "::1") {
		return nil
	}
	return fmt.Errorf("origin %q must use HTTPS (except loopback fixtures)", raw)
}

func validateNavigationURL(raw string, origins []string, inputs map[string]Input) error {
	if len(raw) > 8<<10 {
		return errors.New("navigate_to URL is too long")
	}
	if err := validateTemplates(raw, inputs); err != nil {
		return err
	}
	if inputTemplate.MatchString(raw) {
		return errors.New("navigate_to URL may not interpolate inputs")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return errors.New("navigate_to requires an absolute URL")
	}
	origin := parsed.Scheme + "://" + parsed.Host
	if !slices.Contains(origins, origin) {
		return errors.New("navigate_to URL origin is not in the recipe allowlist")
	}
	return nil
}

func validateTemplates(value string, inputs map[string]Input) error {
	matches := inputTemplate.FindAllStringSubmatch(value, -1)
	without := inputTemplate.ReplaceAllString(value, "")
	if strings.Contains(without, "${") {
		return errors.New("value contains an invalid template")
	}
	for _, match := range matches {
		if _, ok := inputs[match[1]]; !ok {
			return fmt.Errorf("value references undeclared input %q", match[1])
		}
	}
	return nil
}

func Expand(value string, inputs map[string]string) (string, error) {
	var expansionErr error
	result := inputTemplate.ReplaceAllStringFunc(value, func(token string) string {
		name := inputTemplate.FindStringSubmatch(token)[1]
		resolved, ok := inputs[name]
		if !ok {
			expansionErr = fmt.Errorf("input %q is missing", name)
			return ""
		}
		return resolved
	})
	return result, expansionErr
}

func Digest(value Recipe) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
