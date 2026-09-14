package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestCompletionScriptsCoverTheVerbTable(t *testing.T) {
	scripts := map[string]string{"bash": BashCompletion(), "zsh": ZshCompletion()}
	for shell, script := range scripts {
		t.Run(shell, func(t *testing.T) {
			// Comments are stripped first: a script gutted down to a no-op body
			// that still carries the verb list in a comment passed this check.
			code := withoutComments(script)
			for _, word := range topLevelWords() {
				if !mentionsWord(code, word) {
					t.Errorf("%s completion does not mention %q outside its comments", shell, word)
				}
			}
			for first, subs := range subWords() {
				for _, sub := range subs {
					if !mentionsWord(code, sub) {
						t.Errorf("%s completion does not mention %q under %q", shell, sub, first)
					}
				}
			}
			if !mentionsWord(code, "--group") {
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

	for _, tt := range completionCases() {
		t.Run(tt.name, func(t *testing.T) {
			var words []string
			for _, word := range tt.words {
				words = append(words, `"`+word+`"`)
			}
			script := "source \"$1\"\nCOMP_WORDS=(brw " + strings.Join(words, " ") + ")\n" +
				"COMP_CWORD=" + strconv.Itoa(tt.current-1) + "\n_brw\nprintf '%s\\n' \"${COMPREPLY[@]}\"\n"
			got := lines(runShell(t, bash, script, path))
			checkCompletions(t, tt, got)
		})
	}
}

// zsh's own completion system is loaded and the shipped script is sourced into
// it, then _brw is called with the words a shell would hand it. compadd and
// _describe are captured so the test sees the candidates the function actually
// produces: `zsh -n` alone passed against a _brw whose body had been deleted.
func TestZshCompletionCompletesInZsh(t *testing.T) {
	zsh := lookShell(t, "zsh")
	path := writeScript(t, "_brw", ZshCompletion())

	const preamble = `autoload -Uz compinit
compinit -u -d "$2/zcompdump"
source "$1"
_describe() { local n=${@[-1]}; local i; for i in ${(P)n}; do print -r -- "${i%%:*}"; done }
compadd() { local s=0 a; for a in "$@"; do [[ $a == -- ]] && { s=1; continue }; (( s )) && print -r -- "$a"; done }
_files() { : }
`

	for _, tt := range completionCases() {
		t.Run(tt.name, func(t *testing.T) {
			var words []string
			for _, word := range tt.words {
				words = append(words, `"`+word+`"`)
			}
			script := preamble + "words=(brw " + strings.Join(words, " ") + ")\nCURRENT=" + strconv.Itoa(tt.current) + "\n_brw\n"
			// -f keeps the developer's own zsh configuration out of the run.
			cmd := exec.CommandContext(context.Background(), zsh, "-f", "-c", script, zsh, path, filepath.Dir(path))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("zsh: %v\n%s", err, out)
			}
			checkCompletions(t, tt, lines(string(out)))
		})
	}
}

// completionCase is one shell completion request: the words already on the
// command line and which of them the cursor is on, 1-based as zsh counts.
type completionCase struct {
	name     string
	words    []string
	current  int
	want     []string
	wantAll  []string
	unwanted []string
}

func completionCases() []completionCase {
	return []completionCase{
		{
			name:    "the verb list",
			words:   []string{""},
			current: 2,
			// Every verb, not a sample: a script that offers a stale subset is
			// the failure this catches.
			wantAll: topLevelWords(),
		},
		{
			name:    "a two-word verb",
			words:   []string{"artifact", ""},
			current: 3,
			wantAll: subWords()["artifact"],
		},
		{
			name:    "flags for a verb",
			words:   []string{"open", "--"},
			current: 3,
			want:    []string{"--group", "--json", "--timeout"},
		},
		{
			name: "the verb list after a global bool flag",
			// brw accepts a global flag on either side of the verb, so
			// completion has to find the verb position rather than assume it.
			words:   []string{"--json", ""},
			current: 3,
			wantAll: topLevelWords(),
		},
		{
			name:    "the verb list after a global flag and its value",
			words:   []string{"--profile", "work", ""},
			current: 4,
			wantAll: topLevelWords(),
		},
		{
			name:    "a two-word verb after a global flag",
			words:   []string{"--json", "artifact", ""},
			current: 4,
			wantAll: subWords()["artifact"],
		},
		{
			name:    "flags for a verb reached past a global flag",
			words:   []string{"--tab", "99", "open", "--"},
			current: 5,
			want:    []string{"--group", "--json"},
		},
	}
}

func checkCompletions(t *testing.T, tt completionCase, got []string) {
	t.Helper()
	if tt.wantAll != nil {
		want := append([]string(nil), tt.wantAll...)
		sort.Strings(want)
		have := append([]string(nil), got...)
		sort.Strings(have)
		if strings.Join(have, " ") != strings.Join(want, " ") {
			t.Fatalf("completions = %v, want exactly %v", got, want)
		}
	}
	for _, want := range tt.want {
		if !contains(got, want) {
			t.Errorf("completions %v do not include %q", got, want)
		}
	}
	for _, unwanted := range tt.unwanted {
		if contains(got, unwanted) {
			t.Errorf("completions %v unexpectedly include %q", got, unwanted)
		}
	}
}

// compgen filters candidates on the typed prefix; the zsh completion system
// does the same for compadd. Only bash's filtering is observable here, because
// the zsh harness stubs compadd to capture what the function offers.
func TestBashCompletionFiltersOnThePrefix(t *testing.T) {
	bash := lookShell(t, "bash")
	path := writeScript(t, "brw.bash", BashCompletion())
	script := "source \"$1\"\nCOMP_WORDS=(brw sn)\nCOMP_CWORD=1\n_brw\nprintf '%s\\n' \"${COMPREPLY[@]}\"\n"
	got := lines(runShell(t, bash, script, path))
	if strings.Join(got, " ") != "snapshot" {
		t.Fatalf("completions for \"sn\" = %v, want only snapshot", got)
	}
}

// The script also has to be loadable the way it ships: dropped into fpath under
// its #compdef header, or sourced from a shell rc.
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

// A global flag typed before a built-in is the argument order every verb
// accepts, so the built-ins have to accept it too.
func TestGlobalFlagsMayPrecedeABuiltin(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantExit int
		wantOut  string
	}{
		{name: "completion", args: []string{"--json", "completion", "bash"}, wantExit: ExitOK, wantOut: "_brw"},
		{name: "help", args: []string{"--json", "--help"}, wantExit: ExitOK, wantOut: "usage: brw"},
		{name: "version", args: []string{"--profile", "work", "version"}, wantExit: ExitOK, wantOut: Version},
		{name: "help as a word", args: []string{"--json", "help"}, wantExit: ExitOK, wantOut: "usage: brw"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), tt.args, &stdout, &stderr); code != tt.wantExit {
				t.Fatalf("exit=%d, want %d (stderr=%q)", code, tt.wantExit, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantOut) {
				t.Fatalf("stdout = %q, want it to contain %q", stdout.String(), tt.wantOut)
			}
		})
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

func lines(out string) []string {
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	return kept
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func withoutComments(script string) string {
	var kept []string
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func mentionsWord(script, word string) bool {
	return regexp.MustCompile(`(^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(word) + `([^A-Za-z0-9_-]|$)`).MatchString(script)
}
