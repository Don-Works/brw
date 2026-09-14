package siteconsent

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type scriptedPrompter struct {
	siteAnswer    bool
	siteAsked     int
	confirmAnswer bool
	confirmAsked  int
	lastRisks     []Risk
}

func (p *scriptedPrompter) AskSite(string, Scope) (bool, error) {
	p.siteAsked++
	return p.siteAnswer, nil
}

func (p *scriptedPrompter) ConfirmAction(_ ActionRequest, risks []Risk) (bool, error) {
	p.confirmAsked++
	p.lastRisks = risks
	return p.confirmAnswer, nil
}

func newTestGuard(t *testing.T, admin AdminConfig) *Guard {
	t.Helper()
	if err := admin.Normalize(); err != nil {
		t.Fatalf("normalize admin config: %v", err)
	}
	store, err := NewStoreWithKey(filepath.Join(t.TempDir(), "site-grants.json"), fixtureKey)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := NewGuard(store, admin)
	if err != nil {
		t.Fatal(err)
	}
	guard.SetGrantor("fixture-user")
	return guard
}

// TestUngrantedOriginIsRefusedByNameAndScope is acceptance criterion 1's first
// half: the refusal has to tell the agent which origin and which scope, because
// that is the whole of what the user has to be asked for.
func TestUngrantedOriginIsRefusedByNameAndScope(t *testing.T) {
	guard := newTestGuard(t, AdminConfig{})
	err := guard.Authorize("https://unknown.test/page", ScopeAct)
	var notGranted *NotGrantedError
	if !errors.As(err, &notGranted) {
		t.Fatalf("want a NotGrantedError, got %v", err)
	}
	if notGranted.Origin != "https://unknown.test" || notGranted.Scope != ScopeAct {
		t.Fatalf("refusal named origin=%q scope=%q", notGranted.Origin, notGranted.Scope)
	}
	if !strings.Contains(err.Error(), "https://unknown.test") || !strings.Contains(err.Error(), "act") {
		t.Fatalf("refusal text omits the origin or the scope: %v", err)
	}
}

func TestGrantedOriginIsAuthorized(t *testing.T) {
	guard := newTestGuard(t, AdminConfig{})
	if _, err := guard.Allow(GrantOptions{Origin: "https://granted.test", Scope: ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatalf("allow: %v", err)
	}
	if err := guard.Authorize("https://granted.test/deep/page?q=1", ScopeAct); err != nil {
		t.Fatalf("a granted origin was refused: %v", err)
	}
	if err := guard.Authorize("https://other.test/", ScopeAct); err == nil {
		t.Fatal("a grant on one origin authorised another")
	}
}

// TestExpiredGrantRePromptsInsteadOfPassing is acceptance criterion 1's second
// half.
func TestExpiredGrantRePromptsInsteadOfPassing(t *testing.T) {
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	guard := newTestGuard(t, AdminConfig{})
	guard.SetClock(func() time.Time { return base })
	if _, err := guard.Allow(GrantOptions{Origin: "https://expiring.test", Scope: ScopeAct, TTL: time.Hour, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	if err := guard.Authorize("https://expiring.test/", ScopeAct); err != nil {
		t.Fatalf("a live grant was refused: %v", err)
	}

	guard.SetClock(func() time.Time { return base.Add(2 * time.Hour) })
	err := guard.Authorize("https://expiring.test/", ScopeAct)
	var notGranted *NotGrantedError
	if !errors.As(err, &notGranted) || !notGranted.Expired {
		t.Fatalf("an expired grant must refuse and say so, got %v", err)
	}

	// Interactive: the same expired grant re-prompts rather than passing.
	prompter := &scriptedPrompter{siteAnswer: true}
	guard.SetPrompter(prompter)
	if err := guard.Authorize("https://expiring.test/", ScopeAct); err != nil {
		t.Fatalf("interactive re-grant failed: %v", err)
	}
	if prompter.siteAsked != 1 {
		t.Fatalf("expired grant asked %d times, want 1", prompter.siteAsked)
	}
}

// TestRevocationAppliesToTheNextAuthorize is acceptance criterion 1's third
// half: no restart, no new Guard.
func TestRevocationAppliesToTheNextAuthorize(t *testing.T) {
	guard := newTestGuard(t, AdminConfig{})
	if _, err := guard.Allow(GrantOptions{Origin: "https://revokeme.test", Scope: ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	if err := guard.Authorize("https://revokeme.test/", ScopeAct); err != nil {
		t.Fatalf("grant did not authorise: %v", err)
	}
	removed, err := guard.Revoke("https://revokeme.test", "", "fixture-user")
	if err != nil || removed != 1 {
		t.Fatalf("revoke removed=%d err=%v", removed, err)
	}
	if err := guard.Authorize("https://revokeme.test/", ScopeAct); err == nil {
		t.Fatal("a revoked origin is still authorised")
	}
}

// TestInteractivePromptIsAskedOnceAndTheAnswerRecorded covers both directions:
// a yes is not re-asked, and a no is not re-asked either.
func TestInteractivePromptIsAskedOnceAndTheAnswerRecorded(t *testing.T) {
	cases := []struct {
		name       string
		answer     bool
		wantSecond bool // whether the second Authorize should succeed
	}{
		{name: "yes is recorded", answer: true, wantSecond: true},
		{name: "no is recorded", answer: false, wantSecond: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			guard := newTestGuard(t, AdminConfig{})
			prompter := &scriptedPrompter{siteAnswer: c.answer}
			guard.SetPrompter(prompter)

			first := guard.Authorize("https://asked.test/", ScopeRead)
			second := guard.Authorize("https://asked.test/other", ScopeRead)
			if prompter.siteAsked != 1 {
				t.Fatalf("prompted %d times, want exactly 1", prompter.siteAsked)
			}
			if (first == nil) != c.answer {
				t.Fatalf("first Authorize err=%v for answer=%v", first, c.answer)
			}
			if (second == nil) != c.wantSecond {
				t.Fatalf("second Authorize err=%v, want success=%v", second, c.wantSecond)
			}
			if !c.answer {
				var denied *DeniedError
				if !errors.As(second, &denied) {
					t.Fatalf("a recorded refusal must surface as DeniedError, got %v", second)
				}
			}
		})
	}
}

// TestBlocklistedCategoryNeedsAnExplicitOverride is acceptance criterion 2.
func TestBlocklistedCategoryNeedsAnExplicitOverride(t *testing.T) {
	guard := newTestGuard(t, AdminConfig{})
	// Interactive: a prompt must NOT be able to grant a blocklisted category.
	prompter := &scriptedPrompter{siteAnswer: true}
	guard.SetPrompter(prompter)

	err := guard.Authorize("https://paypal.com/checkout", ScopeAct)
	var blocked *CategoryBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("a blocklisted origin must refuse with CategoryBlockedError, got %v", err)
	}
	if blocked.Category.Name != "financial-services" {
		t.Fatalf("refusal named category %q", blocked.Category.Name)
	}
	if prompter.siteAsked != 0 {
		t.Fatal("a blocklisted origin was offered to the interactive prompt")
	}
	if !strings.Contains(err.Error(), "override") || !strings.Contains(err.Error(), blocked.Source) {
		t.Fatalf("refusal must name the override and the list source: %v", err)
	}

	// Allow without the flag is refused too.
	if _, err := guard.Allow(GrantOptions{Origin: "https://paypal.com", Scope: ScopeAct, Actor: "fixture-user"}); !errors.As(err, &blocked) {
		t.Fatalf("Allow without an override must refuse, got %v", err)
	}
	// The wrong category name is not an override either.
	if _, err := guard.Allow(GrantOptions{Origin: "https://paypal.com", Scope: ScopeAct, OverrideCategory: "adult", Actor: "fixture-user"}); !errors.As(err, &blocked) {
		t.Fatalf("an override naming another category must refuse, got %v", err)
	}
	if err := guard.Authorize("https://paypal.com/checkout", ScopeAct); !errors.As(err, &blocked) {
		t.Fatalf("a refused override must leave the origin refused, got %v", err)
	}

	// With the right override it is granted, and the ledger says who and when.
	grant, err := guard.Allow(GrantOptions{Origin: "https://paypal.com", Scope: ScopeAct, OverrideCategory: "financial-services", Actor: "fixture-operator", Note: "fixture reason"})
	if err != nil {
		t.Fatalf("override grant failed: %v", err)
	}
	if grant.OverrideCategory != "financial-services" {
		t.Fatalf("grant did not record the override: %+v", grant)
	}
	if err := guard.Authorize("https://paypal.com/checkout", ScopeAct); err != nil {
		t.Fatalf("an overridden origin is still refused: %v", err)
	}

	entries, err := guard.Store().Ledger()
	if err != nil {
		t.Fatal(err)
	}
	var found *LedgerEntry
	for i := range entries {
		if entries[i].Event == "category-override" {
			found = &entries[i]
		}
	}
	if found == nil {
		t.Fatalf("the override is not in the ledger: %+v", entries)
	}
	if found.Actor != "fixture-operator" || found.OverrideCategory != "financial-services" || found.At.IsZero() {
		t.Fatalf("ledger entry does not record who and when: %+v", found)
	}
}

func TestAdminListsDecideWithoutAGrant(t *testing.T) {
	guard := newTestGuard(t, AdminConfig{
		AllowedOrigins: []string{"intranet.example.test"},
		BlockedOrigins: []string{"forbidden.example.test"},
	})
	prompter := &scriptedPrompter{siteAnswer: true}
	guard.SetPrompter(prompter)

	if err := guard.Authorize("https://app.intranet.example.test/x", ScopeAct); err != nil {
		t.Fatalf("an admin-allowed origin was refused: %v", err)
	}
	if prompter.siteAsked != 0 {
		t.Fatal("an admin-allowed origin still prompted")
	}
	if got := guard.Store().List(); len(got) != 0 {
		t.Fatalf("an admin allowlist wrote local grant records: %+v", got)
	}

	err := guard.Authorize("https://forbidden.example.test/x", ScopeRead)
	var adminBlocked *AdminBlockedError
	if !errors.As(err, &adminBlocked) {
		t.Fatalf("an admin-blocked origin must refuse with AdminBlockedError, got %v", err)
	}
	if _, err := guard.Allow(GrantOptions{Origin: "https://forbidden.example.test", Scope: ScopeRead, Actor: "fixture-user"}); !errors.As(err, &adminBlocked) {
		t.Fatalf("an admin-blocked origin must not be grantable, got %v", err)
	}
}

func TestAdminCategoryDomainsExtendTheShippedList(t *testing.T) {
	guard := newTestGuard(t, AdminConfig{
		CategoryDomains: map[string][]string{"financial-services": {"ledger.example.test"}},
	})
	err := guard.Authorize("https://ledger.example.test/", ScopeRead)
	var blocked *CategoryBlockedError
	if !errors.As(err, &blocked) || blocked.Category.Name != "financial-services" {
		t.Fatalf("an admin-added category domain must be blocklisted, got %v", err)
	}
}

// TestHighRiskActionIsRefusedWhenNonInteractive is acceptance criterion 4.
func TestHighRiskActionIsRefusedWhenNonInteractive(t *testing.T) {
	guard := newTestGuard(t, AdminConfig{ConfirmActions: true})
	if _, err := guard.Allow(GrantOptions{Origin: "https://shop.example.test", Scope: ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		request ActionRequest
		refuse  bool
		class   RiskClass
	}{
		{
			name:    "purchase button",
			request: ActionRequest{Tool: "brw_click", Origin: "https://shop.example.test", Label: "Place order"},
			refuse:  true,
			class:   RiskPurchase,
		},
		{
			name:    "publish button",
			request: ActionRequest{Tool: "brw_click_text", Origin: "https://shop.example.test", Text: "Publish"},
			refuse:  true,
			class:   RiskPublish,
		},
		{
			name:    "form carrying personal data",
			request: ActionRequest{Tool: "brw_fill", Origin: "https://shop.example.test", Label: "Continue", Fields: []string{"Full name", "Card number"}},
			refuse:  true,
			class:   RiskPersonalData,
		},
		{
			name:    "any action on a blocklisted category",
			request: ActionRequest{Tool: "brw_click", Origin: "https://pornhub.com", Label: "Next"},
			refuse:  true,
			class:   RiskBlocklistedCategory,
		},
		{
			name:    "ordinary click",
			request: ActionRequest{Tool: "brw_click", Origin: "https://shop.example.test", Label: "Show more results"},
			refuse:  false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := guard.CheckAction(c.request)
			if !c.refuse {
				if err != nil {
					t.Fatalf("an ordinary action was refused: %v", err)
				}
				return
			}
			var required *ConfirmationRequiredError
			if !errors.As(err, &required) {
				t.Fatalf("want ConfirmationRequiredError, got %v", err)
			}
			var classes []RiskClass
			for _, risk := range required.Risks {
				classes = append(classes, risk.Class)
			}
			if !containsClass(classes, c.class) {
				t.Fatalf("classified as %v, want %s", classes, c.class)
			}
			if !strings.Contains(err.Error(), "non-interactive") {
				t.Fatalf("the refusal must say why nobody confirmed: %v", err)
			}
		})
	}
}

func TestConfirmActionsOffLeavesActionsAlone(t *testing.T) {
	guard := newTestGuard(t, AdminConfig{ConfirmActions: false})
	if err := guard.CheckAction(ActionRequest{Tool: "brw_click", Origin: "https://shop.example.test", Label: "Place order"}); err != nil {
		t.Fatalf("confirm-actions is off, so nothing should be gated: %v", err)
	}
}

func TestConfirmActionPromptDecidesWhenInteractive(t *testing.T) {
	for _, answer := range []bool{true, false} {
		guard := newTestGuard(t, AdminConfig{ConfirmActions: true})
		prompter := &scriptedPrompter{confirmAnswer: answer}
		guard.SetPrompter(prompter)
		err := guard.CheckAction(ActionRequest{Tool: "brw_click", Origin: "https://shop.example.test", Label: "Buy now"})
		if prompter.confirmAsked != 1 {
			t.Fatalf("confirm asked %d times", prompter.confirmAsked)
		}
		if answer && err != nil {
			t.Fatalf("a confirmed action was still refused: %v", err)
		}
		var declined *ConfirmationDeclinedError
		if !answer && !errors.As(err, &declined) {
			t.Fatalf("a declined action must refuse with ConfirmationDeclinedError, got %v", err)
		}
	}
}

func TestDisabledGuardAuthorizesEverything(t *testing.T) {
	guard, err := NewGuard(nil, AdminConfig{ConfirmActions: true})
	if err != nil {
		t.Fatal(err)
	}
	if guard.Enabled() {
		t.Fatal("a guard with no store reports itself enabled")
	}
	if err := guard.Authorize("https://anything.test/", ScopeAct); err != nil {
		t.Fatalf("consent is opt-in; a storeless guard must pass: %v", err)
	}
	if err := guard.CheckAction(ActionRequest{Tool: "brw_click", Origin: "https://anything.test", Label: "Buy now"}); err != nil {
		t.Fatalf("a storeless guard must not gate actions: %v", err)
	}
}

func TestOriginlessTargetsNeedNoConsent(t *testing.T) {
	guard := newTestGuard(t, AdminConfig{})
	for _, target := range []string{"", "about:blank", "about:newtab", "data:text/html,hi", "/relative/path"} {
		if err := guard.Authorize(target, ScopeAct); err != nil {
			t.Errorf("Authorize(%q) = %v, want nil: there is no site there to consent to", target, err)
		}
	}
}

func containsClass(classes []RiskClass, want RiskClass) bool {
	for _, class := range classes {
		if class == want {
			return true
		}
	}
	return false
}
