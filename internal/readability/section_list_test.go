package readability

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSectionListDecodesArrayAndCommaString(t *testing.T) {
	cases := []struct {
		name string
		args string
		want []string
	}{
		{"array", `{"include":["headings","links"]}`, []string{"headings", "links"}},
		{"comma string", `{"include":"headings,links"}`, []string{"headings", "links"}},
		{"comma string with spaces", `{"include":"headings, links"}`, []string{"headings", " links"}},
		{"single string", `{"include":"main"}`, []string{"main"}},
		{"absent", `{}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var opts ReadOptions
			if err := json.Unmarshal([]byte(tc.args), &opts); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if err := opts.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			if strings.Join(opts.Include, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("include = %q, want %q", opts.Include, tc.want)
			}
			if len(tc.want) > 0 && !opts.wants(strings.TrimSpace(tc.want[len(tc.want)-1])) {
				t.Fatalf("wants(%q) = false after decoding a list that names it", tc.want[len(tc.want)-1])
			}
		})
	}
	var opts ReadOptions
	if err := json.Unmarshal([]byte(`{"include":42}`), &opts); err == nil {
		t.Fatal("a numeric include decoded without error")
	}
	if err := json.Unmarshal([]byte(`{"include":"headings,bogus"}`), &opts); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := opts.Validate(); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("Validate() = %v, want an error naming the unknown section", err)
	}
}
