package actions

import "testing"

// A held modifier has to describe as a real key. Before modifierKey existed,
// "shift" fell through to the single-rune fallback and produced key "shift",
// virtual key code 0 and text "shift" — a page listening for keydown saw a
// nonsense key and getModifierState never became true.
func TestDescribeKeyResolvesStandaloneModifiers(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		key       string
		code      string
		vk        int64
		modifiers int64
		text      string
	}{
		{name: "shift", raw: "shift", key: "Shift", code: "ShiftLeft", vk: 16, modifiers: ModifierShift},
		{name: "uppercase shift", raw: "Shift", key: "Shift", code: "ShiftLeft", vk: 16, modifiers: ModifierShift},
		{name: "right shift keeps the side in code", raw: "ShiftRight", key: "Shift", code: "ShiftRight", vk: 16, modifiers: ModifierShift},
		{name: "ctrl alias", raw: "ctrl", key: "Control", code: "ControlLeft", vk: 17, modifiers: ModifierCtrl},
		{name: "control", raw: "Control", key: "Control", code: "ControlLeft", vk: 17, modifiers: ModifierCtrl},
		{name: "alt", raw: "alt", key: "Alt", code: "AltLeft", vk: 18, modifiers: ModifierAlt},
		{name: "option alias", raw: "option", key: "Alt", code: "AltLeft", vk: 18, modifiers: ModifierAlt},
		{name: "meta", raw: "meta", key: "Meta", code: "MetaLeft", vk: 91, modifiers: ModifierMeta},
		{name: "cmd alias", raw: "cmd", key: "Meta", code: "MetaLeft", vk: 91, modifiers: ModifierMeta},
		// A chord's own modifier bit survives alongside the prefix bits.
		{name: "chord of two modifiers", raw: "ctrl+shift", key: "Shift", code: "ShiftLeft", vk: 16, modifiers: ModifierCtrl | ModifierShift},
		// Ordinary keys are untouched by the modifier table.
		{name: "letter", raw: "a", key: "a", code: "KeyA", vk: 65, text: "a"},
		{name: "named key", raw: "Enter", key: "Enter", code: "Enter", vk: 13, text: "\r"},
		{name: "chord over a letter", raw: "shift+a", key: "a", code: "KeyA", vk: 65, modifiers: ModifierShift},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DescribeKey(tt.raw)
			if got.Key != tt.key || got.Code != tt.code || got.WindowsVirtualKeyCode != tt.vk {
				t.Fatalf("DescribeKey(%q) = {key:%q code:%q vk:%d}, want {key:%q code:%q vk:%d}",
					tt.raw, got.Key, got.Code, got.WindowsVirtualKeyCode, tt.key, tt.code, tt.vk)
			}
			if got.Modifiers != tt.modifiers {
				t.Fatalf("DescribeKey(%q).Modifiers = %d, want %d", tt.raw, got.Modifiers, tt.modifiers)
			}
			if got.Text != tt.text {
				t.Fatalf("DescribeKey(%q).Text = %q, want %q", tt.raw, got.Text, tt.text)
			}
		})
	}
}
