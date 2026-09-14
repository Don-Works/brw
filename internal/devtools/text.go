package devtools

import "unicode/utf8"

// boundedText cuts text to at most limit bytes on a rune boundary. A caption
// drawn into a page and a failure summary copied into a summary are both
// caller- or engine-supplied text that is frequently not ASCII, and a raw byte
// slice splits a multi-byte rune into a replacement character.
func boundedText(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
