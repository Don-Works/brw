// Package cli is brw's per-action command surface: one short-lived process
// that issues one request to a running brwd and prints the answer. Every verb
// is a binding onto a route internal/http already serves, so the CLI can add no
// capability the HTTP API does not already have — and a verb without a route
// fails cli's route test rather than 404ing in someone's shell.
package cli

import (
	"bytes"
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
	// Headroom for the one verb that hands its timeout to the daemon: the client
	// deadline has to sit beyond the daemon's own so a wait that runs its full
	// timeout is answered rather than cut off here, which would report a
	// transport failure for a working daemon. Every other verb forwards no
	// timeout, so adding this to its deadline would only make --timeout a lie.
	clientHeadroom = 10 * time.Second
)

// errNoDaemon marks every failure where the action never reached a daemon.
var errNoDaemon = errors.New("no brw daemon reachable")

// builtinCommands carry no route: they print and exit. A verb may not start
// with one of these words or it would never be dispatched.
func builtinCommands() []string { return []string{"completion", "help", "version"} }

// builtinFlagWords are the built-ins people also spell with dashes, as in
// `brw --help`.
var builtinFlagWords = map[string]bool{"h": true, "help": true, "version": true}

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
	// Hoisting runs before the built-ins are matched, so a global flag typed to
	// the left of one (`brw --json completion bash`) is the same argument order
	// every verb already accepts rather than an unknown-command error.
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
	switch rest[0] {
	case "help", "-h", "--help":
		usage(stdout)
		return ExitOK
	case "version", "--version":
		fmt.Fprintln(stdout, Version)
		return ExitOK
	case "completion":
		return runCompletion(rest[1:], stdout, stderr)
	}

	v, verbArgs, ok := lookupVerb(verbs(), rest)
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
		if builtinFlagWords[name] {
			// help and version are commands spelled like flags, not globals.
			// Handing them back as the verb is what lets `brw --json --help`
			// print usage instead of dying on an unknown flag.
			return leading, args[i:], nil
		}
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
func lookupVerb(all []verb, args []string) (verb, []string, bool) {
	// Ordering is this lookup's own business; the caller's table keeps its shape.
	all = append([]verb(nil), all...)
	sort.SliceStable(all, func(i, j int) bool {
		return len(strings.Fields(all[i].name)) > len(strings.Fields(all[j].name))
	})
	for _, v := range all {
		tokens := strings.Fields(v.name)
		// A malformed entry with a blank name has no tokens, and matching zero
		// tokens would dispatch it for any argument list at all.
		if len(tokens) == 0 || len(args) < len(tokens) {
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

	if v.exactBody && opts.tab != "" {
		// The strict-schema routes are host-local and have no tab to act on, and
		// their handlers answer 400 to any field they do not declare. Saying so
		// beats sending a request that cannot work.
		fmt.Fprintf(stderr, "brw %s: --tab does not apply to this verb\n", v.name)
		return ExitUsage
	}

	baseURL, err := resolveBaseURL(opts)
	if err != nil {
		fmt.Fprintf(stderr, "brw: %v\n", err)
		return ExitNoDaemon
	}
	// --timeout is the whole wait for every verb that keeps its own deadline;
	// only a verb the daemon times out server-side needs the client to wait
	// longer than it asked for.
	deadline := opts.timeout
	if v.serverTimeout {
		deadline += clientHeadroom
	}
	ctrl, err := httpclient.New(baseURL, deadline)
	if err != nil {
		fmt.Fprintf(stderr, "brw: %v\n", err)
		return ExitNoDaemon
	}

	if opts.tab != "" {
		ctx = browser.WithTabID(ctx, opts.tab)
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	body, err := v.call(ctx, ctrl, req)
	if err != nil {
		if unreachable(ctx, err) {
			fmt.Fprintf(stderr, "brw: %v: %v\n", errNoDaemon, err)
			return ExitNoDaemon
		}
		err = actionError(ctx, deadline, err)
		if opts.json {
			writeJSONError(stdout, err)
		} else {
			fmt.Fprintf(stderr, "brw %s: %v\n", v.name, err)
		}
		return ExitActionFailed
	}

	// Human output is rendered into a buffer first so the failure path below can
	// see whether the verb already printed the daemon's reason, and not print it
	// a second time on stderr.
	var rendered string
	if opts.json {
		if _, err := stdout.Write(append([]byte(strings.TrimRight(string(body), "\n")), '\n')); err != nil {
			fmt.Fprintf(stderr, "brw %s: %v\n", v.name, err)
			return ExitActionFailed
		}
	} else {
		var out bytes.Buffer
		if err := v.render(&out, opts, body); err != nil {
			fmt.Fprintf(stderr, "brw %s: %v\n", v.name, err)
			return ExitActionFailed
		}
		rendered = out.String()
		if _, err := stdout.Write(out.Bytes()); err != nil {
			fmt.Fprintf(stderr, "brw %s: %v\n", v.name, err)
			return ExitActionFailed
		}
	}

	if reason, failed := actionFailed(v, body); failed {
		if reason != "" && !strings.Contains(rendered, reason) {
			fmt.Fprintf(stderr, "brw %s: %s\n", v.name, reason)
		}
		return ExitActionFailed
	}
	return ExitOK
}

// call issues the verb's request, keeping the strict-schema routes off the
// path that folds context values into the body.
func (v verb) call(ctx context.Context, ctrl *httpclient.Controller, req request) (json.RawMessage, error) {
	if v.exactBody {
		return ctrl.RequestExact(ctx, v.method, v.path, req.Query, req.Body)
	}
	return ctrl.Request(ctx, v.method, v.path, req.Query, req.Body)
}

// actionFailed reports the daemon's own reason for a 200 that is not a success:
// an ok:false envelope, or a capability the active transport does not have.
// Both answer 200, and a shell script only ever sees the exit code.
func actionFailed(v verb, body []byte) (string, bool) {
	if message, failed := envelopeRefused(body); failed {
		return message, true
	}
	if v.unsupported == nil {
		return "", false
	}
	return v.unsupported(body)
}

// unreachable reports whether an error means the request never got an answer
// from a daemon. httpclient turns every HTTP status into a plain error, so
// anything still carrying a *url.Error is a transport failure — except a
// cancelled or expired one: the daemon may be up and still working, and exit 3
// tells a script to go start one. cmd/brw wires SIGINT into this context, so
// Ctrl-C lands here too.
func unreachable(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return false
	}
	// http.Client's own deadline surfaces as a timeout on the *url.Error rather
	// than as a context error.
	return !urlErr.Timeout()
}

// actionError names why an action ended without an answer. The transport error
// says only that the connection went away, which reads as a broken daemon; the
// operator's Ctrl-C or their --timeout is the real reason.
func actionError(ctx context.Context, deadline time.Duration, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return errors.New("cancelled")
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("timed out after %s", deadline)
	default:
		return err
	}
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
	fs.StringVar(&opts.tab, "tab", "", "act on this tab id instead of the active tab (page verbs only)")
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
  --tab <id>                 act on this tab id instead of the active tab (page verbs only)
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
