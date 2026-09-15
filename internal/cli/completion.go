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
func topLevelWords(all []verb) []string {
	seen := map[string]bool{}
	var words []string
	for _, v := range all {
		tokens := strings.Fields(v.name)
		if len(tokens) == 0 {
			continue
		}
		first := tokens[0]
		if seen[first] {
			continue
		}
		seen[first] = true
		words = append(words, first)
	}
	words = append(words, builtinCommands()...)
	sort.Strings(words)
	return words
}

// globalFlagNames lists the flags every verb accepts. withValue is the subset
// that consumes the following word, which is what lets a completion script walk
// past `--profile work` to the verb behind it.
func globalFlagNames() (all, withValue []string) {
	fs := flag.NewFlagSet("brw", flag.ContinueOnError)
	registerGlobalFlags(fs, &options{})
	fs.VisitAll(func(f *flag.Flag) {
		all = append(all, "--"+f.Name)
		if !isBoolFlag(f) {
			withValue = append(withValue, "--"+f.Name)
		}
	})
	sort.Strings(all)
	sort.Strings(withValue)
	return all, withValue
}

// subWords maps a first token to its second tokens, for the verbs whose name is
// two words.
func subWords(all []verb) map[string][]string {
	subs := map[string][]string{}
	for _, v := range all {
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
func flagsByFirstWord(all []verb) map[string][]string {
	byWord := map[string][]string{}
	for _, v := range all {
		tokens := strings.Fields(v.name)
		if len(tokens) == 0 {
			continue
		}
		first := tokens[0]
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
func BashCompletion() string { return bashCompletion(verbs()) }

func bashCompletion(all []verb) string {
	globals, valueGlobals := globalFlagNames()
	var b strings.Builder
	b.WriteString(`# brw bash completion. Regenerate with: brw completion bash
_brw() {
    local cur verb verb_index flags
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"

    # A global flag may be typed before the verb, so the verb is the first word
    # that is neither a flag nor a flag's value — not always COMP_WORDS[1].
    verb_index=1
    while [ "$verb_index" -lt "${#COMP_WORDS[@]}" ]; do
        case "${COMP_WORDS[$verb_index]}" in
            --) verb_index=$((verb_index + 1)); break ;;
            -*=*) verb_index=$((verb_index + 1)) ;;
`)
	fmt.Fprintf(&b, "            %s) verb_index=$((verb_index + 2)) ;;\n", strings.Join(valueGlobals, "|"))
	b.WriteString(`            -*) verb_index=$((verb_index + 1)) ;;
            *) break ;;
        esac
    done
    verb="${COMP_WORDS[$verb_index]}"

    if [ "$COMP_CWORD" -eq "$verb_index" ]; then
        COMPREPLY=( $(compgen -W "`)
	b.WriteString(strings.Join(topLevelWords(all), " "))
	b.WriteString(`" -- "$cur") )
        return 0
    fi

    case "$verb" in
`)
	subs := subWords(all)
	for _, word := range sortedKeys(subs) {
		fmt.Fprintf(&b, "        %s)\n            if [ \"$COMP_CWORD\" -eq \"$((verb_index + 1))\" ]; then\n                COMPREPLY=( $(compgen -W \"%s\" -- \"$cur\") )\n                return 0\n            fi\n            ;;\n",
			word, strings.Join(subs[word], " "))
	}
	b.WriteString(`    esac

    case "$cur" in
        -*)
            case "$verb" in
`)
	byWord := flagsByFirstWord(all)
	for _, word := range sortedKeys(byWord) {
		fmt.Fprintf(&b, "                %s) flags=\"%s\" ;;\n", word, strings.Join(byWord[word], " "))
	}
	fmt.Fprintf(&b, "                *) flags=\"%s\" ;;\n", strings.Join(globals, " "))
	b.WriteString(`            esac
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
func ZshCompletion() string { return zshCompletion(verbs()) }

func zshCompletion(all []verb) string {
	globals, valueGlobals := globalFlagNames()
	var b strings.Builder
	b.WriteString("#compdef brw\n# brw zsh completion. Regenerate with: brw completion zsh\n_brw() {\n    local -a _brw_verbs _brw_subs _brw_flags\n    local -i _brw_verb_index\n    _brw_verbs=(\n")
	// A word may be BOTH a verb of its own and the first token of a two-word
	// verb ("grants" and "grants revoke"). Emitting it from both loops offered
	// it to the shell twice, so the first word is recorded here and the group
	// loop skips what the verb loop already described.
	described := map[string]bool{}
	for _, v := range all {
		tokens := strings.Fields(v.name)
		if len(tokens) != 1 {
			continue
		}
		described[tokens[0]] = true
		fmt.Fprintf(&b, "        '%s:%s'\n", tokens[0], describeForZsh(v.summary))
	}
	builtin := map[string]bool{}
	for _, command := range builtinCommandTable {
		builtin[command.name] = true
	}
	for _, word := range sortedKeys(subWords(all)) {
		if builtin[word] || described[word] {
			continue
		}
		fmt.Fprintf(&b, "        '%s:%s'\n", word, describeForZsh(groupSummary(all, word)))
	}
	// Generated from the same table the dispatcher uses, so a built-in cannot
	// exist in one and not the other.
	for _, command := range builtinCommandTable {
		fmt.Fprintf(&b, "        '%s:%s'\n", command.name, describeForZsh(command.summary))
	}
	b.WriteString(`    )

    # A global flag may be typed before the verb, so the verb is the first word
    # that is neither a flag nor a flag's value — not always words[2].
    _brw_verb_index=2
    while (( _brw_verb_index <= ${#words} )); do
        case "${words[_brw_verb_index]}" in
            '--') (( _brw_verb_index++ )); break ;;
            -*=*) (( _brw_verb_index++ )) ;;
`)
	fmt.Fprintf(&b, "            %s) (( _brw_verb_index += 2 )) ;;\n", strings.Join(valueGlobals, "|"))
	b.WriteString(`            -*) (( _brw_verb_index++ )) ;;
            *) break ;;
        esac
    done

    if (( CURRENT == _brw_verb_index )); then
        _describe -t commands 'brw verb' _brw_verbs
        return
    fi

    case "${words[_brw_verb_index]}" in
`)
	subs := subWords(all)
	for _, word := range sortedKeys(subs) {
		fmt.Fprintf(&b, "        %s)\n            if (( CURRENT == _brw_verb_index + 1 )); then\n                _brw_subs=(%s)\n                compadd -- $_brw_subs\n                return\n            fi\n            ;;\n",
			word, strings.Join(subs[word], " "))
	}
	b.WriteString(`    esac

    case "${words[_brw_verb_index]}" in
`)
	byWord := flagsByFirstWord(all)
	for _, word := range sortedKeys(byWord) {
		fmt.Fprintf(&b, "        %s) _brw_flags=(%s) ;;\n", word, strings.Join(byWord[word], " "))
	}
	fmt.Fprintf(&b, "        *) _brw_flags=(%s) ;;\n", strings.Join(globals, " "))
	b.WriteString(`    esac

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
func groupSummary(all []verb, word string) string {
	var summaries []string
	for _, v := range all {
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
