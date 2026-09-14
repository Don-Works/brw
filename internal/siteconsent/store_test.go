package siteconsent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureKey is an obviously fabricated 32-byte consent key. It is not a secret
// and never leaves the test process.
var fixtureKey = []byte("fixture-consent-key-one-two-three")

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStoreWithKey(filepath.Join(t.TempDir(), "site-grants.json"), fixtureKey)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return store
}

func TestStoreRecordsAndLooksUpGrants(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		record    Grant
		lookupURL string
		want      Scope
		at        time.Time
		found     bool
	}{
		{
			name:      "read grant answers a read request",
			record:    Grant{Origin: "https://example.test", Scope: ScopeRead, Decision: DecisionAllow, GrantedAt: now, GrantedBy: "fixture-user"},
			lookupURL: "https://example.test",
			want:      ScopeRead,
			at:        now,
			found:     true,
		},
		{
			name:      "read grant does not answer an act request",
			record:    Grant{Origin: "https://example.test", Scope: ScopeRead, Decision: DecisionAllow, GrantedAt: now, GrantedBy: "fixture-user"},
			lookupURL: "https://example.test",
			want:      ScopeAct,
			at:        now,
			found:     false,
		},
		{
			name:      "act grant covers a read request",
			record:    Grant{Origin: "https://example.test", Scope: ScopeAct, Decision: DecisionAllow, GrantedAt: now, GrantedBy: "fixture-user"},
			lookupURL: "https://example.test",
			want:      ScopeRead,
			at:        now,
			found:     true,
		},
		{
			name:      "expired grant is not found",
			record:    Grant{Origin: "https://example.test", Scope: ScopeAct, Decision: DecisionAllow, GrantedAt: now, GrantedBy: "fixture-user", Expiry: now.Add(time.Hour)},
			lookupURL: "https://example.test",
			want:      ScopeAct,
			at:        now.Add(2 * time.Hour),
			found:     false,
		},
		{
			name:      "a different port is a different origin",
			record:    Grant{Origin: "https://example.test:8443", Scope: ScopeAct, Decision: DecisionAllow, GrantedAt: now, GrantedBy: "fixture-user"},
			lookupURL: "https://example.test",
			want:      ScopeAct,
			at:        now,
			found:     false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newTestStore(t)
			if _, err := store.Record(c.record); err != nil {
				t.Fatalf("record: %v", err)
			}
			origin, err := CanonicalOrigin(c.lookupURL)
			if err != nil {
				t.Fatalf("canonical origin: %v", err)
			}
			_, found := store.Lookup(origin, c.want, c.at)
			if found != c.found {
				t.Fatalf("Lookup(%s, %s) found=%v, want %v", origin, c.want, found, c.found)
			}
		})
	}
}

// TestHandWrittenRecordWithoutValidMACIsRefused is the forgery test. A local
// process writes itself a grant straight into the file; the store must not
// honour it, and Lookup must keep answering "no grant".
func TestHandWrittenRecordWithoutValidMACIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "site-grants.json")

	forged := []struct {
		name string
		mac  string
	}{
		{name: "no mac at all", mac: ""},
		{name: "mac of another record", mac: macOf(t, Grant{Origin: "https://elsewhere.test", Scope: ScopeRead, Decision: DecisionAllow, GrantedBy: "fixture-user"})},
		{name: "arbitrary bytes", mac: "bm90LWEtcmVhbC1tYWM"},
	}
	for _, c := range forged {
		t.Run(c.name, func(t *testing.T) {
			payload := storeFile{Version: storeVersion, Grants: []Grant{{
				Origin:    "https://attacker.test",
				Scope:     ScopeAct,
				Decision:  DecisionAllow,
				GrantedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
				GrantedBy: "fixture-user",
				MAC:       c.mac,
			}}}
			data, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := NewStoreWithKey(path, fixtureKey)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			if got := store.List(); len(got) != 0 {
				t.Fatalf("forged record was loaded: %+v", got)
			}
			if _, found := store.Lookup("https://attacker.test", ScopeAct, time.Now()); found {
				t.Fatal("forged record authorised an action")
			}
			rejected := store.Rejected()
			if len(rejected) != 1 || !strings.Contains(rejected[0].Reason, "not authenticated") {
				t.Fatalf("rejection was not reported as an authentication failure: %+v", rejected)
			}
		})
	}
}

// TestTamperedFieldInvalidatesTheMAC proves the MAC covers each authorising
// field, so a record cannot be widened in place by editing the file.
func TestTamperedFieldInvalidatesTheMAC(t *testing.T) {
	base := Grant{
		Origin:    "https://example.test",
		Scope:     ScopeRead,
		Decision:  DecisionAllow,
		GrantedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		GrantedBy: "fixture-user",
		Expiry:    time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC),
	}
	base.Sign(fixtureKey)
	if err := base.Verify(fixtureKey); err != nil {
		t.Fatalf("freshly signed record does not verify: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(g *Grant)
	}{
		{"origin", func(g *Grant) { g.Origin = "https://attacker.test" }},
		{"scope widened to act", func(g *Grant) { g.Scope = ScopeAct }},
		{"decision flipped to deny", func(g *Grant) { g.Decision = DecisionDeny }},
		{"expiry pushed out", func(g *Grant) { g.Expiry = g.Expiry.Add(10000 * time.Hour) }},
		{"expiry removed", func(g *Grant) { g.Expiry = time.Time{} }},
		{"grantor rewritten", func(g *Grant) { g.GrantedBy = "somebody-else" }},
		{"granted_at backdated", func(g *Grant) { g.GrantedAt = g.GrantedAt.Add(-time.Hour) }},
		{"category override added", func(g *Grant) { g.OverrideCategory = "financial-services" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tampered := base
			c.mutate(&tampered)
			if err := tampered.Verify(fixtureKey); err == nil {
				t.Fatalf("tampering with %s left the MAC valid", c.name)
			}
		})
	}
}

// TestDifferentKeyRejectsRecords proves the MAC is keyed: a grant file copied
// from another user's profile does not authorise anything here.
func TestDifferentKeyRejectsRecords(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "site-grants.json")
	mine, err := NewStoreWithKey(path, fixtureKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mine.Record(Grant{Origin: "https://example.test", Scope: ScopeAct, Decision: DecisionAllow, GrantedBy: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	theirs, err := NewStoreWithKey(path, []byte("fixture-consent-key-four-five-six"))
	if err != nil {
		t.Fatal(err)
	}
	if got := theirs.List(); len(got) != 0 {
		t.Fatalf("a record signed with another key was honoured: %+v", got)
	}
}

// TestRevocationTakesEffectWithoutReopeningTheStore proves the "next action, no
// daemon restart" requirement: one Store handle answers differently after
// another process rewrites the file.
func TestRevocationTakesEffectWithoutReopeningTheStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "site-grants.json")
	daemon, err := NewStoreWithKey(path, fixtureKey)
	if err != nil {
		t.Fatal(err)
	}
	control, err := NewStoreWithKey(path, fixtureKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.Record(Grant{Origin: "https://example.test", Scope: ScopeAct, Decision: DecisionAllow, GrantedBy: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	if _, found := daemon.Lookup("https://example.test", ScopeAct, time.Now()); !found {
		t.Fatal("the long-lived handle never saw the grant another process wrote")
	}
	// Move the file's timestamp backwards is not needed: the write above and the
	// revoke below are separate rename operations, so mtime or size moves.
	if removed, err := control.Revoke("https://example.test", ""); err != nil || removed != 1 {
		t.Fatalf("revoke: removed=%d err=%v", removed, err)
	}
	if _, found := daemon.Lookup("https://example.test", ScopeAct, time.Now()); found {
		t.Fatal("the long-lived handle still honours a revoked grant")
	}
}

func TestRevokeAllClearsEveryRecord(t *testing.T) {
	store := newTestStore(t)
	for _, origin := range []string{"https://one.test", "https://two.test", "https://three.test"} {
		if _, err := store.Record(Grant{Origin: origin, Scope: ScopeRead, Decision: DecisionAllow, GrantedBy: "fixture-user"}); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := store.RevokeAll()
	if err != nil || removed != 3 {
		t.Fatalf("RevokeAll removed=%d err=%v", removed, err)
	}
	if got := store.List(); len(got) != 0 {
		t.Fatalf("records survived RevokeAll: %+v", got)
	}
}

func TestRecordReplacesTheSameOriginAndScope(t *testing.T) {
	store := newTestStore(t)
	for i := 0; i < 3; i++ {
		if _, err := store.Record(Grant{Origin: "https://example.test", Scope: ScopeRead, Decision: DecisionAllow, GrantedBy: "fixture-user"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := store.List(); len(got) != 1 {
		t.Fatalf("re-recording one origin+scope produced %d records", len(got))
	}
}

func TestLedgerRoundTrips(t *testing.T) {
	store := newTestStore(t)
	entries := []LedgerEntry{
		{Event: "allow", Origin: "https://one.test", Scope: ScopeRead, Decision: DecisionAllow, Actor: "fixture-user"},
		{Event: "category-override", Origin: "https://two.test", Scope: ScopeAct, Decision: DecisionAllow, Actor: "fixture-user", OverrideCategory: "financial-services"},
	}
	for _, entry := range entries {
		if err := store.AppendLedger(entry); err != nil {
			t.Fatal(err)
		}
	}
	read, err := store.Ledger()
	if err != nil {
		t.Fatal(err)
	}
	if len(read) != 2 || read[1].OverrideCategory != "financial-services" || read[1].Actor != "fixture-user" {
		t.Fatalf("ledger did not round-trip: %+v", read)
	}
}

func macOf(t *testing.T, g Grant) string {
	t.Helper()
	g.Sign(fixtureKey)
	return g.MAC
}
