package recipe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
)

type fakeSurface struct {
	mu         sync.Mutex
	origin     string
	elements   []ResolvedElement
	events     map[string]bool
	notify     chan struct{}
	clicks     int
	fills      int
	presses    int
	armCalls   int
	lastFill   string
	traceSafe  bool
	eventSafe  bool
	onClick    func(*fakeSurface) error
	onPress    func(*fakeSurface) error
	fillErr    error
	artifact   artifact.Meta
	captureErr error
	captures   int
}

type surfaceWithoutEventArmer struct{ Surface }

type stubProvider struct {
	matches []Match
	recipe  Recipe
}

type retainingProvider struct {
	matches []Match
}

func (p *retainingProvider) Search(context.Context, string, string, int) ([]Match, error) {
	return p.matches, nil
}

func (p *retainingProvider) Fetch(context.Context, string, string, string) (Recipe, error) {
	return Recipe{}, errors.New("unused")
}

func (p stubProvider) Search(context.Context, string, string, int) ([]Match, error) {
	return append([]Match(nil), p.matches...), nil
}

func (p stubProvider) Fetch(context.Context, string, string, string) (Recipe, error) {
	return cloneRecipe(p.recipe), nil
}

func newFakeSurface() *fakeSurface {
	return &fakeSurface{
		origin: "https://billing.example.test", events: map[string]bool{},
		notify:   make(chan struct{}, 1),
		elements: []ResolvedElement{{Ref: "e1", Role: "button", Name: "Download invoices"}, {Ref: "e2", Role: "textbox", Name: "Month"}},
		artifact: artifact.Meta{ID: "art_00000000000000000000000000000000", Kind: "text", SizeBytes: 500_000},
	}
}

func waitForServiceLockUsers(t *testing.T, service *Service, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		service.locksMu.Lock()
		users := 0
		if lock := service.locks[key]; lock != nil {
			users = lock.users
		}
		service.locksMu.Unlock()
		if users == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("service lock %q never reached %d users", key, want)
}

func (f *fakeSurface) Origin(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.origin, nil
}
func (f *fakeSurface) Resolve(_ context.Context, target Target) ([]ResolvedElement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ResolvedElement
	for _, element := range f.elements {
		if element.Role == target.Role && (target.Name == "" || element.Name == target.Name) && (target.NameContains == "" || strings.Contains(element.Name, target.NameContains)) {
			out = append(out, element)
		}
	}
	return out, nil
}
func (f *fakeSurface) Click(context.Context, string) error {
	f.mu.Lock()
	f.clicks++
	callback := f.onClick
	f.mu.Unlock()
	if callback != nil {
		return callback(f)
	}
	return nil
}
func (f *fakeSurface) Fill(ctx context.Context, _ string, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fills++
	f.lastFill = value
	f.traceSafe = browser.RedactTraceEntry(ctx, browser.TraceEntry{Text: value}).Redacted
	return f.fillErr
}
func (f *fakeSurface) Type(context.Context, string, string) error   { return nil }
func (f *fakeSurface) Select(context.Context, string, string) error { return nil }
func (f *fakeSurface) Press(context.Context, string, string) error {
	f.mu.Lock()
	f.presses++
	callback := f.onPress
	f.mu.Unlock()
	if callback != nil {
		return callback(f)
	}
	return nil
}
func (f *fakeSurface) NavigateTo(context.Context, string) error { return nil }
func (f *fakeSurface) Capture(context.Context, CaptureSpec) (artifact.Meta, error) {
	f.mu.Lock()
	f.captures++
	f.mu.Unlock()
	return f.artifact, f.captureErr
}
func (f *fakeSurface) WaitEvent(ctx context.Context, event Event) error {
	f.mu.Lock()
	f.eventSafe = browser.RedactTraceEntry(ctx, browser.TraceEntry{Text: event.Match}).Redacted
	f.mu.Unlock()
	key := event.Kind + "\x00" + event.Match
	timer := time.NewTimer(time.Duration(event.TimeoutMS) * time.Millisecond)
	defer timer.Stop()
	for {
		f.mu.Lock()
		ready := f.events[key]
		f.mu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-f.notify:
		case <-timer.C:
			return errors.New("event timed out")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
func (f *fakeSurface) ArmEvent(_ context.Context, event Event) (func(context.Context) error, error) {
	f.mu.Lock()
	f.armCalls++
	f.mu.Unlock()
	return func(ctx context.Context) error { return f.WaitEvent(ctx, event) }, nil
}
func (f *fakeSurface) EventSatisfied(_ context.Context, event Event) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.events[event.Kind+"\x00"+event.Match], nil
}
func (f *fakeSurface) emit(kind, match string) {
	f.mu.Lock()
	f.events[kind+"\x00"+match] = true
	f.mu.Unlock()
	select {
	case f.notify <- struct{}{}:
	default:
	}
}

func runRecipe() Recipe {
	value := validRecipe("https://billing.example.test")
	value.Steps = []Step{
		{ID: "fill_month", Action: "fill", Effect: "read", Target: &Target{Role: "textbox", Name: "Month"}, Value: "${input:month}"},
		{ID: "click_download", Action: "click", Effect: "read", Target: &Target{Role: "button", Name: "Download invoices"}},
		{ID: "await_ready", Action: "wait_event", Event: &Event{Kind: "text.present", Match: "ready", TimeoutMS: 500}},
		{ID: "short_timer", Action: "timer", TimerMS: 5},
		{ID: "save", Action: "capture", Capture: &CaptureSpec{Kind: "text"}},
	}
	return value
}

func TestRunnerEventsTimersInputsAndPayloadFreeArtifacts(t *testing.T) {
	surface := newFakeSurface()
	surface.onClick = func(f *fakeSurface) error {
		go func() { time.Sleep(20 * time.Millisecond); f.emit("text.present", "ready") }()
		return nil
	}
	result, err := (Runner{Surface: surface}).Run(context.Background(), runRecipe(), map[string]string{"month": "2026-09-private"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "done" || len(result.Steps) != 5 || len(result.Artifacts) != 1 || surface.clicks != 1 || surface.fills != 1 {
		t.Fatalf("result=%+v surface=%+v", result, surface)
	}
	encoded, _ := json.Marshal(result)
	if bytes.Contains(encoded, []byte("2026-09-private")) || surface.lastFill != "2026-09-private" {
		t.Fatalf("secret/input leaked or not filled: %s fill=%q", encoded, surface.lastFill)
	}
}

func TestRunnerFailsClosedOnOriginDriftMissingAndAmbiguousTargets(t *testing.T) {
	for name, test := range map[string]struct {
		mutate func(*fakeSurface)
		want   string
	}{
		"origin":  {func(f *fakeSurface) { f.origin = "https://evil.example.test" }, "not allowed"},
		"missing": {func(f *fakeSurface) { f.elements = f.elements[1:] }, "resolved to 0"},
		"ambiguous": {func(f *fakeSurface) {
			f.elements = append(f.elements, ResolvedElement{Ref: "e9", Role: "button", Name: "Download invoices"})
		}, "resolved to 2"},
	} {
		t.Run(name, func(t *testing.T) {
			surface := newFakeSurface()
			test.mutate(surface)
			value := runRecipe()
			value.Steps = value.Steps[1:2]
			_, err := (Runner{Surface: surface}).Run(context.Background(), value, map[string]string{"month": "x"})
			if err == nil || !strings.Contains(err.Error(), test.want) || surface.clicks != 0 {
				t.Fatalf("err=%v clicks=%d", err, surface.clicks)
			}
		})
	}
}

func TestRunnerExpandsSemanticTargetInputsBeforeResolution(t *testing.T) {
	surface := newFakeSurface()
	surface.elements = []ResolvedElement{{Ref: "person", Role: "link", Name: "Ada Lovelace"}}
	value := validRecipe("https://billing.example.test")
	value.Inputs = map[string]Input{"person": {Required: true}}
	value.Steps = []Step{{
		ID: "open_person", Action: "click", Effect: "read",
		Target: &Target{Role: "link", Name: "${input:person}"},
	}}
	result, err := (Runner{Surface: surface}).Run(context.Background(), value, map[string]string{"person": "Ada Lovelace"})
	if err != nil || result.Status != "done" || surface.clicks != 1 {
		t.Fatalf("result=%+v clicks=%d err=%v", result, surface.clicks, err)
	}
}

func TestRuntimeEventExpansionIsSinglePass(t *testing.T) {
	literalTemplate := "literal ${input:not_recipe_syntax}"
	event, err := expandEvent(Event{
		Kind: "element.value", Match: "${input:value}", TimeoutMS: 100,
		Target: &Target{Role: "textbox", Name: "${input:target}"},
	}, map[string]string{"value": literalTemplate, "target": literalTemplate})
	if err != nil {
		t.Fatalf("expanded user data was parsed as recipe syntax: %v", err)
	}
	if event.Match != literalTemplate || event.Target == nil || event.Target.Name != literalTemplate {
		t.Fatalf("expanded event = %+v, want literal user data preserved", event)
	}
}

func TestRunnerPreflightsEveryRuntimeExpansionBeforeStepOne(t *testing.T) {
	for name, inputs := range map[string]map[string]string{
		"missing later optional input": {},
		"empty selector":               {"person": ""},
		"oversized selector":           {"person": strings.Repeat("x", 1001)},
	} {
		t.Run(name, func(t *testing.T) {
			surface := newFakeSurface()
			value := validRecipe("https://billing.example.test")
			value.Inputs = map[string]Input{"person": {Required: false}}
			value.Steps = []Step{
				{ID: "first", Action: "click", Effect: "read", Target: &Target{Role: "button", Name: "Download invoices"}},
				{ID: "later", Action: "click", Effect: "read", Target: &Target{Role: "link", Name: "${input:person}"}},
			}
			_, err := (Runner{Surface: surface}).Run(context.Background(), value, inputs)
			if err == nil || surface.clicks != 0 {
				t.Fatalf("err=%v clicks=%d; no action may precede a runtime-plan validation failure", err, surface.clicks)
			}
		})
	}
}

func TestTargetTemplatesPropagateSensitivityAndExpandAcrossEvents(t *testing.T) {
	secret := map[string]Input{"needle": {Secret: true, Required: true}}
	step := Step{
		Target:        &Target{Role: "link", NameContains: "${input:needle}"},
		Postcondition: &Event{Kind: "element.value", Match: "", TimeoutMS: 100, Target: &Target{Role: "textbox", TestID: "composer-${input:needle}"}},
		Capture:       &CaptureSpec{Kind: "screenshot", Target: &Target{Role: "region", HrefContains: "${input:needle}"}},
	}
	if !stepUsesSecret(step, secret) {
		t.Fatal("secret used only in a semantic target was not marked sensitive")
	}
	event, err := expandEvent(*step.Postcondition, map[string]string{"needle": "private"})
	if err != nil || event.Target.TestID != "composer-private" {
		t.Fatalf("expanded event=%+v err=%v", event, err)
	}
}

func TestExternalWriteWithEmptyComposerPostconditionIsReplaySafe(t *testing.T) {
	surface := newFakeSurface()
	surface.onPress = func(f *fakeSurface) error {
		f.emit("element.value", "")
		return nil
	}
	value := validRecipe("https://billing.example.test")
	value.Risk = "external_write"
	value.Inputs = nil
	value.Steps = []Step{{
		ID: "send", Action: "press", Key: "Enter",
		Target:         &Target{Role: "textbox", Name: "Month"},
		Effect:         "external_write",
		IdempotencyKey: "send-current-draft",
		Postcondition: &Event{
			Kind: "element.value", Match: "", TimeoutMS: 500,
			Target: &Target{Role: "textbox", Name: "Month"},
		},
	}}
	runner := Runner{Surface: surface}
	first, err := runner.Run(context.Background(), value, nil)
	if err != nil || first.Status != "done" || surface.presses != 1 || first.Steps[0].Attempts != 1 {
		t.Fatalf("first=%+v presses=%d err=%v", first, surface.presses, err)
	}
	second, err := runner.Run(context.Background(), value, nil)
	if err != nil || second.Status != "done" || surface.presses != 1 || second.Steps[0].Attempts != 0 {
		t.Fatalf("second=%+v presses=%d err=%v", second, surface.presses, err)
	}
}

func TestRunnerRejectsOriginDriftCausedByFinalAction(t *testing.T) {
	surface := newFakeSurface()
	surface.onClick = func(f *fakeSurface) error {
		f.mu.Lock()
		f.origin = "https://evil.example.test"
		f.mu.Unlock()
		return nil
	}
	value := runRecipe()
	value.Steps = value.Steps[1:2]
	result, err := (Runner{Surface: surface}).Run(context.Background(), value, map[string]string{"month": "x"})
	if err == nil || !strings.Contains(err.Error(), "not allowed") || result.Status != "failed" || surface.clicks != 1 {
		t.Fatalf("result=%+v clicks=%d err=%v", result, surface.clicks, err)
	}
}

type driftOnResolveSurface struct{ *fakeSurface }

func (s *driftOnResolveSurface) Resolve(ctx context.Context, target Target) ([]ResolvedElement, error) {
	matches, err := s.fakeSurface.Resolve(ctx, target)
	s.mu.Lock()
	s.origin = "https://evil.example.test"
	s.mu.Unlock()
	return matches, err
}

func TestRunnerRejectsOriginDriftBetweenCaptureResolutionAndPersistence(t *testing.T) {
	base := newFakeSurface()
	surface := &driftOnResolveSurface{fakeSurface: base}
	value := runRecipe()
	value.Steps = []Step{{
		ID: "capture_button", Action: "capture",
		Capture: &CaptureSpec{Kind: "screenshot", Target: &Target{Role: "button", Name: "Download invoices"}},
	}}
	_, err := (Runner{Surface: surface}).Run(context.Background(), value, map[string]string{"month": "x"})
	if err == nil || !strings.Contains(err.Error(), "not allowed") || base.captures != 0 {
		t.Fatalf("err=%v captures=%d", err, base.captures)
	}
}

type driftOnArmSurface struct{ *fakeSurface }

func (s *driftOnArmSurface) ArmEvent(ctx context.Context, event Event) (func(context.Context) error, error) {
	wait, err := s.fakeSurface.ArmEvent(ctx, event)
	s.mu.Lock()
	s.origin = "https://evil.example.test"
	s.mu.Unlock()
	return wait, err
}

func TestRunnerRejectsOriginDriftWhileArmingPostcondition(t *testing.T) {
	base := newFakeSurface()
	surface := &driftOnArmSurface{fakeSurface: base}
	value := runRecipe()
	value.Steps = []Step{{
		ID: "download", Action: "click", Effect: "read", Target: &Target{Role: "button", Name: "Download invoices"},
		Postcondition: &Event{Kind: "download.completed", Match: "invoice.pdf", TimeoutMS: 100},
	}}
	_, err := (Runner{Surface: surface}).Run(context.Background(), value, map[string]string{"month": "x"})
	if err == nil || !strings.Contains(err.Error(), "not allowed") || base.clicks != 0 || base.armCalls != 1 {
		t.Fatalf("err=%v clicks=%d arms=%d", err, base.clicks, base.armCalls)
	}
}

type cancelOnArmSurface struct {
	*fakeSurface
	cancel context.CancelFunc
}

func (s *cancelOnArmSurface) ArmEvent(ctx context.Context, event Event) (func(context.Context) error, error) {
	wait, err := s.fakeSurface.ArmEvent(ctx, event)
	s.cancel()
	return wait, err
}

func TestRunnerDoesNotActuateAfterCancellationDuringEventArm(t *testing.T) {
	base := newFakeSurface()
	ctx, cancel := context.WithCancel(context.Background())
	surface := &cancelOnArmSurface{fakeSurface: base, cancel: cancel}
	value := runRecipe()
	value.Steps = []Step{{
		ID: "download", Action: "click", Effect: "read", Target: &Target{Role: "button", Name: "Download invoices"},
		Postcondition: &Event{Kind: "download.completed", Match: "invoice.pdf", TimeoutMS: 100},
	}}
	_, err := (Runner{Surface: surface}).Run(ctx, value, map[string]string{"month": "x"})
	if !errors.Is(err, context.Canceled) || base.clicks != 0 || base.armCalls != 1 {
		t.Fatalf("err=%v clicks=%d arms=%d", err, base.clicks, base.armCalls)
	}
}

func TestRunnerMarksDeclaredSecretActionForTraceRedaction(t *testing.T) {
	surface := newFakeSurface()
	value := runRecipe()
	value.Steps = value.Steps[:1]
	input := value.Inputs["month"]
	input.Secret = true
	value.Inputs["month"] = input
	if _, err := (Runner{Surface: surface}).Run(context.Background(), value, map[string]string{"month": "synthetic-sensitive-input"}); err != nil {
		t.Fatal(err)
	}
	if !surface.traceSafe {
		t.Fatal("declared secret action was not marked for browser trace redaction")
	}
}

func TestRunnerMarksDeclaredSecretEventForTraceRedaction(t *testing.T) {
	surface := newFakeSurface()
	value := runRecipe()
	input := value.Inputs["month"]
	input.Secret = true
	value.Inputs["month"] = input
	value.Steps = []Step{{
		ID: "wait_for_private_value", Action: "wait_event",
		Event: &Event{Kind: "text.present", Match: "ready ${input:month}", TimeoutMS: 100},
	}}
	surface.emit("text.present", "ready synthetic-sensitive-event")
	if _, err := (Runner{Surface: surface}).Run(context.Background(), value, map[string]string{"month": "synthetic-sensitive-event"}); err != nil {
		t.Fatal(err)
	}
	if !surface.eventSafe {
		t.Fatal("declared secret event was not marked for browser trace redaction")
	}
}

func TestRunnerReconcilesLostAcknowledgementWithoutDuplicateWrite(t *testing.T) {
	surface := newFakeSurface()
	value := runRecipe()
	value.Risk = "external_write"
	value.Steps = []Step{{
		ID: "send", Action: "click", Target: &Target{Role: "button", Name: "Download invoices"},
		Effect: "external_write", MaxAttempts: 1, IdempotencyKey: "invoice:${input:month}",
		Postcondition: &Event{Kind: "text.present", Match: "sent", TimeoutMS: 100},
	}}
	surface.onClick = func(f *fakeSurface) error {
		if f.armCalls == 0 {
			return errors.New("postcondition was not armed before click")
		}
		f.emit("text.present", "sent")
		return errors.New("acknowledgement lost")
	}
	result, err := (Runner{Surface: surface}).Run(context.Background(), value, map[string]string{"month": "2026-09"})
	if err != nil || surface.clicks != 1 || surface.armCalls != 1 || result.Steps[0].Attempts != 1 {
		t.Fatalf("result=%+v clicks=%d err=%v", result, surface.clicks, err)
	}
}

func TestRunnerRefusesTransientPostconditionWithoutPrearmSupport(t *testing.T) {
	base := newFakeSurface()
	value := runRecipe()
	value.Steps = []Step{{
		ID: "download", Action: "click", Effect: "read", Target: &Target{Role: "button", Name: "Download invoices"},
		Postcondition: &Event{Kind: "download.completed", Match: "invoice.pdf", TimeoutMS: 100},
	}}
	_, err := (Runner{Surface: surfaceWithoutEventArmer{Surface: base}}).Run(context.Background(), value, map[string]string{"month": "x"})
	if err == nil || !strings.Contains(err.Error(), "pre-arm") || base.clicks != 0 {
		t.Fatalf("err=%v clicks=%d", err, base.clicks)
	}
}

func TestRunnerSkipsExternalWriteWhenDesiredStateAlreadyExists(t *testing.T) {
	surface := newFakeSurface()
	surface.emit("text.present", "sent 2026-09")
	value := runRecipe()
	value.Risk = "external_write"
	value.Steps = []Step{{
		ID: "send", Action: "click", Target: &Target{Role: "button", Name: "Download invoices"},
		Effect: "external_write", IdempotencyKey: "invoice:${input:month}",
		Postcondition: &Event{Kind: "text.present", Match: "sent ${input:month}", TimeoutMS: 100},
	}}
	result, err := (Runner{Surface: surface}).Run(context.Background(), value, map[string]string{"month": "2026-09"})
	if err != nil || surface.clicks != 0 || surface.armCalls != 0 || result.Steps[0].Attempts != 0 {
		t.Fatalf("result=%+v clicks=%d arms=%d err=%v", result, surface.clicks, surface.armCalls, err)
	}
}

func TestRunnerAppliesPostconditionsToPressAndExpandsEventInputs(t *testing.T) {
	surface := newFakeSurface()
	value := runRecipe()
	value.Risk = "external_write"
	value.Steps = []Step{{
		ID: "submit", Action: "press", Key: "Enter", Target: &Target{Role: "textbox", Name: "Month"}, Effect: "external_write",
		IdempotencyKey: "submit:${input:month}",
		Postcondition:  &Event{Kind: "text.present", Match: "saved ${input:month}", TimeoutMS: 100},
	}}
	surface.onPress = func(f *fakeSurface) error {
		f.emit("text.present", "saved September")
		return errors.New("lost press acknowledgement")
	}
	result, err := (Runner{Surface: surface}).Run(context.Background(), value, map[string]string{"month": "September"})
	if err != nil || result.Status != "done" || surface.presses != 1 || surface.armCalls != 1 {
		t.Fatalf("result=%+v presses=%d arms=%d err=%v", result, surface.presses, surface.armCalls, err)
	}
}

func TestRunnerRedactsInputsFromActionErrors(t *testing.T) {
	surface := newFakeSurface()
	secret := "customer-secret-value"
	surface.fillErr = errors.New("page rejected " + secret)
	value := runRecipe()
	value.Steps = value.Steps[:1]
	_, err := (Runner{Surface: surface}).Run(context.Background(), value, map[string]string{"month": secret})
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted input:month]") {
		t.Fatalf("input was not redacted from error: %v", err)
	}
}

func TestRunnerTimerCancellationIsPrompt(t *testing.T) {
	surface := newFakeSurface()
	value := runRecipe()
	value.Steps = []Step{{ID: "timer", Action: "timer", TimerMS: 2_000}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := (Runner{Surface: surface}).Run(ctx, value, map[string]string{"month": "x"})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("err=%v elapsed=%s", err, time.Since(started))
	}
}

func TestServiceFetchesPinnedRecipeBeforeRun(t *testing.T) {
	surface := newFakeSurface()
	surface.onClick = func(f *fakeSurface) error { go func() { f.emit("text.present", "ready") }(); return nil }
	value := runRecipe()
	catalog, err := NewCatalog(context.Background(), []Recipe{value}, nil)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(catalog, Runner{Surface: surface})
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := Digest(value)
	if _, err := service.RunRecipe(context.Background(), RunRequest{ID: value.ID, Version: value.Version, Digest: digest, Inputs: map[string]string{"month": "x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RunRecipe(context.Background(), RunRequest{ID: value.ID, Version: value.Version, Digest: strings.Repeat("0", 64), Inputs: map[string]string{"month": "x"}}); err == nil {
		t.Fatal("tampered digest ran")
	}
}

func TestServiceSerializesRecipeTransactionsOnTheSameTab(t *testing.T) {
	surface := newFakeSurface()
	entered := make(chan struct{}, 2)
	releaseFirst := make(chan struct{})
	surface.onClick = func(*fakeSurface) error {
		entered <- struct{}{}
		<-releaseFirst
		return nil
	}
	value := validRecipe("https://billing.example.test")
	value.Inputs = nil
	value.Steps = []Step{{ID: "act", Action: "click", Effect: "read", Target: &Target{Role: "button", Name: "Download invoices"}}}
	catalog, err := NewCatalog(context.Background(), []Recipe{value}, nil)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(catalog, Runner{Surface: surface})
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := Digest(value)
	request := RunRequest{ID: value.ID, Version: value.Version, Digest: digest}
	ctx := browser.WithTabID(context.Background(), "same-tab")
	errs := make(chan error, 2)
	go func() { _, err := service.RunRecipe(ctx, request); errs <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first recipe never entered its action")
	}
	go func() { _, err := service.RunRecipe(ctx, request); errs <- err }()
	waitForServiceLockUsers(t, service, "tab:same-tab", 2)
	select {
	case <-entered:
		t.Fatal("second same-tab recipe interleaved before the first completed")
	default:
	}
	close(releaseFirst)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if surface.clicks != 2 {
		t.Fatalf("clicks=%d, want two serialized actions", surface.clicks)
	}
	service.locksMu.Lock()
	remainingLocks := len(service.locks)
	service.locksMu.Unlock()
	if remainingLocks != 0 {
		t.Fatalf("service retained %d locks after same-tab runs", remainingLocks)
	}
}

func TestServiceSerializesSameIdempotencyKeyAcrossTabs(t *testing.T) {
	surface := newFakeSurface()
	entered := make(chan struct{}, 2)
	releaseFirst := make(chan struct{})
	surface.onClick = func(f *fakeSurface) error {
		entered <- struct{}{}
		<-releaseFirst
		f.emit("text.present", "Message sent")
		return nil
	}
	value := validRecipe("https://billing.example.test")
	value.Inputs = nil
	value.Risk = "external_write"
	value.Steps = []Step{{
		ID:             "send",
		Action:         "click",
		Effect:         "external_write",
		Target:         &Target{Role: "button", Name: "Download invoices"},
		IdempotencyKey: "send:conversation-42:message-7",
		Postcondition:  &Event{Kind: "text.present", Match: "Message sent", TimeoutMS: 500},
	}}
	catalog, err := NewCatalog(context.Background(), []Recipe{value}, nil)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(catalog, Runner{Surface: surface})
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := Digest(value)
	request := RunRequest{ID: value.ID, Version: value.Version, Digest: digest}
	type outcome struct {
		result RunResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	go func() {
		result, err := service.RunRecipe(browser.WithTabID(context.Background(), "tab-a"), request)
		outcomes <- outcome{result: result, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first external write never entered its action")
	}
	go func() {
		result, err := service.RunRecipe(browser.WithTabID(context.Background(), "tab-b"), request)
		outcomes <- outcome{result: result, err: err}
	}()
	waitForServiceLockUsers(t, service, "write:send:conversation-42:message-7", 2)
	select {
	case <-entered:
		t.Fatal("same idempotency key interleaved across tabs")
	default:
	}
	close(releaseFirst)

	attempts := make([]int, 0, 2)
	for range 2 {
		outcome := <-outcomes
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		if len(outcome.result.Steps) != 1 {
			t.Fatalf("unexpected result: %+v", outcome.result)
		}
		attempts = append(attempts, outcome.result.Steps[0].Attempts)
	}
	sort.Ints(attempts)
	if surface.clicks != 1 || !slices.Equal(attempts, []int{0, 1}) {
		t.Fatalf("clicks=%d attempts=%v, want one write and one reconciled no-op", surface.clicks, attempts)
	}
	service.locksMu.Lock()
	remainingLocks := len(service.locks)
	service.locksMu.Unlock()
	if remainingLocks != 0 {
		t.Fatalf("service retained %d locks after cross-tab runs", remainingLocks)
	}
}

func TestServiceIndependentlyRejectsProviderPinSubstitution(t *testing.T) {
	surface := newFakeSurface()
	wanted := runRecipe()
	digest, _ := Digest(wanted)
	substitute := cloneRecipe(wanted)
	substitute.Description = "A different executable document"
	service, err := NewService(stubProvider{recipe: substitute}, Runner{Surface: surface})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.RunRecipe(context.Background(), RunRequest{
		ID: wanted.ID, Version: wanted.Version, Digest: digest,
		Inputs: map[string]string{"month": "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "pinned identity") || surface.clicks != 0 || surface.fills != 0 {
		t.Fatalf("substitution err=%v clicks=%d fills=%d", err, surface.clicks, surface.fills)
	}
}

func TestServiceIndependentlyRejectsUnsafeSearchMetadata(t *testing.T) {
	value := runRecipe()
	digest, _ := Digest(value)
	provider := stubProvider{matches: []Match{{
		ID: value.ID, Version: value.Version, Name: value.Name,
		Description: "token=should-not-cross-the-boundary", Origins: value.Origins,
		Risk: value.Risk, Digest: digest, Score: 1,
	}}}
	service, err := NewService(provider, Runner{Surface: newFakeSurface()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SearchRecipes(context.Background(), "download invoice", value.Origins[0], 5); err == nil || !strings.Contains(err.Error(), "disclosure-safe") {
		t.Fatalf("unsafe provider metadata error = %v", err)
	}
}

func TestServiceDoesNotExposeProviderOwnedSearchStorage(t *testing.T) {
	value := runRecipe()
	digest, _ := Digest(value)
	provider := &retainingProvider{matches: []Match{{
		ID: value.ID, Version: value.Version, Name: value.Name,
		Description: value.Description, Origins: append([]string(nil), value.Origins...),
		Risk: value.Risk, Digest: digest, Score: 1,
	}}}
	service, err := NewService(provider, Runner{Surface: newFakeSurface()})
	if err != nil {
		t.Fatal(err)
	}
	matches, err := service.SearchRecipes(context.Background(), "download invoice", value.Origins[0], 5)
	if err != nil {
		t.Fatal(err)
	}
	provider.matches[0].Name = "mutated by provider"
	provider.matches[0].Origins[0] = "https://mutated.example.test"
	if matches[0].Name != value.Name || matches[0].Origins[0] != value.Origins[0] {
		t.Fatalf("provider mutation escaped boundary: %+v", matches[0])
	}
}
