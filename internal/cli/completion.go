package cli

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Completion scripts are generated from the verb table rather than written by
// hand, so a verb or flag added below is completable the moment it exists.

func runCompletion(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: brw completion <bash|zsh>")
		return ExitUsage
	}
	switch args[0] {
	case "bash":
		fmt.Fprint(stdout, BashCompletion())
	case "zsh":
		fmt.Fprint(stdout, ZshCompletion())
	default:
		fmt.Fprintf(stderr, "brw completion: unknown shell %q (want bash or zsh)\n", args[0])
		return ExitUsage
	}
	return ExitOK
}

// topLevelWords is what a user can type as the first word: every verb's first
// token plus the built-ins that carry no route.
func topLevelWords() []string {
	seen := map[string]bool{}
	var words []string
	for _, v := range verbs() {
		first := strings.Fields(v.name)[0]
		if seen[first] {
			continue
		}
		seen[first] = true
		words = append(words, first)
	}
	words = append(words, "completion", "help", "version")
	sort.Strings(words)
	return words
}

// subWords maps a first token to its second tokens, for the verbs whose name is
// two words.
func subWords() map[string][]string {
	subs := map[string][]string{}
	for _, v := range verbs() {
		tokens := strings.Fields(v.name)
		if len(tokens) < 2 {
			continue
		}
		subs[tokens[0]] = append(subs[tokens[0]], tokens[1])
	}
	subs["completion"] = []string{"bash", "zsh"}
	for _, values := range subs {
		sort.Strings(values)
	}
	return subs
}

// verbFlags lists the flags one verb accepts, globals included, by registering
// them exactly as the dispatcher does.
func verbFlags(v verb) []string {
	fs := flag.NewFlagSet(v.name, flag.ContinueOnError)
	opts := &options{}
	registerGlobalFlags(fs, opts)
	if v.flags != nil {
		v.flags(fs, opts)
	}
	var names []string
	fs.VisitAll(func(f *flag.Flag) { names = append(names, "--"+f.Name) })
	sort.Strings(names)
	return names
}

// flagsByFirstWord collapses flags onto the first word of each verb name, which
// is what a shell has in hand when completing.
func flagsByFirstWord() map[string][]string {
	byWord := map[string][]string{}
	for _, v := range verbs() {
		first := strings.Fields(v.name)[0]
		seen := map[string]bool{}
		for _, name := range append(byWord[first], verbFlags(v)...) {
			seen[name] = true
		}
		merged := make([]string, 0, len(seen))
		for name := range seen {
			merged = append(merged, name)
		}
		sort.Strings(merged)
		byWord[first] = merged
	}
	return byWord
}

func sortedKeys(values map[string][]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// BashCompletion returns the bash completion script for brw.
func BashCompletion() string {
	var b strings.Builder
	b.WriteString(`# brw bash completion. Regenerate with: brw completion bash
_brw() {
    local cur words_1 flags
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"
    words_1="${COMP_WORDS[1]}"

    if [ "$COMP_CWORD" -eq 1 ]; then
        COMPREPLY=( $(compgen -W "`)
	b.WriteString(strings.Join(topLevelWords(), " "))
	b.WriteString(`" -- "$cur") )
        return 0
    fi

    case "$words_1" in
`)
	subs := subWords()
	for _, word := range sortedKeys(subs) {
		fmt.Fprintf(&b, "        %s)\n            if [ \"$COMP_CWORD\" -eq 2 ]; then\n                COMPREPLY=( $(compgen -W \"%s\" -- \"$cur\") )\n                return 0\n            fi\n            ;;\n",
			word, strings.Join(subs[word], " "))
	}
	b.WriteString(`    esac

    case "$cur" in
        -*)
            case "$words_1" in
`)
	byWord := flagsByFirstWord()
	for _, word := range sortedKeys(byWord) {
		fmt.Fprintf(&b, "                %s) flags=\"%s\" ;;\n", word, strings.Join(byWord[word], " "))
	}
	b.WriteString(`                *) flags="--json --daemon --profile --profile-policy --tab --timeout" ;;
            esac
            COMPREPLY=( $(compgen -W "$flags" -- "$cur") )
            return 0
            ;;
    esac
    return 0
}
complete -F _brw brw
`)
	return b.String()
}

// ZshCompletion returns the zsh completion script for brw. It works both when
// dropped into fpath as _brw and when sourced directly from a shell rc.
func ZshCompletion() string {
	var b strings.Builder
	b.WriteString("#compdef brw\n# brw zsh completion. Regenerate with: brw completion zsh\n_brw() {\n    local -a _brw_verbs _brw_subs _brw_flags\n    _brw_verbs=(\n")
	for _, v := range verbs() {
		tokens := strings.Fields(v.name)
		if len(tokens) > 1 {
			continue
		}
		fmt.Fprintf(&b, "        '%s:%s'\n", tokens[0], describeForZsh(v.summary))
	}
	for _, word := range sortedKeys(subWords()) {
		if word == "completion" {
			continue
		}
		fmt.Fprintf(&b, "        '%s:%s'\n", word, describeForZsh(groupSummary(word)))
	}
	b.WriteString("        'completion:print the shell completion script'\n")
	b.WriteString("        'help:print the verb list'\n")
	b.WriteString("        'version:print the brw version'\n")
	b.WriteString(`    )

    if (( CURRENT == 2 )); then
        _describe -t commands 'brw verb' _brw_verbs
        return
    fi

    case "${words[2]}" in
`)
	subs := subWords()
	for _, word := range sortedKeys(subs) {
		fmt.Fprintf(&b, "        %s)\n            if (( CURRENT == 3 )); then\n                _brw_subs=(%s)\n                compadd -- $_brw_subs\n                return\n            fi\n            ;;\n",
			word, strings.Join(subs[word], " "))
	}
	b.WriteString(`    esac

    case "${words[2]}" in
`)
	byWord := flagsByFirstWord()
	for _, word := range sortedKeys(byWord) {
		fmt.Fprintf(&b, "        %s) _brw_flags=(%s) ;;\n", word, strings.Join(byWord[word], " "))
	}
	b.WriteString(`        *) _brw_flags=(--json --daemon --profile --profile-policy --tab --timeout) ;;
    esac

    if [[ "${words[CURRENT]}" == -* ]]; then
        compadd -- $_brw_flags
        return
    fi
    _files
}

if [[ "$funcstack[1]" = "_brw" ]]; then
    _brw "$@"
else
    compdef _brw brw
fi
`)
	return b.String()
}

// groupSummary describes a first word that is shared by two-word verbs.
func groupSummary(word string) string {
	var summaries []string
	for _, v := range verbs() {
		tokens := strings.Fields(v.name)
		if len(tokens) > 1 && tokens[0] == word {
			summaries = append(summaries, tokens[1])
		}
	}
	sort.Strings(summaries)
	return word + " commands: " + strings.Join(summaries, ", ")
}

// describeForZsh strips the two characters zsh's completion-list syntax treats
// as structure, so a summary can be written for humans in the verb table.
func describeForZsh(summary string) string {
	summary = strings.ReplaceAll(summary, ":", " -")
	return strings.ReplaceAll(summary, "'", "")
}
