package navpolicy

import (
	"strings"
	"testing"
)

func TestNormalizeNavigationURLRefusesScriptSchemes(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"javascript", "javascript:alert(1)"},
		{"javascript assigning a global", "javascript:window.x=1"},
		{"uppercase scheme", "JaVaScRiPt:alert(1)"},
		{"leading and trailing whitespace", "   javascript:alert(1)   "},

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

func TestScriptSchemeRefusedWithoutAPolicy(t *testing.T) {
	var empty *Policy
	if !empty.Empty() {
		t.Fatal("a nil policy should report itself as empty")
	}
	if _, err := empty.CheckNavigation("javascript:alert(1)"); err == nil {
		t.Fatal("a javascript: navigation must be refused even with no policy configured")
	}

	normalized, err := empty.CheckNavigation("example.com/path")
	if err != nil {
		t.Fatalf("an ordinary navigation should still pass: %v", err)
	}
	if normalized != "https://example.com/path" {
		t.Fatalf("normalized = %q, want the https-defaulted URL", normalized)
	}
}

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
