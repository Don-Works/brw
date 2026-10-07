package siteconsent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func unwritableLedger(t *testing.T, store *Store) {
	t.Helper()
	if err := os.MkdirAll(LedgerPathFor(store.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerFailuresAreReported(t *testing.T) {
	cases := []struct {
		name string
		act  func(t *testing.T, guard *Guard)
	}{
		{
			name: "a prompt that was answered",
			act: func(t *testing.T, guard *Guard) {
				guard.SetPrompter(&scriptedPrompter{siteAnswer: true})
				if err := guard.Authorize("https://asked.test/page", ScopeRead); err != nil {
					t.Fatalf("a prompted yes did not authorise: %v", err)
				}
			},
		},
		{
			name: "a revocation",
			act: func(t *testing.T, guard *Guard) {
				if _, err := guard.Store().Record(Grant{Origin: "https://gone.test", Scope: ScopeAct, Decision: DecisionAllow, GrantedAt: time.Now(), GrantedBy: "fixture-user"}); err != nil {
					t.Fatal(err)
				}
				if removed, err := guard.Revoke("https://gone.test", "", "fixture-user"); err != nil || removed != 1 {
					t.Fatalf("revoke: removed=%d err=%v", removed, err)
				}
			},
		},
		{
			name: "a revoke-all",
			act: func(t *testing.T, guard *Guard) {
				if _, err := guard.Store().Record(Grant{Origin: "https://gone.test", Scope: ScopeAct, Decision: DecisionAllow, GrantedAt: time.Now(), GrantedBy: "fixture-user"}); err != nil {
					t.Fatal(err)
				}
				if removed, err := guard.RevokeAll("fixture-user"); err != nil || removed != 1 {
					t.Fatalf("revoke-all: removed=%d err=%v", removed, err)
				}
			},
		},
		{
			name: "a confirmation a person gave",
			act: func(t *testing.T, guard *Guard) {
				guard.SetPrompter(&scriptedPrompter{siteAnswer: true, confirmAnswer: true})
				if err := guard.CheckAction(ActionRequest{Tool: "brw_click", Origin: "https://shop.test", Label: "Place order"}); err != nil {
					t.Fatalf("an allowed confirmation returned %v", err)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			guard := newTestGuard(t, AdminConfig{ConfirmActions: true})
			unwritableLedger(t, guard.Store())
			var reported []error
			guard.SetLedgerErrorHandler(func(err error) { reported = append(reported, err) })
			c.act(t, guard)
			if len(reported) == 0 {
				t.Fatal("the ledger write failed and nothing reported it, so the event is gone with no signal")
			}
		})
	}
}

func TestAllowReturnsTheGrantWhenOnlyTheLedgerFailed(t *testing.T) {
	guard := newTestGuard(t, AdminConfig{})
	unwritableLedger(t, guard.Store())
	grant, err := guard.Allow(GrantOptions{Origin: "https://example.test", Scope: ScopeAct, Actor: "fixture-user"})
	if err == nil {
		t.Fatal("a failed ledger write was silent")
	}
	if !strings.Contains(err.Error(), "was recorded") {
		t.Fatalf("the error does not say the grant exists: %v", err)
	}
	if grant.Origin != "https://example.test" || grant.MAC == "" {
		t.Fatalf("the persisted grant was not returned: %+v", grant)
	}
	if err := guard.Authorize("https://example.test/page", ScopeAct); err != nil {
		t.Fatalf("the grant the caller was told about does not authorise: %v", err)
	}
}

func TestLedgerPathIsBesideTheStore(t *testing.T) {
	store := newTestStore(t)
	if filepath.Dir(LedgerPathFor(store.Path())) != filepath.Dir(store.Path()) {
		t.Fatalf("ledger %q is not beside %q", LedgerPathFor(store.Path()), store.Path())
	}
}
