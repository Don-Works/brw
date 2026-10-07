package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestCompletionScriptsCoverTheVerbTable(t *testing.T) {
	scripts := map[string]string{"bash": BashCompletion(), "zsh": ZshCompletion()}
	for shell, script := range scripts {
		t.Run(shell, func(t *testing.T) {

			code := withoutComments(script)
			for _, word := range topLevelWords(verbs()) {
				if !mentionsWord(code, word) {
					t.Errorf("%s completion does not mention %q outside its comments", shell, word)
				}
			}
			for first, subs := range subWords(verbs()) {
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

			cmd := exec.CommandContext(context.Background(), zsh, "-f", "-c", script, zsh, path, filepath.Dir(path))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("zsh: %v\n%s", err, out)
			}
			checkCompletions(t, tt, lines(string(out)))
		})
	}
}

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

			wantAll: topLevelWords(verbs()),
		},
		{
			name:    "a two-word verb",
			words:   []string{"artifact", ""},
			current: 3,
			wantAll: subWords(verbs())["artifact"],
		},
		{
			name:    "flags for a verb",
			words:   []string{"open", "--"},
			current: 3,
			want:    []string{"--group", "--json", "--timeout"},
		},
		{
			name: "the verb list after a global bool flag",

			words:   []string{"--json", ""},
			current: 3,
			wantAll: topLevelWords(verbs()),
		},
		{
			name:    "the verb list after a global flag and its value",
			words:   []string{"--profile", "work", ""},
			current: 4,
			wantAll: topLevelWords(verbs()),
		},
		{
			name:    "a two-word verb after a global flag",
			words:   []string{"--json", "artifact", ""},
			current: 4,
			wantAll: subWords(verbs())["artifact"],
		},
		{
			name:    "flags for a verb reached past a global flag",
			words:   []string{"--tab", "99", "open", "--"},
			current: 5,
			want:    []string{"--group", "--json"},
		},
		{

			name:    "flags for a built-in with its own flag set",
			words:   []string{"run", "--"},
			current: 3,
			wantAll: runCommandFlags(),
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
		if !slices.Contains(got, want) {
			t.Errorf("completions %v do not include %q", got, want)
		}
	}
	for _, unwanted := range tt.unwanted {
		if slices.Contains(got, unwanted) {
			t.Errorf("completions %v unexpectedly include %q", got, unwanted)
		}
	}
}

func TestBashCompletionFiltersOnThePrefix(t *testing.T) {
	bash := lookShell(t, "bash")
	path := writeScript(t, "brw.bash", BashCompletion())
	script := "source \"$1\"\nCOMP_WORDS=(brw sn)\nCOMP_CWORD=1\n_brw\nprintf '%s\\n' \"${COMPREPLY[@]}\"\n"
	got := lines(runShell(t, bash, script, path))
	if strings.Join(got, " ") != "snapshot" {
		t.Fatalf("completions for \"sn\" = %v, want only snapshot", got)
	}
}

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

func TestAMalformedVerbNameIsSkippedNotIndexed(t *testing.T) {
	table := []verb{
		{name: "", summary: "a malformed entry"},
		{name: "open", summary: "open a URL"},
		{name: "artifact read", summary: "read an artifact"},
	}
	tests := []struct {
		name string
		got  func() []string
	}{
		{name: "topLevelWords", got: func() []string { return topLevelWords(table) }},
		{name: "subWords", got: func() []string { return sortedKeys(subWords(table)) }},
		{name: "flagsByFirstWord", got: func() []string { return sortedKeys(flagsByFirstWord(table)) }},
		{name: "bashCompletion", got: func() []string { return []string{bashCompletion(table)} }},
		{name: "zshCompletion", got: func() []string { return []string{zshCompletion(table)} }},
		{name: "groupSummary", got: func() []string { return []string{groupSummary(table, "artifact")} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, word := range tt.got() {
				if strings.TrimSpace(word) == "" {
					t.Fatalf("%s produced an empty word from the blank entry", tt.name)
				}
			}
		})
	}

	if v, _, ok := lookupVerb(table, []string{"no-such-verb"}); ok {
		t.Fatalf("lookupVerb dispatched %q for an unknown verb", v.name)
	}
	v, rest, ok := lookupVerb(table, []string{"artifact", "read", "art_1"})
	if !ok || v.name != "artifact read" || len(rest) != 1 || rest[0] != "art_1" {
		t.Fatalf("lookupVerb = %q %v %v, want the two-word verb and its argument", v.name, rest, ok)
	}
}
