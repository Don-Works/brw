package actions

import (
	"strings"
	"testing"
)

func TestDescribeKey(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want KeyDescriptor
	}{
		{"Enter", KeyDescriptor{Key: "Enter", Code: "Enter", Text: "\r", WindowsVirtualKeyCode: 13}},
		{"ENTER", KeyDescriptor{Key: "Enter", Code: "Enter", Text: "\r", WindowsVirtualKeyCode: 13}},
		{"enter", KeyDescriptor{Key: "Enter", Code: "Enter", Text: "\r", WindowsVirtualKeyCode: 13}},
		{"Tab", KeyDescriptor{Key: "Tab", Code: "Tab", WindowsVirtualKeyCode: 9}},
		{"Escape", KeyDescriptor{Key: "Escape", Code: "Escape", WindowsVirtualKeyCode: 27}},
		{"ArrowDown", KeyDescriptor{Key: "ArrowDown", Code: "ArrowDown", WindowsVirtualKeyCode: 40}},
		{"Space", KeyDescriptor{Key: " ", Code: "Space", Text: " ", WindowsVirtualKeyCode: 32}},
		{"a", KeyDescriptor{Key: "a", Code: "KeyA", Text: "a", WindowsVirtualKeyCode: 65}},
		{"5", KeyDescriptor{Key: "5", Code: "Digit5", Text: "5", WindowsVirtualKeyCode: 53}},
		{"", KeyDescriptor{}},
		{"Ctrl+a", KeyDescriptor{Key: "a", Code: "KeyA", WindowsVirtualKeyCode: 65, Modifiers: ModifierCtrl}},
		{"Meta+Shift+s", KeyDescriptor{Key: "s", Code: "KeyS", WindowsVirtualKeyCode: 83, Modifiers: ModifierMeta | ModifierShift}},
		{"Alt+Enter", KeyDescriptor{Key: "Enter", Code: "Enter", WindowsVirtualKeyCode: 13, Modifiers: ModifierAlt}},
		{"Option+Enter", KeyDescriptor{Key: "Enter", Code: "Enter", WindowsVirtualKeyCode: 13, Modifiers: ModifierAlt}},
		{"Delete", KeyDescriptor{Key: "Delete", Code: "Delete", WindowsVirtualKeyCode: 46}},
		{"Backspace", KeyDescriptor{Key: "Backspace", Code: "Backspace", WindowsVirtualKeyCode: 8}},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if got := DescribeKey(tc.raw); got != tc.want {
				t.Errorf("DescribeKey(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

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

		if lower := DescribeKey(strings.ToLower(c.in)); lower.Key != c.key {
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

	if d := DescribeKey("F25"); d.WindowsVirtualKeyCode == 136 {
		t.Errorf("F25 must not map as a function key, got %+v", d)
	}
	if d := DescribeKey("F0"); d.Key == "F0" && d.WindowsVirtualKeyCode >= 112 {
		t.Errorf("F0 must not map as a function key, got %+v", d)
	}
}

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

		{"ctrl+a", true},
		{"meta+s", true},
		{"Ctrl+C", true},
		{"ctrl+shift+a", true},
		{"alt+7", true},

		{"shift+a", false},
		{"shift+7", false},
		{"a", false},
		{"7", false},
		{"", false},
		{"ctrl+", false},

		{"hyper+a", false},
		{"ctrl+foo", false},
	}
	for _, test := range tests {
		if got := IsCommandKey(test.raw); got != test.want {
			t.Errorf("IsCommandKey(%q) = %v, want %v", test.raw, got, test.want)
		}
	}
}

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
