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

// TestModifiersDecideTheInsertedCharacter covers both halves of what a modifier
// does to a keystroke: Shift changes which character the key inserts, and
// Ctrl/Alt/Meta stop it inserting one. Chrome types the event's text verbatim,
// so a descriptor that carries the mask but not the matching text drives the
// wrong character into the page.
func TestModifiersDecideTheInsertedCharacter(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		held      int64
		wantKey   string
		wantCode  string
		wantText  string
		wantMasks int64
	}{
		{name: "plain letter", key: "a", wantKey: "a", wantCode: "KeyA", wantText: "a"},
		{name: "shift chord on a letter", key: "shift+a", wantKey: "A", wantCode: "KeyA", wantText: "A", wantMasks: ModifierShift},
		{name: "held shift on a letter", key: "a", held: ModifierShift, wantKey: "A", wantCode: "KeyA", wantText: "A", wantMasks: ModifierShift},
		{name: "held shift on an already upper letter", key: "A", held: ModifierShift, wantKey: "A", wantCode: "KeyA", wantText: "A", wantMasks: ModifierShift},
		{name: "shift chord on a digit", key: "shift+1", wantKey: "!", wantCode: "Digit1", wantText: "!", wantMasks: ModifierShift},
		{name: "held shift on punctuation", key: "/", held: ModifierShift, wantKey: "?", wantCode: "/", wantText: "?", wantMasks: ModifierShift},
		// A named key has no character to shift, and its name must survive.
		{name: "shift chord on a named key", key: "shift+Enter", wantKey: "Enter", wantCode: "Enter", wantText: "\r", wantMasks: ModifierShift},
		{name: "held shift on Tab", key: "Tab", held: ModifierShift, wantKey: "Tab", wantCode: "Tab", wantMasks: ModifierShift},
		{name: "ctrl chord inserts nothing", key: "ctrl+a", wantKey: "a", wantCode: "KeyA", wantMasks: ModifierCtrl},
		{name: "held ctrl inserts nothing", key: "a", held: ModifierCtrl, wantKey: "a", wantCode: "KeyA", wantMasks: ModifierCtrl},
		{name: "ctrl and shift together insert nothing", key: "ctrl+shift+a", wantKey: "a", wantCode: "KeyA", wantMasks: ModifierCtrl | ModifierShift},
		{name: "meta chord inserts nothing", key: "meta+s", wantKey: "s", wantCode: "KeyS", wantMasks: ModifierMeta},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			desc := ApplyModifiers(DescribeKey(tt.key), tt.held)
			if desc.Key != tt.wantKey {
				t.Errorf("key = %q, want %q", desc.Key, tt.wantKey)
			}
			if desc.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", desc.Code, tt.wantCode)
			}
			if desc.Text != tt.wantText {
				t.Errorf("text = %q, want %q", desc.Text, tt.wantText)
			}
			if desc.Modifiers != tt.wantMasks {
				t.Errorf("modifiers = %d, want %d", desc.Modifiers, tt.wantMasks)
			}
		})
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

// IsCommandKey splits recorded keystrokes into commands, which a compiled
// recipe may carry, and characters, which are the data that was being typed.
func TestIsCommandKeySeparatesCommandsFromTypedCharacters(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{"Enter", true},
		{"tab", true},
		{"ArrowDown", true},
		{"f5", true},
		{"shift", true},
		{"ctrl+shift+Tab", true},
		// A Ctrl, Alt or Meta chord is an accelerator: Chrome attaches no text
		// to it, so nothing of what was typed survives into the recipe.
		{"ctrl+a", true},
		{"meta+s", true},
		{"Ctrl+C", true},
		{"ctrl+shift+a", true},
		{"alt+7", true},
		// Shift is the modifier that still types, so shift+a is the letter "A".
		{"shift+a", false},
		{"shift+7", false},
		{"a", false},
		{"7", false},
		{"", false},
		{"ctrl+", false},
		// Not a chord at all: "hyper" is no modifier brw dispatches, and "foo"
		// names no key, so neither reaches the accelerator rule.
		{"hyper+a", false},
		{"ctrl+foo", false},
	}
	for _, test := range tests {
		if got := IsCommandKey(test.raw); got != test.want {
			t.Errorf("IsCommandKey(%q) = %v, want %v", test.raw, got, test.want)
		}
	}
}

// chordModifiers is the domain the accelerator rule runs over, and whether
// "<modifier>+a" is a command or the letter "a" is decided one modifier at a
// time. Enumerating the map is what stops the next modifier added to it from
// inheriting a classification nobody made: ctrl was the reported case, meta and
// alt are its siblings, and shift is the one that goes the other way.
func TestEveryChordModifierIsClassifiedAsTypingOrNotTyping(t *testing.T) {
	suppressesInsertion := map[string]bool{
		"alt": true, "option": true,
		"ctrl": true, "control": true,
		"meta": true, "cmd": true, "command": true,
		"shift": false,
	}
	for name := range chordModifiers {
		suppresses, classified := suppressesInsertion[name]
		if !classified {
			t.Errorf("chord modifier %q is classified nowhere, so nothing decided whether %q+a is a command or the letter a", name, name)
			continue
		}
		chord := name + "+a"
		if got := DescribeKey(chord).Text; (got == "") != suppresses {
			t.Errorf("DescribeKey(%q).Text = %q, want inserted text %v", chord, got, !suppresses)
		}
		if got := IsCommandKey(chord); got != suppresses {
			t.Errorf("IsCommandKey(%q) = %v, want %v", chord, got, suppresses)
		}
	}
	for name := range suppressesInsertion {
		if _, ok := chordModifiers[name]; !ok {
			t.Errorf("%q is classified here but is not a chord modifier, so the classification covers nothing", name)
		}
	}
}
