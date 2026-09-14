package recipe

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/artifact"
)

const receiptOrigin = "https://billing.example.test"

func receiptRecipe() Recipe {
	value := validRecipe(receiptOrigin)
	value.Risk = "external_write"
	value.Inputs = map[string]Input{"month": {Required: true}}
	value.Steps = []Step{
		{
			ID: "send", Action: "click", Effect: "external_write",
			Target:         &Target{Role: "button", Name: "Download invoices"},
			IdempotencyKey: "send:${input:month}",
			Postcondition:  &Event{Kind: "text.present", Match: "sent", TimeoutMS: 100},
		},
		verifyWriteStep(),
	}
	return value
}

// recordingReceipts is a durable store that also remembers the order it was
// called in, so "recorded before dispatch" can be checked rather than assumed.
type recordingReceipts struct {
	inner *MemoryReceipts
	calls *[]string
}

func newRecordingReceipts(calls *[]string) *recordingReceipts {
	return &recordingReceipts{inner: NewMemoryReceipts(), calls: calls}
}

func (r *recordingReceipts) Lookup(ctx context.Context, key string) (Receipt, bool, error) {
	*r.calls = append(*r.calls, "lookup")
	return r.inner.Lookup(ctx, key)
}

func (r *recordingReceipts) Begin(ctx context.Context, receipt Receipt) (Receipt, error) {
	*r.calls = append(*r.calls, "begin")
	return r.inner.Begin(ctx, receipt)
}

func (r *recordingReceipts) Commit(ctx context.Context, key, evidence string) (Receipt, error) {
	*r.calls = append(*r.calls, "commit")
	return r.inner.Commit(ctx, key, evidence)
}

func onlyReceipt(t *testing.T, store *MemoryReceipts) Receipt {
	t.Helper()
	if len(store.records) != 1 {
		t.Fatalf("store holds %d receipts, want exactly one", len(store.records))
	}
	for _, record := range store.records {
		return record
	}
	return Receipt{}
}

func TestWriteReceiptIsRecordedBeforeDispatchAndCommittedAfterThePostcondition(t *testing.T) {
	calls := []string{}
	store := newRecordingReceipts(&calls)
	surface := newFakeSurface()
	surface.onClick = func(f *fakeSurface) error {
		calls = append(calls, "click")
		f.emit("text.present", "sent")
		return nil
	}
	result, err := (Runner{Surface: surface, Receipts: store}).
		Run(context.Background(), receiptRecipe(), map[string]string{"month": "2026-09"})
	if err != nil || result.Status != "done" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	want := []string{"lookup", "begin", "click", "commit"}
	if !slices.Equal(calls, want) {
		t.Fatalf("call order = %v, want %v", calls, want)
	}
	record := onlyReceipt(t, store.inner)
	if record.Status != ReceiptCommitted || record.Evidence == "" {
		t.Fatalf("receipt = %+v, want committed with evidence", record)
	}
	if record.RecipeDigest == "" || record.StepID != "send" || record.Origin != receiptOrigin {
		t.Fatalf("receipt does not identify the write it covers: %+v", record)
	}
}

// A postcondition that never passes must never produce a committed receipt: the
// receipt is the thing a later run trusts, and a receipt that reports success
// for an unacknowledged write is worse than no receipt at all.
func TestReceiptIsNeverCommittedWhenThePostconditionDoesNotPass(t *testing.T) {
	calls := []string{}
	store := newRecordingReceipts(&calls)
	surface := newFakeSurface()
	surface.onClick = func(f *fakeSurface) error {
		calls = append(calls, "click")
		return nil
	}
	result, err := (Runner{Surface: surface, Receipts: store}).
		Run(context.Background(), receiptRecipe(), map[string]string{"month": "2026-09"})
	if err == nil || result.Status != "failed" {
		t.Fatalf("an unacknowledged write reported result=%+v err=%v", result, err)
	}
	if slices.Contains(calls, "commit") {
		t.Fatalf("calls = %v, want no commit", calls)
	}
	record := onlyReceipt(t, store.inner)
	if record.Status != ReceiptInFlight || record.Evidence != "" {
		t.Fatalf("receipt = %+v, want it left in flight", record)
	}
}

// The daemon dies between dispatch and acknowledgement. Everything in memory is
// gone; the provider-side receipt is not. The rerun must find it and must not
// click a second time.
func TestInterruptedWriteIsNotResubmittedAfterARestart(t *testing.T) {
	store := NewMemoryReceipts()
	first := newFakeSurface()
	first.onClick = func(*fakeSurface) error { return nil }
	if _, err := (Runner{Surface: first, Receipts: store}).
		Run(context.Background(), receiptRecipe(), map[string]string{"month": "2026-09"}); err == nil {
		t.Fatal("the interrupted first run reported success")
	}
	if first.clicks != 1 {
		t.Fatalf("first run clicked %d times", first.clicks)
	}
	if onlyReceipt(t, store).Status != ReceiptInFlight {
		t.Fatalf("receipt = %+v, want in flight", onlyReceipt(t, store))
	}

	tests := []struct {
		name         string
		verification error
		wantClicks   int
		wantErr      string
		wantStatus   string
	}{
		{
			name: "verification cannot confirm it", verification: errors.New("no such row"),
			wantClicks: 0, wantErr: "refuses to re-submit", wantStatus: ReceiptInFlight,
		},
		{
			name: "verification confirms it landed", verification: nil,
			wantClicks: 0, wantErr: "", wantStatus: ReceiptCommitted,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Every rerun starts from the receipt the interrupted run left, so
			// the two cases are independent of each other.
			store.records = map[string]Receipt{}
			seed := newFakeSurface()
			seed.onClick = func(*fakeSurface) error { return nil }
			_, _ = (Runner{Surface: seed, Receipts: store}).
				Run(context.Background(), receiptRecipe(), map[string]string{"month": "2026-09"})

			restarted := newFakeSurface()
			restarted.assertErr = test.verification
			restarted.onClick = func(*fakeSurface) error { return nil }
			_, err := (Runner{Surface: restarted, Receipts: store}).
				Run(context.Background(), receiptRecipe(), map[string]string{"month": "2026-09"})
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("rerun after a confirmed write failed: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, test.wantErr)
			}
			if restarted.clicks != test.wantClicks {
				t.Fatalf("rerun clicked %d times, want %d", restarted.clicks, test.wantClicks)
			}
			if got := onlyReceipt(t, store).Status; got != test.wantStatus {
				t.Fatalf("receipt status = %q, want %q", got, test.wantStatus)
			}
		})
	}
}

func TestCommittedReceiptSuppressesTheWriteEntirely(t *testing.T) {
	store := NewMemoryReceipts()
	first := newFakeSurface()
	first.onClick = func(f *fakeSurface) error {
		f.emit("text.present", "sent")
		return nil
	}
	if _, err := (Runner{Surface: first, Receipts: store}).
		Run(context.Background(), receiptRecipe(), map[string]string{"month": "2026-09"}); err != nil {
		t.Fatal(err)
	}
	second := newFakeSurface()
	result, err := (Runner{Surface: second, Receipts: store}).
		Run(context.Background(), receiptRecipe(), map[string]string{"month": "2026-09"})
	if err != nil || result.Status != "done" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if second.clicks != 0 || result.Steps[0].Attempts != 0 {
		t.Fatalf("a committed receipt did not suppress the write: clicks=%d steps=%+v", second.clicks, result.Steps)
	}
}

func nonceRecipe(nonceTarget Target) Recipe {
	value := receiptRecipe()
	value.Steps[0].SiteIdempotency = &SiteIdempotency{Kind: SiteIdempotencyFormNonce, Target: &nonceTarget}
	return value
}

// nonceBlindSurface has every capability an external write needs except the one
// that reads a declared nonce. A missing capability has to be named.
type nonceBlindSurface struct{ inner *fakeSurface }

func (s nonceBlindSurface) Origin(ctx context.Context) (string, error) { return s.inner.Origin(ctx) }
func (s nonceBlindSurface) Resolve(ctx context.Context, target Target) ([]ResolvedElement, error) {
	return s.inner.Resolve(ctx, target)
}
func (s nonceBlindSurface) Click(ctx context.Context, ref string) error {
	return s.inner.Click(ctx, ref)
}
func (s nonceBlindSurface) Fill(ctx context.Context, ref, value string) error {
	return s.inner.Fill(ctx, ref, value)
}
func (s nonceBlindSurface) Type(ctx context.Context, ref, value string) error {
	return s.inner.Type(ctx, ref, value)
}
func (s nonceBlindSurface) Select(ctx context.Context, ref, value string) error {
	return s.inner.Select(ctx, ref, value)
}
func (s nonceBlindSurface) Press(ctx context.Context, ref, key string) error {
	return s.inner.Press(ctx, ref, key)
}
func (s nonceBlindSurface) NavigateTo(ctx context.Context, url string) error {
	return s.inner.NavigateTo(ctx, url)
}
func (s nonceBlindSurface) WaitEvent(ctx context.Context, event Event) error {
	return s.inner.WaitEvent(ctx, event)
}
func (s nonceBlindSurface) Capture(ctx context.Context, spec CaptureSpec) (artifact.Meta, error) {
	return s.inner.Capture(ctx, spec)
}
func (s nonceBlindSurface) ArmEvent(ctx context.Context, event Event) (func(context.Context) error, error) {
	return s.inner.ArmEvent(ctx, event)
}
func (s nonceBlindSurface) EventSatisfied(ctx context.Context, event Event) (bool, error) {
	return s.inner.EventSatisfied(ctx, event)
}
func (s nonceBlindSurface) Assert(ctx context.Context, assertion Assertion) error {
	return s.inner.Assert(ctx, assertion)
}

func TestDeclaredSiteNonceDecidesWhetherAnInterruptedWriteMayBeRetried(t *testing.T) {
	nonceTarget := Target{Role: "textbox", Name: "Submission token"}
	value := nonceRecipe(nonceTarget)

	newNonceSurface := func(nonce string) *fakeSurface {
		surface := newFakeSurface()
		surface.elements = append(surface.elements, ResolvedElement{Ref: "e3", Role: "textbox", Name: "Submission token"})
		surface.nonce = nonce
		surface.onClick = func(*fakeSurface) error { return nil }
		return surface
	}

	tests := []struct {
		name       string
		rerunNonce string
		wantClicks int
		wantErr    string
	}{
		{
			// The page still carries the token the interrupted attempt used, and
			// the recipe declares the site rejects a second submission bearing
			// it, so brw defers to the site's mechanism.
			name: "site still holds the same token", rerunNonce: "fixture-nonce-value-one", wantClicks: 1,
		},
		{
			// A fresh token means the site would accept the submission as new,
			// so the site's mechanism protects nothing and brw stops.
			name: "site minted a fresh token", rerunNonce: "fixture-nonce-value-two", wantClicks: 0, wantErr: "refuses to re-submit",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := NewMemoryReceipts()
			interrupted := newNonceSurface("fixture-nonce-value-one")
			if _, err := (Runner{Surface: interrupted, Receipts: store}).
				Run(context.Background(), value, map[string]string{"month": "2026-09"}); err == nil {
				t.Fatal("the interrupted run reported success")
			}
			if record := onlyReceipt(t, store); record.SiteNonce != "fixture-nonce-value-one" {
				t.Fatalf("receipt did not record the site token: %+v", record)
			}

			restarted := newNonceSurface(test.rerunNonce)
			restarted.assertErr = errors.New("remote state does not show the write")
			_, err := (Runner{Surface: restarted, Receipts: store}).
				Run(context.Background(), value, map[string]string{"month": "2026-09"})
			if err == nil {
				t.Fatal("an unconfirmed write reported success")
			}
			if test.wantErr != "" && !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, test.wantErr)
			}
			if restarted.clicks != test.wantClicks {
				t.Fatalf("rerun clicked %d times, want %d", restarted.clicks, test.wantClicks)
			}
		})
	}
}

func TestDeclaredSiteNonceFailsClosed(t *testing.T) {
	nonceTarget := Target{Role: "textbox", Name: "Submission token"}
	value := nonceRecipe(nonceTarget)
	inputs := map[string]string{"month": "2026-09"}

	t.Run("surface cannot read a nonce", func(t *testing.T) {
		inner := newFakeSurface()
		inner.elements = append(inner.elements, ResolvedElement{Ref: "e3", Role: "textbox", Name: "Submission token"})
		_, err := (Runner{Surface: nonceBlindSurface{inner: inner}, Receipts: NewMemoryReceipts()}).
			Run(context.Background(), value, inputs)
		if err == nil || !strings.Contains(err.Error(), "cannot read") {
			t.Fatalf("err = %v, want the missing capability named", err)
		}
		if inner.clicks != 0 {
			t.Fatal("the write was dispatched without the declared token")
		}
	})

	t.Run("nonce field is empty", func(t *testing.T) {
		surface := newFakeSurface()
		surface.elements = append(surface.elements, ResolvedElement{Ref: "e3", Role: "textbox", Name: "Submission token"})
		surface.nonce = ""
		_, err := (Runner{Surface: surface, Receipts: NewMemoryReceipts()}).
			Run(context.Background(), value, inputs)
		if err == nil || !strings.Contains(err.Error(), "the field is empty") {
			t.Fatalf("err = %v, want a refusal naming the empty token", err)
		}
		if surface.clicks != 0 {
			t.Fatal("the write was dispatched without the declared token")
		}
	})

	t.Run("no receipt store to record it against", func(t *testing.T) {
		surface := newFakeSurface()
		surface.elements = append(surface.elements, ResolvedElement{Ref: "e3", Role: "textbox", Name: "Submission token"})
		_, err := (Runner{Surface: surface}).Run(context.Background(), value, inputs)
		if err == nil || !strings.Contains(err.Error(), "no write receipt store") {
			t.Fatalf("err = %v, want the missing store named", err)
		}
		if surface.clicks != 0 {
			t.Fatal("the write was dispatched with its declared mechanism silently disabled")
		}
	})
}

func TestReceiptKeyIsReproducibleAndSeparatesDistinctWrites(t *testing.T) {
	value := receiptRecipe()
	step := value.Steps[0]
	inputs := map[string]string{"month": "2026-09"}

	key, err := ReceiptKey(value, step, receiptOrigin, inputs)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ReceiptKey(value, step, receiptOrigin, map[string]string{"month": "2026-09"})
	if err != nil || again != key {
		t.Fatalf("the same write produced two keys: %q and %q (err=%v)", key, again, err)
	}
	if len(key) != 64 {
		t.Fatalf("key %q is not a sha256 digest", key)
	}

	changed := cloneRecipe(value)
	changed.Steps[0].Target.Name = "Download statements"
	changedKey, err := ReceiptKey(changed, changed.Steps[0], receiptOrigin, inputs)
	if err != nil {
		t.Fatal(err)
	}

	twoWrites := cloneRecipe(value)
	twoWrites.Steps[0].ID = "send_again"
	secondStepKey, err := ReceiptKey(twoWrites, twoWrites.Steps[0], receiptOrigin, inputs)
	if err != nil {
		t.Fatal(err)
	}

	twoInputs := cloneRecipe(value)
	twoInputs.Inputs = map[string]Input{"month": {Required: true}, "note": {}}
	leftSwap, err := ReceiptKey(twoInputs, twoInputs.Steps[0], receiptOrigin, map[string]string{"month": "a", "note": "b"})
	if err != nil {
		t.Fatal(err)
	}
	rightSwap, err := ReceiptKey(twoInputs, twoInputs.Steps[0], receiptOrigin, map[string]string{"month": "b", "note": "a"})
	if err != nil {
		t.Fatal(err)
	}
	absent, err := ReceiptKey(twoInputs, twoInputs.Steps[0], receiptOrigin, map[string]string{"month": "2026-09"})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := ReceiptKey(twoInputs, twoInputs.Steps[0], receiptOrigin, map[string]string{"month": "2026-09", "note": ""})
	if err != nil {
		t.Fatal(err)
	}
	crlf, err := ReceiptKey(twoInputs, twoInputs.Steps[0], receiptOrigin, map[string]string{"month": "2026-09", "note": "one\r\ntwo"})
	if err != nil {
		t.Fatal(err)
	}
	lf, err := ReceiptKey(twoInputs, twoInputs.Steps[0], receiptOrigin, map[string]string{"month": "2026-09", "note": "one\ntwo"})
	if err != nil {
		t.Fatal(err)
	}
	spelledOrigin, err := ReceiptKey(value, step, "https://Billing.Example.Test:443", inputs)
	if err != nil {
		t.Fatal(err)
	}
	otherInput, err := ReceiptKey(value, step, receiptOrigin, map[string]string{"month": "2026-10"})
	if err != nil {
		t.Fatal(err)
	}
	otherOrigin, err := ReceiptKey(value, step, "https://other.example.test", inputs)
	if err != nil {
		t.Fatal(err)
	}

	same := map[string][2]string{
		"an origin spelled with a default port and mixed case": {key, spelledOrigin},
		"CRLF and LF in the same typed paragraph":              {crlf, lf},
	}
	for name, pair := range same {
		if pair[0] != pair[1] {
			t.Fatalf("%s produced two keys: %q and %q", name, pair[0], pair[1])
		}
	}
	different := map[string][2]string{
		"a different input value":              {key, otherInput},
		"a different origin":                   {key, otherOrigin},
		"a different recipe digest":            {key, changedKey},
		"a different write step in one recipe": {key, secondStepKey},
		"two inputs whose values are swapped":  {leftSwap, rightSwap},
		"an absent input versus an empty one":  {absent, empty},
	}
	for name, pair := range different {
		if pair[0] == pair[1] {
			t.Fatalf("%s produced the same key %q", name, pair[0])
		}
	}
}

// Go randomises map iteration on purpose. A key derived by walking the input
// map would differ between two runs in the same process, which is the worst
// possible failure: the receipt exists and no rerun can find it.
func TestReceiptKeyDoesNotDependOnMapIterationOrder(t *testing.T) {
	value := cloneRecipe(receiptRecipe())
	value.Inputs = map[string]Input{
		"month": {Required: true}, "note": {}, "reference": {}, "recipient": {}, "memo": {},
	}
	inputs := map[string]string{
		"month": "2026-09", "note": "one", "reference": "two", "recipient": "three", "memo": "four",
	}
	first, err := ReceiptKey(value, value.Steps[0], receiptOrigin, inputs)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 64; attempt++ {
		again, err := ReceiptKey(value, value.Steps[0], receiptOrigin, inputs)
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("attempt %d produced key %q, first run produced %q", attempt, again, first)
		}
	}
}

func TestReceiptKeyRefusesWhatItCannotReproduce(t *testing.T) {
	value := receiptRecipe()
	step := value.Steps[0]
	tests := []struct {
		name   string
		origin string
		inputs map[string]string
		want   string
	}{
		{"missing required input", receiptOrigin, nil, "required input"},
		{"origin with a path", receiptOrigin + "/invoices", map[string]string{"month": "x"}, "exact origin"},
		{"origin with credentials", "https://user@billing.example.test", map[string]string{"month": "x"}, "exact origin"},
		{"not an origin at all", "billing.example.test", map[string]string{"month": "x"}, "exact origin"},
		{"input that is not valid UTF-8", receiptOrigin, map[string]string{"month": "\xff\xfe"}, "valid UTF-8"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ReceiptKey(value, step, test.origin, test.inputs); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want it to mention %q", err, test.want)
			}
		})
	}
}

func TestRequireWriteVerification(t *testing.T) {
	minimum := 1
	verification := Step{ID: "verify", Action: "assert", Assert: &Assertion{
		Kind: "element_count", Target: &Target{Role: "status", Name: "Sent"}, Min: &minimum,
	}}
	write := func(id string) Step {
		return Step{
			ID: id, Action: "click", Effect: "external_write",
			Target:         &Target{Role: "button", Name: "Send"},
			IdempotencyKey: "k:" + id,
			Postcondition:  &Event{Kind: "text.present", Match: "sent", TimeoutMS: 100},
		}
	}
	tests := []struct {
		name  string
		steps []Step
		ok    bool
	}{
		{"write followed by an assertion", []Step{write("a"), verification}, true},
		{"write with nothing after it", []Step{write("a")}, false},
		{"write followed only by another write", []Step{write("a"), write("b"), verification}, false},
		{"two writes each verified", []Step{write("a"), verification, write("b"), {ID: "verify2", Action: "assert", Assert: verification.Assert}}, true},
		{"no writes at all", []Step{{ID: "read", Action: "click", Effect: "read", Target: &Target{Role: "button", Name: "Send"}}}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := RequireWriteVerification(Recipe{Steps: test.steps})
			if (err == nil) != test.ok {
				t.Fatalf("err = %v, want ok=%v", err, test.ok)
			}
		})
	}
}
