package actions

import (
	"testing"
)

func TestDescribeKey_Enter(t *testing.T) {
	d := DescribeKey("Enter")
	if d.Key != "Enter" || d.Code != "Enter" || d.Text != "\r" {
		t.Fatalf("unexpected Enter descriptor: %+v", d)
	}
}

func TestDescribeKey_Tab(t *testing.T) {
	d := DescribeKey("Tab")
	if d.Key != "Tab" || d.Code != "Tab" {
		t.Fatalf("unexpected Tab descriptor: %+v", d)
	}
}

func TestDescribeKey_Escape(t *testing.T) {
	d := DescribeKey("Escape")
	if d.Key != "Escape" || d.WindowsVirtualKeyCode != 27 {
		t.Fatalf("unexpected Escape descriptor: %+v", d)
	}
}

func TestDescribeKey_ArrowDown(t *testing.T) {
	d := DescribeKey("ArrowDown")
	if d.Key != "ArrowDown" || d.WindowsVirtualKeyCode != 40 {
		t.Fatalf("unexpected ArrowDown descriptor: %+v", d)
	}
}

func TestDescribeKey_Space(t *testing.T) {
	d := DescribeKey("Space")
	if d.Key != " " || d.Code != "Space" || d.Text != " " {
		t.Fatalf("unexpected Space descriptor: %+v", d)
	}
}

func TestDescribeKey_SingleChar(t *testing.T) {
	d := DescribeKey("a")
	if d.Key != "a" || d.Code != "KeyA" || d.Text != "a" {
		t.Fatalf("unexpected 'a' descriptor: %+v", d)
	}
}

func TestDescribeKey_Digit(t *testing.T) {
	d := DescribeKey("5")
	if d.Key != "5" || d.Code != "Digit5" || d.Text != "5" {
		t.Fatalf("unexpected '5' descriptor: %+v", d)
	}
}

func TestDescribeKey_Empty(t *testing.T) {
	d := DescribeKey("")
	if d.Key != "" {
		t.Fatalf("expected empty descriptor for empty input, got %+v", d)
	}
}

func TestDescribeKey_CtrlA(t *testing.T) {
	d := DescribeKey("Ctrl+a")
	if d.Key != "a" || d.Modifiers != 2 {
		t.Fatalf("unexpected Ctrl+a descriptor: %+v", d)
	}
	if d.Text != "" {
		t.Fatalf("expected empty text for modified key, got %q", d.Text)
	}
}

func TestDescribeKey_MetaShiftS(t *testing.T) {
	d := DescribeKey("Meta+Shift+s")
	if d.Key != "s" || d.Modifiers != 12 { // 4 (meta) + 8 (shift)
		t.Fatalf("unexpected Meta+Shift+s descriptor: %+v", d)
	}
}

func TestDescribeKey_AltOption(t *testing.T) {
	d := DescribeKey("Alt+Enter")
	if d.Key != "Enter" || d.Modifiers != 1 {
		t.Fatalf("unexpected Alt+Enter descriptor: %+v", d)
	}
	d2 := DescribeKey("Option+Enter")
	if d2.Key != "Enter" || d2.Modifiers != 1 {
		t.Fatalf("unexpected Option+Enter descriptor: %+v", d2)
	}
}

func TestDescribeKey_CaseInsensitive(t *testing.T) {
	d1 := DescribeKey("ENTER")
	d2 := DescribeKey("enter")
	if d1.Key != d2.Key || d1.Code != d2.Code {
		t.Fatalf("expected case-insensitive match: %+v vs %+v", d1, d2)
	}
}

func TestDescribeKey_Delete(t *testing.T) {
	d := DescribeKey("Delete")
	if d.Key != "Delete" || d.WindowsVirtualKeyCode != 46 {
		t.Fatalf("unexpected Delete descriptor: %+v", d)
	}
}

func TestDescribeKey_Backspace(t *testing.T) {
	d := DescribeKey("Backspace")
	if d.Key != "Backspace" || d.WindowsVirtualKeyCode != 8 {
		t.Fatalf("unexpected Backspace descriptor: %+v", d)
	}
}

// Navigation/editing keys that previously fell to the raw-string fallback (VK 0),
// which silently no-ops on pages that read event.keyCode/which.
func TestDescribeKey_NavigationKeys(t *testing.T) {
	cases := []struct {
		in   string
		key  string
		code string
		vk   int64
	}{
		{"Home", "Home", "Home", 36},
		{"End", "End", "End", 35},
		{"PageUp", "PageUp", "PageUp", 33},
		{"PageDown", "PageDown", "PageDown", 34},
		{"Insert", "Insert", "Insert", 45},
	}
	for _, c := range cases {
		d := DescribeKey(c.in)
		if d.Key != c.key || d.Code != c.code || d.WindowsVirtualKeyCode != c.vk {
			t.Errorf("%s: got %+v, want key=%s code=%s vk=%d", c.in, d, c.key, c.code, c.vk)
		}
		// Case-insensitive, matching the other named keys.
		if lower := DescribeKey(toLowerASCII(c.in)); lower.Key != c.key {
			t.Errorf("%s must be case-insensitive, got %+v", c.in, lower)
		}
	}
}

func TestDescribeKey_FunctionKeys(t *testing.T) {
	for _, c := range []struct {
		in string
		vk int64
	}{
		{"F1", 112}, {"f1", 112}, {"F5", 116}, {"F12", 123}, {"F24", 135},
	} {
		d := DescribeKey(c.in)
		want := "F" + c.in[1:]
		if c.in == "f1" {
			want = "F1"
		}
		if d.Key != want || d.Code != want || d.WindowsVirtualKeyCode != c.vk {
			t.Errorf("%s: got %+v, want key=%s vk=%d", c.in, d, want, c.vk)
		}
	}
	// Out-of-range / malformed function-key names fall through to the char path,
	// not the function-key mapping.
	if d := DescribeKey("F25"); d.WindowsVirtualKeyCode == 136 {
		t.Errorf("F25 must not map as a function key, got %+v", d)
	}
	if d := DescribeKey("F0"); d.Key == "F0" && d.WindowsVirtualKeyCode >= 112 {
		t.Errorf("F0 must not map as a function key, got %+v", d)
	}
}

func toLowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
