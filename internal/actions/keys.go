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

// ApplyModifiers folds extra modifier bits into desc and re-derives the text the keystroke inserts.
func ApplyModifiers(desc KeyDescriptor, extra int64) KeyDescriptor {
	desc.Modifiers |= extra
	if desc.Text == "" {
		return desc
	}
	if desc.Modifiers&(ModifierAlt|ModifierCtrl|ModifierMeta) != 0 {
		desc.Text = ""
		return desc
	}
	if desc.Modifiers&ModifierShift == 0 {
		return desc
	}
	r, size := utf8.DecodeRuneInString(desc.Text)
	if size != len(desc.Text) {
		return desc
	}
	shifted := shiftRune(r)
	if shifted == r {
		return desc
	}

	if desc.Key == desc.Text {
		desc.Key = string(shifted)
	}
	desc.Text = string(shifted)
	return desc
}

var shiftedPunctuation = map[rune]rune{
	'1': '!', '2': '@', '3': '#', '4': '$', '5': '%',
	'6': '^', '7': '&', '8': '*', '9': '(', '0': ')',
	'-': '_', '=': '+', '[': '{', ']': '}', '\\': '|',
	';': ':', '\'': '"', ',': '<', '.': '>', '/': '?', '`': '~',
}

func shiftRune(r rune) rune {
	if unicode.IsLetter(r) {
		return unicode.ToUpper(r)
	}
	if shifted, ok := shiftedPunctuation[r]; ok {
		return shifted
	}
	return r
}

var chordModifiers = map[string]int64{
	"alt": ModifierAlt, "option": ModifierAlt,
	"ctrl": ModifierCtrl, "control": ModifierCtrl,
	"meta": ModifierMeta, "cmd": ModifierMeta, "command": ModifierMeta,
	"shift": ModifierShift,
}

func namedKey(raw string) *KeyDescriptor {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "enter", "return":
		return &KeyDescriptor{Key: "Enter", Code: "Enter", Text: "\r", WindowsVirtualKeyCode: 13}
	case "tab":
		return &KeyDescriptor{Key: "Tab", Code: "Tab", WindowsVirtualKeyCode: 9}
	case "escape", "esc":
		return &KeyDescriptor{Key: "Escape", Code: "Escape", WindowsVirtualKeyCode: 27}
	case "backspace":
		return &KeyDescriptor{Key: "Backspace", Code: "Backspace", WindowsVirtualKeyCode: 8}
	case "delete":
		return &KeyDescriptor{Key: "Delete", Code: "Delete", WindowsVirtualKeyCode: 46}
	case "space", " ":
		return &KeyDescriptor{Key: " ", Code: "Space", Text: " ", WindowsVirtualKeyCode: 32}
	case "arrowup":
		return &KeyDescriptor{Key: "ArrowUp", Code: "ArrowUp", WindowsVirtualKeyCode: 38}
	case "arrowdown":
		return &KeyDescriptor{Key: "ArrowDown", Code: "ArrowDown", WindowsVirtualKeyCode: 40}
	case "arrowleft":
		return &KeyDescriptor{Key: "ArrowLeft", Code: "ArrowLeft", WindowsVirtualKeyCode: 37}
	case "arrowright":
		return &KeyDescriptor{Key: "ArrowRight", Code: "ArrowRight", WindowsVirtualKeyCode: 39}
	case "home":
		return &KeyDescriptor{Key: "Home", Code: "Home", WindowsVirtualKeyCode: 36}
	case "end":
		return &KeyDescriptor{Key: "End", Code: "End", WindowsVirtualKeyCode: 35}
	case "pageup":
		return &KeyDescriptor{Key: "PageUp", Code: "PageUp", WindowsVirtualKeyCode: 33}
	case "pagedown":
		return &KeyDescriptor{Key: "PageDown", Code: "PageDown", WindowsVirtualKeyCode: 34}
	case "insert":
		return &KeyDescriptor{Key: "Insert", Code: "Insert", WindowsVirtualKeyCode: 45}
	}

	if mk := modifierKey(raw); mk != nil {
		return mk
	}

	return functionKey(raw)
}

// IsCommandKey reports whether raw names a keystroke that issues a command rather than entering a character.
func IsCommandKey(raw string) bool {
	parts := strings.Split(raw, "+")
	for _, part := range parts[:len(parts)-1] {
		if _, ok := chordModifiers[strings.ToLower(strings.TrimSpace(part))]; !ok {
			return false
		}
	}
	final := strings.TrimSpace(parts[len(parts)-1])
	if namedKey(final) != nil {
		return true
	}

	if r, size := utf8.DecodeRuneInString(final); r == utf8.RuneError || size != len(final) {
		return false
	}
	return DescribeKey(raw).Text == ""
}

func DescribeKey(raw string) KeyDescriptor {
	parts := strings.Split(raw, "+")
	if len(parts) > 1 {
		var modifiers int64
		for _, part := range parts[:len(parts)-1] {
			modifiers |= chordModifiers[strings.ToLower(strings.TrimSpace(part))]
		}

		return ApplyModifiers(DescribeKey(parts[len(parts)-1]), modifiers)
	}
	if named := namedKey(raw); named != nil {
		return *named
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
