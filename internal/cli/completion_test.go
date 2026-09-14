package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompletionScriptsCoverTheVerbTable(t *testing.T) {
	scripts := map[string]string{"bash": BashCompletion(), "zsh": ZshCompletion()}
	for shell, script := range scripts {
		t.Run(shell, func(t *testing.T) {
			for _, word := range topLevelWords() {
				if !strings.Contains(script, word) {
					t.Errorf("%s completion does not mention %q", shell, word)
				}
			}
			for first, subs := range subWords() {
				for _, sub := range subs {
					if !strings.Contains(script, sub) {
						t.Errorf("%s completion does not mention %q under %q", shell, sub, first)
					}
				}
			}
			if !strings.Contains(script, "--group") {
				t.Errorf("%s completion does not offer a verb-specific flag", shell)
			}
		})
	}
}

// The bash script is run by bash, not merely inspected: completion that parses
// but returns nothing is worse than none at all.
func TestBashCompletionCompletesInBash(t *testing.T) {
	bash := lookShell(t, "bash")
	path := writeScript(t, "brw.bash", BashCompletion())

	tests := []struct {
		name     string
		words    string
		cword    string
		want     []string
		unwanted []string
	}{
		{
			name:  "the verb list",
			words: `COMP_WORDS=(brw "")`,
			cword: "1",
			want:  []string{"open", "click", "snapshot", "artifact", "completion"},
		},
		{
			name:  "a verb prefix",
			words: `COMP_WORDS=(brw sn)`,
			cword: "1",
			want:  []string{"snapshot"},
			// compgen filters on the prefix, so nothing else may come back.
			unwanted: []string{"open", "click"},
		},
		{
			name:  "a two-word verb",
			words: `COMP_WORDS=(brw artifact "")`,
			cword: "2",
			want:  []string{"read"},
		},
		{
			name:  "flags for a verb",
			words: `COMP_WORDS=(brw open --)`,
			cword: "2",
			want:  []string{"--group", "--json", "--timeout"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := "source \"$1\"\n" + tt.words + "\nCOMP_CWORD=" + tt.cword + "\n_brw\nprintf '%s\\n' \"${COMPREPLY[@]}\"\n"
			out := runShell(t, bash, script, path)
			for _, want := range tt.want {
				if !hasLine(out, want) {
					t.Errorf("completions %q do not include %q", out, want)
				}
			}
			for _, unwanted := range tt.unwanted {
				if hasLine(out, unwanted) {
					t.Errorf("completions %q unexpectedly include %q", out, unwanted)
				}
			}
		})
	}
}

// zsh's completion functions need zsh's completion system loaded to run, so the
// check here is that zsh itself accepts the script we ship.
func TestZshCompletionParsesInZsh(t *testing.T) {
	zsh := lookShell(t, "zsh")
	path := writeScript(t, "_brw", ZshCompletion())

	cmd := exec.CommandContext(context.Background(), zsh, "-n", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("zsh -n rejected the completion script: %v\n%s", err, out)
	}
	script := ZshCompletion()
	if !strings.HasPrefix(script, "#compdef brw\n") {
		t.Error("zsh completion must start with #compdef brw to be usable from fpath")
	}
	if !strings.Contains(script, "compdef _brw brw") {
		t.Error("zsh completion must register itself when sourced directly")
	}
}

func TestCompletionCommandWritesTheScript(t *testing.T) {
	for _, shell := range []string{"bash", "zsh"} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), []string{"completion", shell}, &stdout, &stderr); code != ExitOK {
			t.Fatalf("brw completion %s exit=%d (stderr=%q)", shell, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "_brw") {
			t.Fatalf("brw completion %s printed %q", shell, stdout.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"completion", "fish"}, &stdout, &stderr); code != ExitUsage {
		t.Fatalf("an unsupported shell exit=%d, want %d", code, ExitUsage)
	}
}

func lookShell(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not installed", name)
	}
	return path
}

func writeScript(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runShell(t *testing.T, shell, script, arg string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), shell, "-c", script, shell, arg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", shell, err, out)
	}
	return string(out)
}

func hasLine(out, want string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}
