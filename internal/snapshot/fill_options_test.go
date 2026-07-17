package snapshot

import "testing"

func TestFillOptionsEffectiveText(t *testing.T) {
	cases := []struct {
		name string
		opts FillOptions
		want string
	}{
		{"text wins", FillOptions{Text: "a", Value: "b"}, "a"},
		{"value alias when text empty", FillOptions{Value: "via_value"}, "via_value"},
		{"both empty", FillOptions{}, ""},
		{"explicit empty text with value still prefers empty? no — empty text falls through", FillOptions{Text: "", Value: "x"}, "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.opts.EffectiveText(); got != tc.want {
				t.Fatalf("EffectiveText()=%q want %q", got, tc.want)
			}
		})
	}
}
