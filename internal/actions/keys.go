package actions

import (
	"strconv"
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

// functionKey maps "f1".."f24" (case-insensitive) to its descriptor, or returns
// nil when raw is not a function-key name. VK_F1 is 0x70 and the codes run
// contiguously, so F<n> => 0x70 + (n-1).
func functionKey(raw string) *KeyDescriptor {
	s := strings.ToLower(strings.TrimSpace(raw))
	if len(s) < 2 || s[0] != 'f' {
		return nil
	}
	n, err := strconv.Atoi(s[1:])
	if err != nil || n < 1 || n > 24 {
		return nil
	}
	name := "F" + strconv.Itoa(n)
	return &KeyDescriptor{Key: name, Code: name, WindowsVirtualKeyCode: int64(0x70 + n - 1)}
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
	case "home":
		return KeyDescriptor{Key: "Home", Code: "Home", WindowsVirtualKeyCode: 36}
	case "end":
		return KeyDescriptor{Key: "End", Code: "End", WindowsVirtualKeyCode: 35}
	case "pageup":
		return KeyDescriptor{Key: "PageUp", Code: "PageUp", WindowsVirtualKeyCode: 33}
	case "pagedown":
		return KeyDescriptor{Key: "PageDown", Code: "PageDown", WindowsVirtualKeyCode: 34}
	case "insert":
		return KeyDescriptor{Key: "Insert", Code: "Insert", WindowsVirtualKeyCode: 45}
	}

	// Function keys F1–F24. event.key and code are both "F<n>", and the Windows
	// virtual-key codes are contiguous from VK_F1 = 0x70 (112). Handled here rather
	// than as 24 switch cases so a page listening on keyCode/which sees the right
	// value instead of the raw-string fallback (VK 0), which silently no-ops.
	if fk := functionKey(raw); fk != nil {
		return *fk
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
