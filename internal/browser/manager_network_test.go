package browser

import "testing"

func TestBlockedReplayReason(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		url     string
		blocked bool
	}{
		// Idempotent reads are always allowed — the documented safe use.
		{"GET checkout allowed", "GET", "https://shop.example/checkout/summary", false},
		{"HEAD payment allowed", "HEAD", "https://shop.example/payment/status", false},
		{"empty method defaults GET", "", "https://shop.example/order/123", false},
		{"GET plain api allowed", "GET", "https://api.example/v1/products", false},

		// Mutating methods to transactional URLs are blocked.
		{"POST checkout blocked", "POST", "https://shop.example/checkout/complete", true},
		{"POST payment blocked", "post", "https://pay.example/api/payment", true},
		{"PUT place-order blocked", "PUT", "https://shop.example/place-order", true},
		{"DELETE subscribe blocked", "DELETE", "https://x.example/subscribe", true},
		{"PATCH charge blocked", "PATCH", "https://billing.example/charge/now", true},

		// Mutating methods to non-transactional URLs are allowed.
		{"POST search allowed", "POST", "https://api.example/v1/search", false},
		{"POST login allowed", "POST", "https://api.example/auth/login", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := ReplayRequestParams{Method: tc.method, URL: tc.url}
			reason := p.BlockedReplayReason()
			if tc.blocked && reason == "" {
				t.Fatalf("expected %s %s to be blocked, but it was allowed", tc.method, tc.url)
			}
			if !tc.blocked && reason != "" {
				t.Fatalf("expected %s %s to be allowed, but it was blocked: %s", tc.method, tc.url, reason)
			}
		})
	}
}
