package actions

import "strings"

// Modifier bits as CDP Input.dispatchKeyEvent / Input.dispatchMouseEvent encode them.
const (
	ModifierAlt   int64 = 1
	ModifierCtrl  int64 = 2
	ModifierMeta  int64 = 4
	ModifierShift int64 = 8
)

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
