// Package cli is brw's per-action command surface: one short-lived process
// that issues one request to a running brwd and prints the answer. Every verb
// is a binding onto a route internal/http already serves, so the CLI can add no
// capability the HTTP API does not already have — and a verb without a route
// fails cli's route test rather than 404ing in someone's shell.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/httpclient"
)

// Version is stamped at link time (-X github.com/Don-Works/brw/internal/cli.Version),
// the same way the MCP server's version is.
var Version = "dev"

// Exit codes. Scripting brw needs "the daemon is not there" told apart from
// "the daemon said no": the first is worth retrying after starting brwd, the
// second never is.
const (
	ExitOK           = 0
	ExitActionFailed = 1
	ExitUsage        = 2
	ExitNoDaemon     = 3
)

const (
	defaultTimeout = 30 * time.Second
	// The HTTP client deadline sits beyond the action deadline so a wait that
	// runs its full timeout is answered by the daemon rather than cut off by
	// this client, which would report a transport failure for a working daemon.
	clientHeadroom = 10 * time.Second
)

// errNoDaemon marks every failure where the action never reached a daemon.
var errNoDaemon = errors.New("no brw daemon reachable")

// options carries every flag any verb can take. One struct rather than one per
// verb keeps build/render free of type assertions; a verb only registers the
// flags it accepts, so `brw click --limit 3` is still rejected as unknown.
type options struct {
	json       bool
	daemon     string
	profile    string
	policyPath string
	tab        string
	timeout    time.Duration
	timeoutSet bool

	group        string
	role         string
	text         string
	mode         string
	section      string
	out          string
	limit        int
	maxChars     int
	maxBytes     int
	offset       int64
	viewportOnly bool
	appendText   bool
}

// Run executes one brw invocation and returns its process exit code. args
// excludes the program name.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return ExitUsage
	}
	switch args[0] {
	case "help", "-h", "--help":
		usage(stdout)
		return ExitOK
	case "version", "--version":
		fmt.Fprintln(stdout, Version)
		return ExitOK
	case "completion":
		return runCompletion(args[1:], stdout, stderr)
	}

	leading, rest, err := hoistGlobalFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "brw: %v\n\n", err)
		usage(stderr)
		return ExitUsage
	}
	if len(rest) == 0 {
		usage(stderr)
		return ExitUsage
	}
	v, verbArgs, ok := lookupVerb(rest)
	if !ok {
		fmt.Fprintf(stderr, "brw: unknown command %q\n\n", rest[0])
		usage(stderr)
		return ExitUsage
	}
	return runVerb(ctx, v, append(leading, verbArgs...), stdout, stderr)
}

// hoistGlobalFlags moves global flags typed before the verb to the verb's own
// flag set, so `brw --json tabs` and `brw tabs --json` mean the same thing. Each
// flag's value is consumed as a value, which is what keeps a profile or daemon
// named after a verb from being mistaken for one.
func hoistGlobalFlags(args []string) (leading, rest []string, err error) {
	globals := flag.NewFlagSet("brw", flag.ContinueOnError)
	globals.SetOutput(io.Discard)
	registerGlobalFlags(globals, &options{})

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			return leading, args[i:], nil
		}
		if arg == "--" {
			return leading, args[i+1:], nil
		}
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			leading = append(leading, arg)
			continue
		}
		def := globals.Lookup(name)
		if def == nil {
			return nil, nil, fmt.Errorf("unknown flag %s before the verb", arg)
		}
		leading = append(leading, arg)
		if !isBoolFlag(def) {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("flag %s needs a value", arg)
			}
			i++
			leading = append(leading, args[i])
		}
	}
	return leading, nil, nil
}

// lookupVerb matches the longest verb name against the leading arguments, so a
// two-word verb ("artifact read") wins over any one-word prefix of it.
func lookupVerb(args []string) (verb, []string, bool) {
	all := verbs()
	sort.SliceStable(all, func(i, j int) bool {
		return len(strings.Fields(all[i].name)) > len(strings.Fields(all[j].name))
	})
	for _, v := range all {
		tokens := strings.Fields(v.name)
		if len(args) < len(tokens) {
			continue
		}
		matched := true
		for i, token := range tokens {
			if args[i] != token {
				matched = false
				break
			}
		}
		if matched {
			return v, args[len(tokens):], true
		}
	}
	return verb{}, nil, false
}

func runVerb(ctx context.Context, v verb, args []string, stdout, stderr io.Writer) int {
	opts := &options{timeout: defaultTimeout}
	fs := flag.NewFlagSet("brw "+v.name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { verbUsage(stderr, v, fs) }
	registerGlobalFlags(fs, opts)
	if v.flags != nil {
		v.flags(fs, opts)
	}

	flagArgs, positional, err := splitFlags(fs, args)
	if err != nil {
		fmt.Fprintf(stderr, "brw %s: %v\n", v.name, err)
		verbUsage(stderr, v, fs)
		return ExitUsage
	}
	if err := fs.Parse(flagArgs); err != nil {
		return ExitUsage
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "timeout" {
			opts.timeoutSet = true
		}
	})

	req, err := v.build(opts, positional)
	if err != nil {
		fmt.Fprintf(stderr, "brw %s: %v\n", v.name, err)
		verbUsage(stderr, v, fs)
		return ExitUsage
	}

	baseURL, err := resolveBaseURL(opts)
	if err != nil {
		fmt.Fprintf(stderr, "brw: %v\n", err)
		return ExitNoDaemon
	}
	ctrl, err := httpclient.New(baseURL, opts.timeout+clientHeadroom)
	if err != nil {
		fmt.Fprintf(stderr, "brw: %v\n", err)
		return ExitNoDaemon
	}

	if opts.tab != "" {
		ctx = browser.WithTabID(ctx, opts.tab)
	}
	ctx, cancel := context.WithTimeout(ctx, opts.timeout+clientHeadroom)
	defer cancel()

	body, err := ctrl.Request(ctx, v.method, v.path, req.Query, req.Body)
	if err != nil {
		if unreachable(err) {
			fmt.Fprintf(stderr, "brw: %v: %v\n", errNoDaemon, err)
			return ExitNoDaemon
		}
		if opts.json {
			writeJSONError(stdout, err)
		} else {
			fmt.Fprintf(stderr, "brw %s: %v\n", v.name, err)
		}
		return ExitActionFailed
	}

	if opts.json {
		if _, err := stdout.Write(append([]byte(strings.TrimRight(string(body), "\n")), '\n')); err != nil {
			fmt.Fprintf(stderr, "brw %s: %v\n", v.name, err)
			return ExitActionFailed
		}
	} else if err := v.render(stdout, opts, body); err != nil {
		fmt.Fprintf(stderr, "brw %s: %v\n", v.name, err)
		return ExitActionFailed
	}

	// A refusal the daemon answered 200 to (ok:false) is still a failed action,
	// and a shell script only ever sees the exit code.
	if message, failed := envelopeRefused(body); failed {
		if message != "" && !opts.json {
			fmt.Fprintf(stderr, "brw %s: %s\n", v.name, message)
		}
		return ExitActionFailed
	}
	return ExitOK
}

// unreachable reports whether an error means the request never got an answer
// from a daemon. httpclient turns every HTTP status into a plain error, so
// anything still carrying a *url.Error is a transport failure.
func unreachable(err error) bool {
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

func writeJSONError(w io.Writer, err error) {
	encoded, merr := json.Marshal(map[string]string{"error": err.Error()})
	if merr != nil {
		return
	}
	fmt.Fprintf(w, "%s\n", encoded)
}

// envelopeRefused reads the ok/error fields shared by the daemon's action
// responses. Absent fields mean the response is not an action envelope (a
// snapshot, a page read), which is never a refusal.
func envelopeRefused(body []byte) (string, bool) {
	var envelope struct {
		OK      *bool  `json:"ok"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", false
	}
	if envelope.Error != "" {
		return envelope.Error, true
	}
	if envelope.OK != nil && !*envelope.OK {
		if envelope.Message != "" {
			return envelope.Message, true
		}
		return "the daemon reported the action did not succeed", true
	}
	return "", false
}

func registerGlobalFlags(fs *flag.FlagSet, opts *options) {
	fs.BoolVar(&opts.json, "json", false, "print the daemon's response envelope instead of human output")
	fs.StringVar(&opts.daemon, "daemon", "", "daemon base URL (default $BRW_URL, else the profile policy)")
	fs.StringVar(&opts.profile, "profile", os.Getenv("BRW_PROFILE"), "bridge profile name to act against")
	fs.StringVar(&opts.policyPath, "profile-policy", os.Getenv("BRW_PROFILE_POLICY"), "profile policy JSON path")
	fs.StringVar(&opts.tab, "tab", "", "act on this tab id instead of the active tab")
	fs.DurationVar(&opts.timeout, "timeout", defaultTimeout, "per-action timeout")
}

// splitFlags separates flags from positional arguments. Go's flag package stops
// at the first non-flag word, which would make `brw click @e17 --json` parse
// --json as a positional — the order people actually type.
func splitFlags(fs *flag.FlagSet, args []string) (flagArgs, positional []string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			positional = append(positional, args[i+1:]...)
			return flagArgs, positional, nil
		case arg == "-" || !strings.HasPrefix(arg, "-"):
			positional = append(positional, arg)
		default:
			name := strings.TrimLeft(arg, "-")
			if strings.Contains(name, "=") {
				flagArgs = append(flagArgs, arg)
				continue
			}
			if name == "help" || name == "h" {
				flagArgs = append(flagArgs, arg)
				continue
			}
			def := fs.Lookup(name)
			if def == nil {
				return nil, nil, fmt.Errorf("unknown flag %s", arg)
			}
			flagArgs = append(flagArgs, arg)
			if !isBoolFlag(def) {
				if i+1 >= len(args) {
					return nil, nil, fmt.Errorf("flag %s needs a value", arg)
				}
				i++
				flagArgs = append(flagArgs, args[i])
			}
		}
	}
	return flagArgs, positional, nil
}

func isBoolFlag(f *flag.Flag) bool {
	boolFlag, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && boolFlag.IsBoolFlag()
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: brw <verb> [flags] [arguments]

brw drives a running brwd over its HTTP API. Refs print as @e17 and paste
straight into the next command.

verbs:
`)
	for _, v := range verbs() {
		name := v.name
		if v.usage != "" {
			name += " " + v.usage
		}
		fmt.Fprintf(w, "  %-26s %s\n", name, v.summary)
	}
	fmt.Fprint(w, `
  completion <bash|zsh>      print the shell completion script
  version                    print the brw version

global flags:
  --json                     print the daemon's response envelope verbatim
  --daemon <url>             daemon base URL (default $BRW_URL, else the profile policy)
  --profile <name>           bridge profile to act against (default $BRW_PROFILE)
  --profile-policy <path>    profile policy JSON path (default $BRW_PROFILE_POLICY)
  --tab <id>                 act on this tab id instead of the active tab
  --timeout <duration>       per-action timeout (default 30s)

exit codes:
  0  the action succeeded
  1  the action failed
  2  usage error
  3  no brw daemon reachable
`)
}

func verbUsage(w io.Writer, v verb, fs *flag.FlagSet) {
	name := v.name
	if v.usage != "" {
		name += " " + v.usage
	}
	fmt.Fprintf(w, "usage: brw %s\n\n%s\n\nflags:\n", name, v.summary)
	fs.PrintDefaults()
}

// request is what a verb contributes to the call: the route itself is fixed on
// the verb, so a verb can only shape the query string and the JSON body.
type request struct {
	Query url.Values
	Body  any
}
