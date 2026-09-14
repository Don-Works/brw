package siteconsent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unwritableLedger makes every ledger append fail, the way a full disk or a
// permissions change does: the ledger path is taken by a directory.
func unwritableLedger(t *testing.T, store *Store) {
	t.Helper()
	if err := os.MkdirAll(LedgerPathFor(store.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestLedgerFailuresAreReported covers the events that are recorded ONLY in the
// ledger. A prompt answered, a grant revoked and a confirmation given leave no
// other trace, so a swallowed write makes them vanish with no signal at all.
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

// TestAllowReturnsTheGrantWhenOnlyTheLedgerFailed: the grant is already
// persisted and already authorising by then, so reporting a bare error said
// "nothing happened" about a record that exists.
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

// TestLedgerPathIsBesideTheStore keeps the two files together, since operating
// on one without the other is how a ledger ends up describing another profile.
func TestLedgerPathIsBesideTheStore(t *testing.T) {
	store := newTestStore(t)
	if filepath.Dir(LedgerPathFor(store.Path())) != filepath.Dir(store.Path()) {
		t.Fatalf("ledger %q is not beside %q", LedgerPathFor(store.Path()), store.Path())
	}
}
