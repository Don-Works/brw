package browser

import "strings"
import "testing"

func TestWrapInitScriptGuardsOrigin(t *testing.T) {
	wrapped := WrapInitScript("window.__brw = 1;", "https://app.example.test")
	if !strings.Contains(wrapped, `"https://app.example.test"`) {
		t.Fatalf("wrap missing origin: %s", wrapped)
	}
	if !strings.Contains(wrapped, "window.__brw = 1;") {
		t.Fatalf("wrap missing source: %s", wrapped)
	}
	// A quote in the origin must not end the JS string.
	hostile := WrapInitScript("ok();", `https://evil.test") ; steal(); //`)
	if strings.Contains(hostile, `steal();`) && !strings.Contains(hostile, `\`) {
		t.Fatalf("origin was not encoded: %s", hostile)
	}
}

func TestNormalizeInitScriptBounds(t *testing.T) {
	if _, _, err := NormalizeInitScript(InitScriptOptions{}); err == nil {
		t.Fatal("empty source succeeded")
	}
	big := strings.Repeat("x", maxInitScriptBytes+1)
	if _, _, err := NormalizeInitScript(InitScriptOptions{Source: big}); err == nil {
		t.Fatal("oversize source succeeded")
	}
	_, origin, err := NormalizeInitScript(InitScriptOptions{Source: "ok();", Origin: "https://APP.example.test"})
	if err != nil || origin != "https://app.example.test" {
		t.Fatalf("origin = %q err=%v", origin, err)
	}
}
