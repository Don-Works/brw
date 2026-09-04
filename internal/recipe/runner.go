package recipe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
)

type ResolvedElement struct {
	Ref  string `json:"ref"`
	Role string `json:"role"`
	Name string `json:"name"`
}

// Surface is the deliberately narrow deterministic browser boundary. Its
// implementation resolves semantic targets to ephemeral refs at execution time.
type Surface interface {
	Origin(context.Context) (string, error)
	Resolve(context.Context, Target) ([]ResolvedElement, error)
	Click(context.Context, string) error
	Fill(context.Context, string, string) error
	Type(context.Context, string, string) error
	Select(context.Context, string, string) error
	Press(context.Context, string, string) error
	NavigateTo(context.Context, string) error
	WaitEvent(context.Context, Event) error
	Capture(context.Context, CaptureSpec) (artifact.Meta, error)
}

// EventArmer is an optional stronger event contract. A browser surface uses it
// to install network/download/tab observation before an action, eliminating the
// race where a synchronous response happens before a postcondition starts.
type EventArmer interface {
	ArmEvent(context.Context, Event) (func(context.Context) error, error)
}

// EventProber checks durable desired state without waiting. External writes use
// it before actuation: if the pinned postcondition already holds, the recipe is
// an idempotent no-op rather than a duplicate write. Transient events always
// probe false because only a newly armed occurrence can acknowledge an action.
type EventProber interface {
	EventSatisfied(context.Context, Event) (bool, error)
}

type Clock interface {
	Sleep(context.Context, time.Duration) error
}

type RealClock struct{}

func (RealClock) Sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type RunRequest struct {
	ID      string            `json:"id"`
	Version string            `json:"version"`
	Digest  string            `json:"digest"`
	Inputs  map[string]string `json:"inputs,omitempty"`
}

type RunResult struct {
	RecipeID      string          `json:"recipe_id"`
	RecipeVersion string          `json:"recipe_version"`
	RecipeDigest  string          `json:"recipe_digest"`
	Status        string          `json:"status"`
	StartedAt     time.Time       `json:"started_at"`
	DurationMS    int64           `json:"duration_ms"`
	Steps         []StepResult    `json:"steps"`
	Artifacts     []artifact.Meta `json:"artifacts,omitempty"`
}

type StepResult struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Attempts   int    `json:"attempts,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

type Runner struct {
	Surface     Surface
	Clock       Clock
	MaxDuration time.Duration
}

// DefaultMaxRunDuration is shared with remote transports so their connection
// deadline never cuts off a recipe that the runner itself still permits.
const DefaultMaxRunDuration = 30 * time.Minute

func (r Runner) Run(ctx context.Context, value Recipe, inputs map[string]string) (result RunResult, runErr error) {
	if err := Validate(value); err != nil {
		return RunResult{}, err
	}
	if r.Surface == nil {
		return RunResult{}, errors.New("recipe runner needs a browser surface")
	}
	if r.Clock == nil {
		r.Clock = RealClock{}
	}
	if r.MaxDuration == 0 {
		r.MaxDuration = DefaultMaxRunDuration
	}
	if r.MaxDuration < time.Second {
		return RunResult{}, errors.New("recipe runner max duration must be at least one second")
	}
	runCtx, cancel := context.WithTimeout(ctx, r.MaxDuration)
	defer cancel()
	if err := validateInputs(value, inputs); err != nil {
		return RunResult{}, err
	}
	// Expand and revalidate the ENTIRE runtime plan before step one. Otherwise a
	// missing optional input or an expansion that erases/overgrows a selector in
	// a later step could fail only after earlier browser actions had already run.
	if err := preflightRuntimePlan(value, inputs); err != nil {
		return RunResult{}, err
	}
	digest, err := Digest(value)
	if err != nil {
		return RunResult{}, err
	}
	started := time.Now()
	result = RunResult{
		RecipeID: value.ID, RecipeVersion: value.Version, RecipeDigest: digest,
		Status: "running", StartedAt: started.UTC(), Steps: make([]StepResult, 0, len(value.Steps)),
	}
	defer func() { result.DurationMS = time.Since(started).Milliseconds() }()
	for _, step := range value.Steps {
		stepStarted := time.Now()
		stepResult := StepResult{ID: step.ID, Status: "running"}
		attempts, err := r.runStep(runCtx, value, step, inputs, &result)
		if err == nil {
			// A click, navigation, timer, or wait may finish after the page has
			// crossed origins. Never report a successful final step on a page the
			// pinned recipe was not allowed to operate.
			err = r.checkOrigin(runCtx, value.Origins)
		}
		stepResult.Attempts = attempts
		stepResult.DurationMS = time.Since(stepStarted).Milliseconds()
		if err != nil {
			stepResult.Status = "failed"
			result.Steps = append(result.Steps, stepResult)
			result.Status = "failed"
			return result, fmt.Errorf("step %q: %w", step.ID, redactInputs(err, inputs))
		}
		stepResult.Status = "done"
		result.Steps = append(result.Steps, stepResult)
	}
	result.Status = "done"
	return result, nil
}

func (r Runner) runStep(ctx context.Context, value Recipe, step Step, inputs map[string]string, result *RunResult) (int, error) {
	ctx = browser.WithAllowedOrigins(ctx, value.Origins)
	// Sensitivity follows every runtime template the step can send into browser
	// inspection or actuation, including event and postcondition matches. This is
	// deliberately applied before the action-specific branch so a standalone
	// wait_event cannot leak a declared secret through transport tracing either.
	if stepUsesSecret(step, value.Inputs) {
		ctx = browser.WithSensitiveAction(ctx)
	}
	if err := r.checkOrigin(ctx, value.Origins); err != nil {
		return 0, err
	}
	switch step.Action {
	case "timer":
		return 1, r.Clock.Sleep(ctx, time.Duration(step.TimerMS)*time.Millisecond)
	case "wait_event":
		event, err := expandEvent(*step.Event, inputs)
		if err != nil {
			return 0, err
		}
		return 1, r.Surface.WaitEvent(ctx, event)
	case "capture":
		capture := *step.Capture
		if capture.Target != nil {
			target, err := expandTarget(*capture.Target, inputs)
			if err != nil {
				return 0, err
			}
			matches, err := r.Surface.Resolve(ctx, target)
			if err != nil {
				return 0, err
			}
			if len(matches) != 1 {
				return 0, fmt.Errorf("semantic capture target resolved to %d elements; refusing to guess", len(matches))
			}
			// Resolving a semantic target executes against live page state. Pin the
			// ephemeral ref only after resolution, then re-check the exact origin
			// before allowing capture so a navigation race cannot persist bytes from
			// a page outside this recipe's allowlist.
			capture.Ref = matches[0].Ref
			capture.Target = nil
			if err := r.checkOrigin(ctx, value.Origins); err != nil {
				return 0, err
			}
		}
		meta, err := r.Surface.Capture(ctx, capture)
		if err == nil {
			result.Artifacts = append(result.Artifacts, meta)
		}
		return 1, err
	case "click", "fill", "type", "select", "press", "navigate_to":
		return r.runActuation(ctx, value, step, inputs)
	default:
		return 0, fmt.Errorf("unsupported action %q", step.Action)
	}
}

func (r Runner) runActuation(ctx context.Context, recipe Recipe, step Step, inputs map[string]string) (int, error) {
	origins := recipe.Origins
	attempts := max(1, step.MaxAttempts)
	if step.IdempotencyKey != "" {
		key, err := Expand(step.IdempotencyKey, inputs)
		if err != nil {
			return 0, err
		}
		if strings.TrimSpace(key) == "" {
			return 0, errors.New("idempotency key resolved empty")
		}
	}
	var postcondition *Event
	if step.Postcondition != nil {
		expanded, err := expandEvent(*step.Postcondition, inputs)
		if err != nil {
			return 0, err
		}
		postcondition = &expanded
	}
	var target *Target
	if step.Target != nil {
		expanded, err := expandTarget(*step.Target, inputs)
		if err != nil {
			return 0, err
		}
		target = &expanded
	}
	if step.Effect == "external_write" {
		prober, ok := r.Surface.(EventProber)
		if !ok {
			return 0, errors.New("external_write execution requires postcondition preflight support")
		}
		satisfied, err := prober.EventSatisfied(ctx, *postcondition)
		if err != nil {
			return 0, fmt.Errorf("preflight postcondition: %w", err)
		}
		if satisfied {
			// Zero attempts is intentional and visible in the result: the desired
			// state was already present, so no browser write was issued.
			return 0, nil
		}
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := r.checkOrigin(ctx, origins); err != nil {
			return attempt, err
		}
		ref := ""
		if target != nil {
			matches, err := r.Surface.Resolve(ctx, *target)
			if err != nil {
				return attempt, err
			}
			if len(matches) != 1 {
				return attempt, fmt.Errorf("semantic target resolved to %d elements; refusing to guess", len(matches))
			}
			ref = matches[0].Ref
		}
		// Resolution can execute page code and race a navigation. Re-check the
		// exact origin at the last possible point before every actuation.
		if err := r.checkOrigin(ctx, origins); err != nil {
			return attempt, err
		}
		var awaitEvent func(context.Context) error
		if postcondition != nil {
			if armer, ok := r.Surface.(EventArmer); ok {
				var err error
				awaitEvent, err = armer.ArmEvent(ctx, *postcondition)
				if err != nil {
					return attempt, fmt.Errorf("arm postcondition: %w", err)
				}
			} else if transientEvent(postcondition.Kind) {
				return attempt, errors.New("transient postcondition requires pre-arm support")
			}
			// Arming may cross an asynchronous browser boundary. Do not actuate if
			// the request expired or the page changed origin while the waiter was
			// being installed.
			if err := ctx.Err(); err != nil {
				return attempt, err
			}
			if err := r.checkOrigin(ctx, origins); err != nil {
				return attempt, err
			}
		}
		switch step.Action {
		case "click":
			lastErr = r.Surface.Click(ctx, ref)
		case "fill", "type", "select":
			value, err := Expand(step.Value, inputs)
			if err != nil {
				return attempt, err
			}
			switch step.Action {
			case "fill":
				lastErr = r.Surface.Fill(ctx, ref, value)
			case "type":
				lastErr = r.Surface.Type(ctx, ref, value)
			case "select":
				lastErr = r.Surface.Select(ctx, ref, value)
			}
		case "press":
			lastErr = r.Surface.Press(ctx, ref, step.Key)
		case "navigate_to":
			lastErr = r.Surface.NavigateTo(ctx, step.URL)
		}
		acknowledged := postcondition == nil && lastErr == nil
		if postcondition != nil {
			wait := awaitEvent
			if wait == nil {
				wait = func(waitCtx context.Context) error { return r.Surface.WaitEvent(waitCtx, *postcondition) }
			}
			if postErr := wait(ctx); postErr == nil {
				// Reconciles "action committed but acknowledgement was lost"
				// without issuing the external write a second time.
				acknowledged = true
			} else {
				lastErr = errors.Join(lastErr, postErr)
			}
		}
		if acknowledged {
			return attempt, nil
		}
	}
	return attempts, lastErr
}

func transientEvent(kind string) bool {
	return kind == "network.response" || kind == "download.completed" || kind == "tab.opened"
}

func stepUsesSecret(step Step, declared map[string]Input) bool {
	values := []string{step.Value, step.IdempotencyKey}
	values = appendTargetTemplateValues(values, step.Target)
	if step.Event != nil {
		values = append(values, step.Event.Match)
		values = appendTargetTemplateValues(values, step.Event.Target)
	}
	if step.Postcondition != nil {
		values = append(values, step.Postcondition.Match)
		values = appendTargetTemplateValues(values, step.Postcondition.Target)
	}
	if step.Capture != nil {
		values = appendTargetTemplateValues(values, step.Capture.Target)
	}
	for _, value := range values {
		for _, match := range inputTemplate.FindAllStringSubmatch(value, -1) {
			if declared[match[1]].Secret {
				return true
			}
		}
	}
	return false
}

func appendTargetTemplateValues(values []string, target *Target) []string {
	if target == nil {
		return values
	}
	return append(values, target.Name, target.NameContains, target.TestID, target.HrefContains)
}

func expandTarget(target Target, inputs map[string]string) (Target, error) {
	fields := []*string{&target.Name, &target.NameContains, &target.TestID, &target.HrefContains}
	for _, field := range fields {
		expanded, err := Expand(*field, inputs)
		if err != nil {
			return Target{}, err
		}
		*field = expanded
	}
	if err := validateTarget(target); err != nil {
		return Target{}, fmt.Errorf("expanded target is invalid: %w", err)
	}
	return target, nil
}

func expandEvent(event Event, inputs map[string]string) (Event, error) {
	match, err := Expand(event.Match, inputs)
	if err != nil {
		return Event{}, err
	}
	event.Match = match
	if event.Target != nil {
		target, err := expandTarget(*event.Target, inputs)
		if err != nil {
			return Event{}, err
		}
		event.Target = &target
	}
	if err := validateExpandedEvent(event); err != nil {
		return Event{}, fmt.Errorf("expanded event is invalid: %w", err)
	}
	return event, nil
}

func preflightRuntimePlan(value Recipe, inputs map[string]string) error {
	for index, step := range value.Steps {
		fail := func(err error) error {
			return fmt.Errorf("step %d %q runtime expansion: %w", index+1, step.ID, err)
		}
		if step.Target != nil {
			if _, err := expandTarget(*step.Target, inputs); err != nil {
				return fail(err)
			}
		}
		if step.Action == "fill" || step.Action == "type" || step.Action == "select" {
			if _, err := Expand(step.Value, inputs); err != nil {
				return fail(err)
			}
		}
		if step.IdempotencyKey != "" {
			key, err := Expand(step.IdempotencyKey, inputs)
			if err != nil {
				return fail(err)
			}
			if strings.TrimSpace(key) == "" {
				return fail(errors.New("idempotency key resolved empty"))
			}
			if len(key) > 8<<10 {
				return fail(errors.New("expanded idempotency key is too long"))
			}
		}
		if step.Event != nil {
			if _, err := expandEvent(*step.Event, inputs); err != nil {
				return fail(err)
			}
		}
		if step.Postcondition != nil {
			if _, err := expandEvent(*step.Postcondition, inputs); err != nil {
				return fail(err)
			}
		}
		if step.Capture != nil && step.Capture.Target != nil {
			if _, err := expandTarget(*step.Capture.Target, inputs); err != nil {
				return fail(err)
			}
		}
	}
	return nil
}

type redactedInputError struct {
	err     error
	message string
}

func (e redactedInputError) Error() string { return e.message }
func (e redactedInputError) Unwrap() error { return e.err }

func redactInputs(err error, inputs map[string]string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	type pair struct{ name, value string }
	pairs := make([]pair, 0, len(inputs))
	for name, value := range inputs {
		if value != "" {
			pairs = append(pairs, pair{name: name, value: value})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return len(pairs[i].value) > len(pairs[j].value) })
	for _, input := range pairs {
		replacement := "[redacted input:" + input.name + "]"
		if len(input.value) >= 4 {
			message = strings.ReplaceAll(message, input.value, replacement)
		} else {
			message = replaceBounded(message, input.value, replacement)
		}
	}
	if message == err.Error() {
		return err
	}
	return redactedInputError{err: err, message: message}
}

func replaceBounded(message, value, replacement string) string {
	if value == "" {
		return message
	}
	var out strings.Builder
	for cursor := 0; cursor < len(message); {
		index := strings.Index(message[cursor:], value)
		if index < 0 {
			out.WriteString(message[cursor:])
			break
		}
		index += cursor
		end := index + len(value)
		leftBounded := index == 0 || !inputWordByte(message[index-1])
		rightBounded := end == len(message) || !inputWordByte(message[end])
		out.WriteString(message[cursor:index])
		if leftBounded && rightBounded {
			out.WriteString(replacement)
		} else {
			out.WriteString(value)
		}
		cursor = end
	}
	return out.String()
}

func inputWordByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '_'
}

func (r Runner) checkOrigin(ctx context.Context, allowed []string) error {
	origin, err := r.Surface.Origin(ctx)
	if err != nil {
		return err
	}
	for _, candidate := range allowed {
		if origin == candidate {
			return nil
		}
	}
	return fmt.Errorf("origin %q is not allowed by recipe", origin)
}

func validateInputs(value Recipe, inputs map[string]string) error {
	if len(inputs) > 64 {
		return errors.New("at most 64 recipe inputs are allowed")
	}
	totalBytes := 0
	for name, spec := range value.Inputs {
		input, ok := inputs[name]
		if spec.Required && (!ok || strings.TrimSpace(input) == "") {
			return fmt.Errorf("required input %q is missing", name)
		}
	}
	for name := range inputs {
		if _, ok := value.Inputs[name]; !ok {
			return fmt.Errorf("undeclared input %q", name)
		}
		totalBytes += len(name) + len(inputs[name])
		if len(inputs[name]) > 1<<20 {
			return fmt.Errorf("input %q exceeds 1 MiB", name)
		}
	}
	if totalBytes > 1<<20 {
		return errors.New("recipe inputs exceed 1 MiB in total")
	}
	return nil
}

// Service binds private discovery/fetch to deterministic execution.
type Service struct {
	provider Provider
	runner   Runner
	locksMu  sync.Mutex
	locks    map[string]*serviceRunLock
}

type serviceRunLock struct {
	token chan struct{}
	users int
}

func NewService(provider Provider, runner Runner) (*Service, error) {
	if provider == nil || runner.Surface == nil {
		return nil, errors.New("recipe provider and runner surface are required")
	}
	return &Service{provider: provider, runner: runner, locks: map[string]*serviceRunLock{}}, nil
}

func (s *Service) SearchRecipes(ctx context.Context, query, origin string, limit int) ([]Match, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 1000 {
		return nil, errors.New("invalid recipe query")
	}
	if limit == 0 {
		limit = 10
	}
	if limit < 1 || limit > 50 {
		return nil, errors.New("recipe search limit must be one to 50")
	}
	if origin != "" {
		if err := validateOrigin(origin); err != nil {
			return nil, fmt.Errorf("invalid origin filter: %w", err)
		}
	}
	matches, err := s.provider.Search(ctx, query, origin, limit)
	if err != nil {
		return nil, err
	}
	if len(matches) > limit {
		return nil, errors.New("recipe provider returned more matches than requested")
	}
	// Never return provider-owned backing storage across this trust boundary.
	// A remote adapter or plugin may reuse its buffers after Search returns.
	safeMatches := make([]Match, len(matches))
	seen := make(map[string]bool, len(matches))
	for index := range matches {
		if err := validateMatch(matches[index], origin); err != nil {
			return nil, fmt.Errorf("recipe provider match %d: %w", index+1, err)
		}
		key := matches[index].ID + "@" + matches[index].Version
		if seen[key] {
			return nil, errors.New("recipe provider returned a duplicate match")
		}
		seen[key] = true
		safeMatches[index] = matches[index]
		safeMatches[index].Origins = append([]string(nil), matches[index].Origins...)
	}
	return safeMatches, nil
}

func (s *Service) RunRecipe(ctx context.Context, request RunRequest) (RunResult, error) {
	decodedDigest, digestErr := hex.DecodeString(request.Digest)
	if !recipeIDPattern.MatchString(request.ID) || !versionPattern.MatchString(request.Version) || digestErr != nil || len(decodedDigest) != sha256.Size || hex.EncodeToString(decodedDigest) != request.Digest {
		return RunResult{}, errors.New("invalid recipe identity")
	}
	if err := validateInputEnvelope(request.Inputs); err != nil {
		return RunResult{}, err
	}
	value, err := s.provider.Fetch(ctx, request.ID, request.Version, request.Digest)
	if err != nil {
		return RunResult{}, err
	}
	if err := Validate(value); err != nil {
		return RunResult{}, fmt.Errorf("provider returned invalid recipe: %w", err)
	}
	actualDigest, err := Digest(value)
	if err != nil {
		return RunResult{}, err
	}
	if value.ID != request.ID || value.Version != request.Version || actualDigest != request.Digest {
		return RunResult{}, errors.New("provider returned a recipe that does not match the pinned identity")
	}
	release, err := s.acquireRunLocks(ctx, value, request.Inputs)
	if err != nil {
		return RunResult{}, err
	}
	defer release()
	return s.runner.Run(ctx, value, request.Inputs)
}

// acquireRunLocks makes a recipe one transaction with respect to other recipes
// in this daemon. Runs on different explicit tabs remain parallel, while an
// expanded external-write idempotency key also singleflights across tabs.
func (s *Service) acquireRunLocks(ctx context.Context, value Recipe, inputs map[string]string) (func(), error) {
	tabID := browser.TabIDFromContext(ctx)
	if tabID == "" {
		tabID = "implicit"
	}
	keys := []string{"tab:" + tabID}
	for _, step := range value.Steps {
		if step.Effect != "external_write" {
			continue
		}
		key, err := Expand(step.IdempotencyKey, inputs)
		if err != nil || strings.TrimSpace(key) == "" {
			if err == nil {
				err = errors.New("idempotency key resolved empty")
			}
			return nil, err
		}
		keys = append(keys, "write:"+key)
	}
	sort.Strings(keys)
	keys = slicesCompact(keys)
	releases := make([]func(), 0, len(keys))
	for _, key := range keys {
		release, err := s.acquireRunLock(ctx, key)
		if err != nil {
			for index := len(releases) - 1; index >= 0; index-- {
				releases[index]()
			}
			return nil, err
		}
		releases = append(releases, release)
	}
	return func() {
		for index := len(releases) - 1; index >= 0; index-- {
			releases[index]()
		}
	}, nil
}

func slicesCompact(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func (s *Service) acquireRunLock(ctx context.Context, key string) (func(), error) {
	s.locksMu.Lock()
	lock := s.locks[key]
	if lock == nil {
		lock = &serviceRunLock{token: make(chan struct{}, 1)}
		s.locks[key] = lock
	}
	lock.users++
	s.locksMu.Unlock()

	select {
	case lock.token <- struct{}{}:
		return func() {
			<-lock.token
			s.locksMu.Lock()
			lock.users--
			if lock.users == 0 {
				delete(s.locks, key)
			}
			s.locksMu.Unlock()
		}, nil
	case <-ctx.Done():
		s.locksMu.Lock()
		lock.users--
		if lock.users == 0 {
			delete(s.locks, key)
		}
		s.locksMu.Unlock()
		return nil, ctx.Err()
	}
}

func validateInputEnvelope(inputs map[string]string) error {
	if len(inputs) > 64 {
		return errors.New("at most 64 recipe inputs are allowed")
	}
	totalBytes := 0
	for name, value := range inputs {
		if len(value) > 1<<20 {
			return fmt.Errorf("input %q exceeds 1 MiB", name)
		}
		totalBytes += len(name) + len(value)
		if totalBytes > 1<<20 {
			return errors.New("recipe inputs exceed 1 MiB in total")
		}
	}
	return nil
}
