package sessionstate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Deliberately fabricated, low-entropy fixture material. These are not
// credentials; they are strings a test can grep the ciphertext for.
const (
	fixtureCookieName  = "fixture-session-cookie-one"
	fixtureCookieValue = "fixture-session-value-one"
	fixtureKey         = "fixture-session-state-key-for-tests-0001"
)

func testStore(t *testing.T, now func() time.Time) *Store {
	t.Helper()
	// A nested path, not t.TempDir() itself: the store creates what it owns at
	// 0700 and refuses a pre-existing permissive directory, and t.TempDir hands
	// back a 0755 one.
	store, err := NewStore(Config{Root: filepath.Join(t.TempDir(), "state"), Key: []byte(fixtureKey), Now: now})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func fixtureSnapshot() Snapshot {
	return Snapshot{
		Origins: []string{"https://app.example.test"},
		Cookies: []Cookie{{
			Name: fixtureCookieName, Value: fixtureCookieValue,
			Domain: "app.example.test", Path: "/", HTTPOnly: true, Secure: true,
		}},
	}
}

func TestParseAllowlistRejectsSuffixSweepsAndPatterns(t *testing.T) {
	cases := []struct {
		name    string
		input   []string
		want    string
		origins []string
	}{
		{name: "exact https origin", input: []string{"https://app.example.test"}, origins: []string{"https://app.example.test"}},
		{name: "bare host defaults to https", input: []string{"app.example.test"}, origins: []string{"https://app.example.test"}},
		{name: "port is part of the origin", input: []string{"http://127.0.0.1:8080"}, origins: []string{"http://127.0.0.1:8080"}},
		{name: "localhost is a legitimate single label", input: []string{"http://localhost:3000"}, origins: []string{"http://localhost:3000"}},
		{name: "duplicates collapse", input: []string{"https://a.example.test", "https://a.example.test"}, origins: []string{"https://a.example.test"}},
		{name: "empty list", input: nil, want: "origins is required"},
		{name: "wildcard", input: []string{"https://*.example.test"}, want: "is a pattern"},
		{name: "leading dot", input: []string{"https://.example.test"}, want: "starts with a dot"},
		{name: "single label sweeps a suffix", input: []string{"https://test"}, want: "single-label host"},
		{name: "non http scheme", input: []string{"file:///etc"}, want: "must be http or https"},
		{name: "blank entry", input: []string{"  "}, want: "empty origin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allow, err := ParseAllowlist(tc.input)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %v, want one containing %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAllowlist: %v", err)
			}
			got := allow.Origins()
			if len(got) != len(tc.origins) {
				t.Fatalf("origins = %v, want %v", got, tc.origins)
			}
			for i := range got {
				if got[i] != tc.origins[i] {
					t.Fatalf("origins = %v, want %v", got, tc.origins)
				}
			}
		})
	}
}

func TestAllowlistCookieDomainMatching(t *testing.T) {
	allow, err := ParseAllowlist([]string{"https://shop.example.test"})
	if err != nil {
		t.Fatalf("ParseAllowlist: %v", err)
	}
	cases := []struct {
		domain string
		want   bool
	}{
		{"shop.example.test", true},
		{".shop.example.test", true},
		{".example.test", true}, // a parent-domain cookie applies to the host
		{"example.test", false}, // an exact sibling host is not the allowlisted one
		{"other.example.test", false},
		{"evil.test", false},
		{".test", false},
		{"", false},
		{"notshop.example.test", false},
	}
	for _, tc := range cases {
		if got := allow.AllowsCookieDomain(tc.domain); got != tc.want {
			t.Errorf("AllowsCookieDomain(%q) = %v, want %v", tc.domain, got, tc.want)
		}
	}
}

func TestRestrictDropsEverythingOutsideTheAllowlist(t *testing.T) {
	allow, err := ParseAllowlist([]string{"https://app.example.test"})
	if err != nil {
		t.Fatalf("ParseAllowlist: %v", err)
	}
	cookies := []Cookie{
		{Name: "keep", Domain: "app.example.test"},
		{Name: "keep-parent", Domain: ".example.test"},
		{Name: "drop-other-host", Domain: "evil.test"},
		{Name: "tracking-id", Domain: "app.example.test"},
	}
	kept, skipped, err := Restrict(cookies, allow, []string{"tracking-*"})
	if err != nil {
		t.Fatalf("Restrict: %v", err)
	}
	if skipped != 2 {
		t.Fatalf("skipped = %d, want 2 (one off-allowlist, one redacted)", skipped)
	}
	for _, cookie := range kept {
		if cookie.Domain == "evil.test" || strings.HasPrefix(cookie.Name, "tracking-") {
			t.Fatalf("Restrict kept %q on %q", cookie.Name, cookie.Domain)
		}
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d cookies, want 2", len(kept))
	}
	if _, _, err := Restrict(cookies, Allowlist{}, nil); !errors.Is(err, ErrNoOrigins) {
		t.Fatalf("an empty allowlist must refuse, got %v", err)
	}
	if _, _, err := Restrict(cookies, allow, []string{"["}); err == nil {
		t.Fatal("a malformed redact glob must be an error, not a silently ignored filter")
	}
}

func TestStoreRoundTripsASnapshot(t *testing.T) {
	store := testStore(t, nil)
	meta, err := store.Save(fixtureSnapshot(), SaveOptions{})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if meta.CookieCount != 1 || meta.ID == "" {
		t.Fatalf("meta = %+v, want one cookie and an id", meta)
	}
	snapshot, loaded, err := store.Load(meta.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ID != meta.ID {
		t.Fatalf("loaded id = %q, want %q", loaded.ID, meta.ID)
	}
	if len(snapshot.Cookies) != 1 || snapshot.Cookies[0].Value != fixtureCookieValue {
		t.Fatalf("round trip lost the cookie: %+v", snapshot.Cookies)
	}
	if !snapshot.Cookies[0].HTTPOnly || !snapshot.Cookies[0].Secure {
		t.Fatal("round trip lost the cookie's http_only/secure attributes, so a restore would weaken the session")
	}

	listed, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != meta.ID {
		t.Fatalf("List = %+v, want the one snapshot", listed)
	}
	if err := store.Delete(meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := store.Load(meta.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load after delete = %v, want ErrNotFound", err)
	}
	if err := store.Delete(meta.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting an unknown id = %v, want ErrNotFound", err)
	}
}

func TestStoreRefusesWithoutAnAtRestKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  []byte
	}{
		{"no key at all", nil},
		{"key below the entropy floor", []byte("fixture-short-key")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewStore(Config{Root: t.TempDir(), Key: tc.key}); !errors.Is(err, ErrEncryptionKeyRequired) {
				t.Fatalf("NewStore = %v, want ErrEncryptionKeyRequired — there is no unencrypted mode", err)
			}
		})
	}
}

func TestStoreRefusesAGroupReadableRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := NewStore(Config{Root: root, Key: []byte(fixtureKey)})
	if err == nil || !strings.Contains(err.Error(), "beyond its owner") {
		t.Fatalf("NewStore on a 0755 root = %v, want a permissions refusal", err)
	}
}

// The whole security argument rests on the snapshot being unreadable beside the
// ciphertext and unreachable by another local account.
func TestSealedSnapshotIsCiphertextAndOwnerOnly(t *testing.T) {
	store := testStore(t, nil)
	meta, err := store.Save(fixtureSnapshot(), SaveOptions{})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	path := filepath.Join(store.Root(), meta.ID+fileSuffix)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sealed file: %v", err)
	}
	for _, secret := range []string{fixtureCookieValue, fixtureCookieName, "app.example.test"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("the sealed snapshot contains %q in the clear", secret)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode = %04o, want 0600", info.Mode().Perm())
	}
	rootInfo, err := os.Stat(store.Root())
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	if rootInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("root mode = %04o, want owner-only", rootInfo.Mode().Perm())
	}
}

func TestTamperedOrRelabelledSnapshotFailsAuthentication(t *testing.T) {
	store := testStore(t, nil)
	meta, err := store.Save(fixtureSnapshot(), SaveOptions{})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	path := filepath.Join(store.Root(), meta.ID+fileSuffix)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	t.Run("a flipped byte does not decrypt", func(t *testing.T) {
		tampered := append([]byte(nil), raw...)
		tampered[len(tampered)-1] ^= 0x01
		if err := os.WriteFile(path, tampered, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, _, err := store.Load(meta.ID); err == nil || !strings.Contains(err.Error(), "authentication") {
			t.Fatalf("Load of a tampered snapshot = %v, want an authentication failure", err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatalf("restore: %v", err)
		}
	})

	t.Run("a snapshot renamed onto another id does not decrypt", func(t *testing.T) {
		// The id is the AEAD's additional data, so copying one snapshot's bytes
		// over another id must fail rather than restore the wrong session.
		other, err := store.Save(fixtureSnapshot(), SaveOptions{})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		otherPath := filepath.Join(store.Root(), other.ID+fileSuffix)
		if err := os.WriteFile(otherPath, raw, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, _, err := store.Load(other.ID); err == nil || !strings.Contains(err.Error(), "authentication") {
			t.Fatalf("Load of a relabelled snapshot = %v, want an authentication failure", err)
		}
	})
}

func TestSnapshotExpiresAndIsDeleted(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store, err := NewStore(Config{Root: filepath.Join(t.TempDir(), "state"), Key: []byte(fixtureKey), TTL: time.Hour, Now: clock})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	meta, err := store.Save(fixtureSnapshot(), SaveOptions{})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := meta.ExpiresAt.Sub(meta.CreatedAt); got != time.Hour {
		t.Fatalf("ttl = %s, want 1h", got)
	}
	now = now.Add(59 * time.Minute)
	if _, _, err := store.Load(meta.ID); err != nil {
		t.Fatalf("Load before expiry: %v", err)
	}
	now = now.Add(2 * time.Minute)
	if _, _, err := store.Load(meta.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("Load after expiry = %v, want ErrExpired", err)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), meta.ID+fileSuffix)); !os.IsNotExist(err) {
		t.Fatal("an expired snapshot must be removed from disk, not merely refused")
	}
}

func TestPerSaveTTLOnlyShortens(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store, err := NewStore(Config{
		Root: filepath.Join(t.TempDir(), "state"), Key: []byte(fixtureKey), TTL: time.Hour,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	for _, tc := range []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{"shorter is honoured", 10 * time.Minute, 10 * time.Minute},
		{"longer is capped to the store default", 48 * time.Hour, time.Hour},
		{"zero uses the store default", 0, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta, err := store.Save(fixtureSnapshot(), SaveOptions{TTL: tc.ttl})
			if err != nil {
				t.Fatalf("Save: %v", err)
			}
			if got := meta.ExpiresAt.Sub(meta.CreatedAt); got != tc.want {
				t.Fatalf("retention = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestLoadKeyRefusesAKeyInsideTheStateRoot(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "key")
	if err := os.WriteFile(inside, []byte(fixtureKey), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if _, err := LoadKey(inside, root); err == nil || !strings.Contains(err.Error(), "inside the session state root") {
		t.Fatalf("LoadKey = %v, want a refusal for a key beside the ciphertext", err)
	}

	outside := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(outside, []byte(fixtureKey+"\n"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	key, err := LoadKey(outside, root)
	if err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
	if string(key) != fixtureKey {
		t.Fatalf("key = %q, want the file's contents without the trailing newline", key)
	}

	loose := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(loose, []byte(fixtureKey), 0o644); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if _, err := LoadKey(loose, root); err == nil || !strings.Contains(err.Error(), "beyond its owner") {
		t.Fatalf("LoadKey on a 0644 key = %v, want a permissions refusal", err)
	}
}
