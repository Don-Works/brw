package browser

import (
	"strings"
	"testing"
)

func TestNormalizeInitScript(t *testing.T) {
	tests := []struct {
		name    string
		opts    InitScriptOptions
		wantAct string
		wantErr bool
	}{
		{name: "action defaults to list", opts: InitScriptOptions{}, wantAct: "list"},
		{name: "add needs source", opts: InitScriptOptions{Action: "add", Source: "window.__x=1"}, wantAct: "add"},
		{name: "add without source", opts: InitScriptOptions{Action: "add"}, wantErr: true},
		{name: "add with only whitespace", opts: InitScriptOptions{Action: "add", Source: "  "}, wantErr: true},
		{name: "remove needs id", opts: InitScriptOptions{Action: "remove", ID: "abc"}, wantAct: "remove"},
		{name: "remove without id", opts: InitScriptOptions{Action: "remove"}, wantErr: true},
		{name: "clear needs nothing", opts: InitScriptOptions{Action: "clear"}, wantAct: "clear"},
		{name: "case is normalized", opts: InitScriptOptions{Action: "ADD", Source: "1"}, wantAct: "add"},
		{name: "unknown action", opts: InitScriptOptions{Action: "delete"}, wantErr: true},
		{name: "oversized source", opts: InitScriptOptions{Action: "add", Source: strings.Repeat("x", maxInitScriptBytes+1)}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeInitScript(tt.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NormalizeInitScript(%+v) error = %v, wantErr %v", tt.opts, err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got.Action != tt.wantAct {
				t.Fatalf("action = %q, want %q", got.Action, tt.wantAct)
			}
		})
	}
}

func TestInitScriptPreviewIsBounded(t *testing.T) {
	script := NewInitScript("s1", "window.__x = 1;\n// a comment\nwindow.__y = 2;")
	if script.ID != "s1" || script.Bytes != len("window.__x = 1;\n// a comment\nwindow.__y = 2;") {
		t.Fatalf("script = %+v", script)
	}
	if strings.Contains(script.Preview, "\n") {
		t.Fatalf("preview should be one line, got %q", script.Preview)
	}
	long := NewInitScript("s2", strings.Repeat("a", initScriptPreviewBytes+50))
	if len([]rune(long.Preview)) > initScriptPreviewBytes+1 {
		t.Fatalf("preview not bounded: %q", long.Preview)
	}
	if !strings.HasSuffix(long.Preview, "…") {
		t.Fatalf("a truncated preview should say so: %q", long.Preview)
	}
}
