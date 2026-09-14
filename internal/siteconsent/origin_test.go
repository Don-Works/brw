package siteconsent

import "testing"

func TestCanonicalOrigin(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		err  bool
	}{
		{name: "https url keeps scheme and host", in: "https://Example.Test/some/path?q=1#frag", want: "https://example.test"},
		{name: "http default port is dropped", in: "http://example.test:80/x", want: "http://example.test"},
		{name: "https default port is dropped", in: "https://example.test:443/x", want: "https://example.test"},
		{name: "non-default port is kept", in: "http://example.test:8080/x", want: "http://example.test:8080"},
		{name: "bare host takes the https default", in: "example.test/x", want: "https://example.test"},
		{name: "protocol-relative pins https", in: "//example.test/x", want: "https://example.test"},
		{name: "unicode host normalises to punycode", in: "https://münchen.test/", want: "https://xn--mnchen-3ya.test"},
		{name: "punycode host is already canonical", in: "https://xn--mnchen-3ya.test/", want: "https://xn--mnchen-3ya.test"},
		{name: "backslash authority is read like Chrome reads it", in: "https://evil.test\\@good.test/", want: "https://evil.test"},
		{name: "embedded tab is stripped like Chrome strips it", in: "https://exa\tmple.test/", want: "https://example.test"},
		{name: "about:blank has no origin", in: "about:blank", want: ""},
		{name: "data url has no origin", in: "data:text/html,hi", want: ""},
		{name: "file url has no origin", in: "file:///etc/passwd", want: ""},
		{name: "javascript url has no origin", in: "javascript:alert(1)", want: ""},
		{name: "relative path has no origin", in: "/only/a/path", want: ""},
		{name: "empty has no origin", in: "   ", want: ""},
		{name: "http url with no host is an error", in: "https://", err: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := CanonicalOrigin(c.in)
			if c.err {
				if err == nil {
					t.Fatalf("CanonicalOrigin(%q) = %q, want an error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("CanonicalOrigin(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("CanonicalOrigin(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestUnicodeAndPunycodeAreOneGrant is the reason CanonicalOrigin runs IDNA: two
// spellings of one site must not become two records, one of which the user never
// saw and cannot find to revoke.
func TestUnicodeAndPunycodeAreOneGrant(t *testing.T) {
	guard := newTestGuard(t, AdminConfig{})
	if _, err := guard.Allow(GrantOptions{Origin: "https://münchen.test", Scope: ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	if err := guard.Authorize("https://xn--mnchen-3ya.test/page", ScopeAct); err != nil {
		t.Fatalf("the punycode spelling of a granted origin was refused: %v", err)
	}
	removed, err := guard.Revoke("https://xn--mnchen-3ya.test", "", "fixture-user")
	if err != nil || removed != 1 {
		t.Fatalf("revoking by the punycode spelling removed=%d err=%v", removed, err)
	}
}

func TestHostOfOrigin(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://example.test", "example.test"},
		{"https://example.test:8443", "example.test"},
		{"example.test", "example.test"},
		{"", ""},
	}
	for _, c := range cases {
		if got := HostOfOrigin(c.in); got != c.want {
			t.Errorf("HostOfOrigin(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
