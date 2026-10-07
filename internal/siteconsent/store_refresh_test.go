package siteconsent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestASameLengthRewriteIsNoticed(t *testing.T) {
	granted := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "site-grants.json")

	daemon, err := NewStoreWithKey(path, fixtureKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.Record(Grant{
		Origin: "https://aaa.test", Scope: ScopeAct, Decision: DecisionAllow,
		GrantedAt: granted, GrantedBy: "fixture-user",
	}); err != nil {
		t.Fatal(err)
	}
	if _, found := daemon.Lookup("https://aaa.test", ScopeAct, granted); !found {
		t.Fatal("the store did not record its own grant")
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	other := filepath.Join(dir, "other.json")
	elsewhere, err := NewStoreWithKey(other, fixtureKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := elsewhere.Record(Grant{
		Origin: "https://bbb.test", Scope: ScopeAct, Decision: DecisionAllow,
		GrantedAt: granted, GrantedBy: "fixture-user",
	}); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.ReadFile(other)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(replacement)) != before.Size() {
		t.Fatalf("the replacement is %d bytes and the original %d; this test only means something when they match", len(replacement), before.Size())
	}
	if err := os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("the rewrite moved size or mtime (%d/%s -> %d/%s), so it is not the case this test is about",
			before.Size(), before.ModTime(), after.Size(), after.ModTime())
	}

	if _, found := daemon.Lookup("https://aaa.test", ScopeAct, granted); found {
		t.Fatal("the long-lived handle still honours a grant that is no longer in the file")
	}
	if _, found := daemon.Lookup("https://bbb.test", ScopeAct, granted); !found {
		t.Fatal("the long-lived handle never saw the record that replaced it")
	}
}

func TestChangedStoreFailsClosedAndPreservesUnreadableData(t *testing.T) {
	for _, kind := range []string{"malformed", "future schema", "directory", "removed"} {
		t.Run(kind, func(t *testing.T) {
			store := newTestStore(t)
			grant := Grant{Origin: "https://example.test", Scope: ScopeAct, Decision: DecisionAllow, GrantedBy: "fixture-user"}
			if _, err := store.Record(grant); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(store.Path())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(store.Path()); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "malformed":
				err = os.WriteFile(store.Path(), []byte(`{"version":`), 0600)
			case "future schema":
				err = os.WriteFile(store.Path(), []byte(`{"version":2,"grants":[]}`), 0600)
			case "directory":
				err = os.Mkdir(store.Path(), 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, found := store.Lookup(grant.Origin, ScopeAct, time.Now()); found {
				t.Fatal("an unreadable or removed store retained its previous authority")
			}
			if len(store.List()) != 0 {
				t.Fatal("stale grants remained visible")
			}
			if kind != "removed" {
				if len(store.Rejected()) != 1 {
					t.Fatal("the load failure was not reported")
				}
				if _, err := store.Record(grant); err == nil {
					t.Fatal("record overwrote an unreadable store")
				}
				if _, err := store.Revoke(grant.Origin, ""); err == nil {
					t.Fatal("revoke ignored an unreadable store")
				}
				if _, err := store.RevokeAll(); err == nil {
					t.Fatal("revoke-all ignored an unreadable store")
				}
				if err := os.Remove(store.Path()); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(store.Path(), original, 0600); err != nil {
				t.Fatal(err)
			}
			if _, found := store.Lookup(grant.Origin, ScopeAct, time.Now()); !found {
				t.Fatal("the restored authenticated store was not reloaded")
			}
		})
	}
}

func TestADenyAnswersTheScopeItWasGiven(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		denied    Scope
		asked     Scope
		wantFound bool
	}{
		{name: "a deny on act does not answer a read", denied: ScopeAct, asked: ScopeRead, wantFound: false},
		{name: "a deny on act answers an act", denied: ScopeAct, asked: ScopeAct, wantFound: true},
		{name: "a deny on read answers a read", denied: ScopeRead, asked: ScopeRead, wantFound: true},
		{name: "a deny on read also refuses acting", denied: ScopeRead, asked: ScopeAct, wantFound: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newTestStore(t)
			if _, err := store.Record(Grant{
				Origin: "https://refused.test", Scope: c.denied, Decision: DecisionDeny,
				GrantedAt: now, GrantedBy: "fixture-user",
			}); err != nil {
				t.Fatal(err)
			}
			grant, found := store.Lookup("https://refused.test", c.asked, now)
			if found != c.wantFound {
				t.Fatalf("a deny at %s answered a %s request with found=%v, want %v", c.denied, c.asked, found, c.wantFound)
			}
			if found && grant.Decision != DecisionDeny {
				t.Fatalf("the record returned is %s, not the deny", grant.Decision)
			}
		})
	}
}

func TestADenyOnActLeavesReadToBeAsked(t *testing.T) {
	guard := newTestGuard(t, AdminConfig{})
	if _, err := guard.Store().Record(Grant{
		Origin: "https://refused.test", Scope: ScopeAct, Decision: DecisionDeny,
		GrantedAt: time.Now(), GrantedBy: "fixture-user",
	}); err != nil {
		t.Fatal(err)
	}
	var denied *DeniedError
	if err := guard.Authorize("https://refused.test/page", ScopeAct); !asDenied(err, &denied) {
		t.Fatalf("acting on a refused origin answered %v", err)
	}
	var notGranted *NotGrantedError
	err := guard.Authorize("https://refused.test/page", ScopeRead)
	if !asNotGranted(err, &notGranted) {
		t.Fatalf("a refusal to ACT also refused reading: %v", err)
	}
}

func asDenied(err error, target **DeniedError) bool {
	denied, ok := err.(*DeniedError)
	if ok {
		*target = denied
	}
	return ok
}
