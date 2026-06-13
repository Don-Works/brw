package actions

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

type KeyDescriptor struct {
	Key                   string
	Code                  string
	Text                  string
	WindowsVirtualKeyCode int64
	Modifiers             int64
}

func DescribeKey(raw string) KeyDescriptor {
	parts := strings.Split(raw, "+")
	if len(parts) > 1 {
		var modifiers int64
		for _, part := range parts[:len(parts)-1] {
			switch strings.ToLower(strings.TrimSpace(part)) {
			case "alt", "option":
				modifiers |= 1
			case "ctrl", "control":
				modifiers |= 2
			case "meta", "cmd", "command":
				modifiers |= 4
			case "shift":
				modifiers |= 8
			}
		}
		desc := DescribeKey(parts[len(parts)-1])
		desc.Modifiers = modifiers
		if modifiers != 0 {
			desc.Text = ""
		}
		return desc
	}

	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "enter", "return":
		return KeyDescriptor{Key: "Enter", Code: "Enter", Text: "\r", WindowsVirtualKeyCode: 13}
	case "tab":
		return KeyDescriptor{Key: "Tab", Code: "Tab", WindowsVirtualKeyCode: 9}
	case "escape", "esc":
		return KeyDescriptor{Key: "Escape", Code: "Escape", WindowsVirtualKeyCode: 27}
	case "backspace":
		return KeyDescriptor{Key: "Backspace", Code: "Backspace", WindowsVirtualKeyCode: 8}
	case "delete":
		return KeyDescriptor{Key: "Delete", Code: "Delete", WindowsVirtualKeyCode: 46}
	case "space", " ":
		return KeyDescriptor{Key: " ", Code: "Space", Text: " ", WindowsVirtualKeyCode: 32}
	case "arrowup":
		return KeyDescriptor{Key: "ArrowUp", Code: "ArrowUp", WindowsVirtualKeyCode: 38}
	case "arrowdown":
		return KeyDescriptor{Key: "ArrowDown", Code: "ArrowDown", WindowsVirtualKeyCode: 40}
	case "arrowleft":
		return KeyDescriptor{Key: "ArrowLeft", Code: "ArrowLeft", WindowsVirtualKeyCode: 37}
	case "arrowright":
		return KeyDescriptor{Key: "ArrowRight", Code: "ArrowRight", WindowsVirtualKeyCode: 39}
	}

	raw = strings.TrimSpace(raw)
	if raw == "" {
		return KeyDescriptor{}
	}
	r, size := utf8.DecodeRuneInString(raw)
	if r != utf8.RuneError && size == len(raw) {
		code := raw
		vk := int64(unicode.ToUpper(r))
		if unicode.IsLetter(r) {
			code = "Key" + string(unicode.ToUpper(r))
		} else if unicode.IsDigit(r) {
			code = "Digit" + string(r)
		}
		return KeyDescriptor{Key: raw, Code: code, Text: raw, WindowsVirtualKeyCode: vk}
	}
	return KeyDescriptor{Key: raw, Code: raw}
}
