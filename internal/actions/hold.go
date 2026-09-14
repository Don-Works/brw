package actions

import "strings"

// Modifier bits as CDP Input.dispatchKeyEvent / Input.dispatchMouseEvent encode
// them. Every dispatched event carries its own mask: the protocol keeps no
// keyboard state between calls, so a held modifier only exists for as long as
// the caller keeps stamping it onto each event.
const (
	ModifierAlt   int64 = 1
	ModifierCtrl  int64 = 2
	ModifierMeta  int64 = 4
	ModifierShift int64 = 8
)

// modifierKey maps a standalone modifier name to its descriptor, or returns nil
// when raw does not name one. Left/right variants are distinguished by code
// only, which is what a page reads from event.code.
//
// A modifier pressed on its own is a real key event, not just a mask: holding
// Shift for a click-range or Control for a drag means dispatching keyDown for
// "Shift" and withholding its keyUp. Without this table, "shift" falls through
// to DescribeKey's single-rune fallback, which produces key "shift", no virtual
// key code, and text "shift" — a page listening for keydown sees a nonsense key
// and getModifierState stays false.
//
// The descriptor carries its OWN modifier bit because a real Shift keydown
// reports shiftKey true; the matching keyUp clears it, which is why callers
// recompute the mask after releasing rather than reusing this value.
func modifierKey(raw string) *KeyDescriptor {
	name := strings.ToLower(strings.TrimSpace(raw))
	switch name {
	case "shift", "shiftleft", "shiftright":
		return &KeyDescriptor{Key: "Shift", Code: sideCode(name, "Shift"), WindowsVirtualKeyCode: 16, Modifiers: ModifierShift}
	case "control", "ctrl", "controlleft", "controlright", "ctrlleft", "ctrlright":
		return &KeyDescriptor{Key: "Control", Code: sideCode(name, "Control"), WindowsVirtualKeyCode: 17, Modifiers: ModifierCtrl}
	case "alt", "option", "altleft", "altright", "optionleft", "optionright":
		return &KeyDescriptor{Key: "Alt", Code: sideCode(name, "Alt"), WindowsVirtualKeyCode: 18, Modifiers: ModifierAlt}
	case "meta", "cmd", "command", "super", "os", "metaleft", "metaright", "cmdleft", "cmdright", "commandleft", "commandright":
		return &KeyDescriptor{Key: "Meta", Code: sideCode(name, "Meta"), WindowsVirtualKeyCode: 91, Modifiers: ModifierMeta}
	}
	return nil
}

func sideCode(name, base string) string {
	if strings.HasSuffix(name, "right") {
		return base + "Right"
	}
	return base + "Left"
}

// IsModifierKey reports whether raw names a modifier key rather than a
// character or navigation key.
func IsModifierKey(raw string) bool {
	return modifierKey(raw) != nil
}

// ModifierNames renders a CDP modifier mask as the key names that produced it,
// for reporting which keys a tab is still holding.
func ModifierNames(mask int64) []string {
	var names []string
	for _, m := range []struct {
		bit  int64
		name string
	}{
		{ModifierCtrl, "Control"},
		{ModifierAlt, "Alt"},
		{ModifierShift, "Shift"},
		{ModifierMeta, "Meta"},
	} {
		if mask&m.bit != 0 {
			names = append(names, m.name)
		}
	}
	return names
}
