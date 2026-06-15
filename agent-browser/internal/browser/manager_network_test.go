package browser

import (
	"strings"
	"testing"
)

func TestGuardReplayRequestBlocksPurchaseLikeURLs(t *testing.T) {
	blocked := []struct {
		method string
		url    string
	}{
		{"POST", "https://shop.example.com/api/checkout"},
		{"POST", "https://shop.example.com/api/place-order"},
		{"POST", "https://pay.example.com/v1/pay-now"},
		{"GET", "https://example.com/complete-purchase?id=1"},
		{"POST", "https://example.com/submit-order"},
		{"POST", "https://example.com/confirm-order"},
		{"POST", "https://example.com/buy-now"},
	}
	for _, tc := range blocked {
		err := GuardReplayRequest(tc.method, tc.url)
		if err == nil {
			t.Fatalf("expected %s %s to be blocked, but it was allowed", tc.method, tc.url)
		}
		if !strings.Contains(err.Error(), "replay blocked") {
			t.Fatalf("error for %s does not mention block: %v", tc.url, err)
		}
	}
}

func TestGuardReplayRequestAllowsSafeReads(t *testing.T) {
	safe := []struct {
		method string
		url    string
	}{
		{"GET", "https://api.example.com/v1/products?q=shoe"},
		{"GET", "https://api.example.com/v1/stock/12345"},
		{"GET", "https://example.com/search?term=order-history"}, // "order" alone is not a purchase verb here
		{"POST", "https://api.example.com/v1/cart/add"},
	}
	for _, tc := range safe {
		if err := GuardReplayRequest(tc.method, tc.url); err != nil {
			t.Fatalf("expected %s %s to be allowed, got error: %v", tc.method, tc.url, err)
		}
	}
}
