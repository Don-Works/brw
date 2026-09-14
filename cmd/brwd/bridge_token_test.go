package main

import (
	"testing"

	"github.com/Don-Works/brw/internal/extensionbridge"
)

// TestBridgeRequireTokenDefaults pins the posture an unconfigured install runs
// with. Before this, the token was optional unless an operator set an env var
// nothing in setup sets, so every default install accepted a tokenless hello —
// and the Origin check that gets a caller that far is forgeable by any local
// process.
func TestBridgeRequireTokenDefaults(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want bool
	}{
		{"unset requires the token", "", true},
		{"explicit opt-out allows tokenless", "1", false},
		{"true opts out", "true", false},
		{"unrecognised value still requires", "maybe", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BRW_BRIDGE_ALLOW_TOKENLESS", tc.env)
			if got := bridgeRequireToken(); got != tc.want {
				t.Fatalf("bridgeRequireToken() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBridgeConstructorRequiresToken proves the safe default lives in the
// library, so a caller that never calls SetRequireToken is still strict.
func TestBridgeConstructorRequiresToken(t *testing.T) {
	b := extensionbridge.New("", 0, "")
	if !b.RequireToken() {
		t.Fatal("a freshly constructed bridge must require a handshake token")
	}
}
