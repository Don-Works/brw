package navpolicy

import (
	"strings"
	"testing"
)

// A javascript: "navigation" executes script in the CURRENT page's origin
// instead of navigating. Chrome then reports the navigation as aborted, so
// before this was refused, brw_navigate_to ran arbitrary script on whatever
// signed-in page was open and told the caller it had failed.
func TestNormalizeNavigationURLRefusesScriptSchemes(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"javascript", "javascript:alert(1)"},
		{"javascript assigning a global", "javascript:window.x=1"},
		{"uppercase scheme", "JaVaScRiPt:alert(1)"},
		{"leading and trailing whitespace", "   javascript:alert(1)   "},
		// The normaliser strips these before parsing, so a scheme split across
		// them must not slip through.
		{"embedded tab", "java\tscript:alert(1)"},
		{"embedded newline", "java\nscript:alert(1)"},
		{"embedded carriage return", "java\rscript:alert(1)"},
		{"vbscript", "vbscript:msgbox(1)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			normalized, err := NormalizeNavigationURL(tt.url)
			if err == nil {
				t.Fatalf("NormalizeNavigationURL(%q) = %q, want a refusal", tt.url, normalized)
			}
			if !strings.Contains(err.Error(), "brw_evaluate") {
				t.Errorf("the refusal should point at the tool that does run JavaScript; got %v", err)
			}
		})
	}
}

// The refusal has to hold with NO policy configured, which is the default, and
// on the navigation path both transports share.
func TestScriptSchemeRefusedWithoutAPolicy(t *testing.T) {
	var empty *Policy
	if !empty.Empty() {
		t.Fatal("a nil policy should report itself as empty")
	}
	if _, err := empty.CheckNavigation("javascript:alert(1)"); err == nil {
		t.Fatal("a javascript: navigation must be refused even with no policy configured")
	}
	// And ordinary navigation must be unaffected.
	normalized, err := empty.CheckNavigation("example.com/path")
	if err != nil {
		t.Fatalf("an ordinary navigation should still pass: %v", err)
	}
	if normalized != "https://example.com/path" {
		t.Fatalf("normalized = %q, want the https-defaulted URL", normalized)
	}
}

// Schemes that merely LOOK adjacent must not be caught by the prefix check.
func TestNormalizeNavigationURLAllowsLookalikeSchemes(t *testing.T) {
	for _, raw := range []string{
		"https://example.com/javascript:not-a-scheme",
		"https://example.com/?next=javascript:alert(1)",
		"https://javascript.example.com/",
	} {
		if _, err := NormalizeNavigationURL(raw); err != nil {
			t.Errorf("NormalizeNavigationURL(%q) should pass, got %v", raw, err)
		}
	}
}
