package navpolicy

import "testing"

func TestCheckSubresource(t *testing.T) {
	tests := []struct {
		name    string
		policy  Policy
		url     string
		blocked bool
	}{
		{"empty policy permits everything", Policy{}, "https://tracker.example/px.gif", false},
		{"allowlist permits an allowed host", Policy{Allowed: []string{"example.com"}}, "https://example.com/app.js", false},
		{"allowlist permits a subdomain", Policy{Allowed: []string{"example.com"}}, "https://cdn.example.com/app.js", false},
		{"allowlist blocks an off-list host", Policy{Allowed: []string{"example.com"}}, "https://tracker.example/px.gif", true},
		{"allowlist blocks an off-list websocket", Policy{Allowed: []string{"example.com"}}, "wss://exfil.example/socket", true},
		{"blocklist blocks a listed host", Policy{Blocked: []string{"tracker.example"}}, "https://tracker.example/px.gif", true},
		{"blocklist permits an unlisted host", Policy{Blocked: []string{"tracker.example"}}, "https://example.com/app.js", false},
		{"blocked wins over allowed", Policy{Allowed: []string{"example.com"}, Blocked: []string{"ads.example.com"}}, "https://ads.example.com/px.gif", true},

		// Inline and same-document subresources carry no network destination.
		// Blocking them confines nothing and breaks ordinary pages.
		{"data: image passes under an allowlist", Policy{Allowed: []string{"example.com"}}, "data:image/png;base64,iVBORw0KGgo=", false},
		{"blob: worker script passes under an allowlist", Policy{Allowed: []string{"example.com"}}, "blob:https://example.com/9f8e", false},
		{"about:blank frame passes under an allowlist", Policy{Allowed: []string{"example.com"}}, "about:blank", false},
		{"relative reference passes under an allowlist", Policy{Allowed: []string{"example.com"}}, "/api/data.json", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.policy.CheckSubresource(tt.url)
			if tt.blocked && err == nil {
				t.Fatalf("CheckSubresource(%q) should be refused", tt.url)
			}
			if !tt.blocked && err != nil {
				t.Fatalf("CheckSubresource(%q) should pass, got %v", tt.url, err)
			}
		})
	}
}

// A data: NAVIGATION can execute attacker content as a document, so navigation
// keeps the stricter rule even though the subresource rule relaxes it.
func TestSubresourceRelaxationDoesNotWeakenNavigation(t *testing.T) {
	p := Policy{Allowed: []string{"example.com"}}
	if err := p.CheckSubresource("data:text/html,<h1>x"); err != nil {
		t.Fatalf("data: subresource should pass: %v", err)
	}
	if err := p.Check("data:text/html,<h1>x"); err == nil {
		t.Fatal("data: navigation must still be refused under an allowlist")
	}
}
