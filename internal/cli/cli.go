// Package cli is brw's per-action command surface: one short-lived process that issues one request to a running brwd and prints the answer.
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
	"os/user"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/usagelog"
)

// Version is stamped at link time (-X github.com/Don-Works/brw/internal/cli.Version), the same way the MCP server's version is.
var Version = "dev"

// Exit codes.
const (
	ExitOK           = 0
	ExitActionFailed = 1
	ExitUsage        = 2
	ExitNoDaemon     = 3
)

const (
	defaultTimeout = 30 * time.Second

	clientHeadroom = 10 * time.Second
)

var errNoDaemon = errors.New("no brw daemon reachable")

type builtinCommand struct {
	name    string
	summary string

	flags func() []string
}

var builtinCommandTable = []builtinCommand{
	{name: "completion", summary: "print the shell completion script"},
	{name: "help", summary: "print the verb list"},
	{name: "run", summary: "run one recipe non-interactively for a scheduler", flags: runCommandFlags},
	{name: "version", summary: "print the brw version"},
	{name: "usage", summary: "review local context usage and latency logs", flags: usageCommandFlags},
}

func builtinCommands() []string {
	names := make([]string, 0, len(builtinCommandTable))
	for _, command := range builtinCommandTable {
		names = append(names, command.name)
	}
	return names
}

var builtinFlagWords = map[string]bool{"h": true, "help": true, "version": true}

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

	settleMS     int
	readSettleMS *int
	tags         string
	rule         string
	color        string
	label        string
	durationMS   int
	clear        bool
	scroll       bool
	scope        string

	clickCount int
	button     string
	level      string
	pattern    string
	onlyErrors bool
	width      int
	height     int
	kind       string
	filter     string
	origin     string
	domain     string
	cookiePath string
	secure     bool
	httpOnly   bool
	sameSite   string
	timezone   string
}

// Run executes one brw invocation and returns its process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return ExitUsage
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
	switch rest[0] {
	case "help", "-h", "--help":
		usage(stdout)
		return ExitOK
	case "version", "--version":
		fmt.Fprintln(stdout, Version)
		return ExitOK
	case "completion":
		return runCompletion(rest[1:], stdout, stderr)
	case "usage":
		return usageCommand(ctx, append(leading, rest[1:]...), stdout, stderr)
	case "run":

		return runCommand(ctx, append(leading, rest[1:]...), stdout, stderr)
	}

	v, verbArgs, ok := lookupVerb(verbs(), rest)
	if !ok {
		fmt.Fprintf(stderr, "brw: unknown command %q\n\n", rest[0])
		usage(stderr)
		return ExitUsage
	}
	return runVerb(ctx, v, append(leading, verbArgs...), stdout, stderr)
}

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

func lookupVerb(all []verb, args []string) (verb, []string, bool) {
	var matched verb
	longest := 0
	for _, v := range all {
		tokens := strings.Fields(v.name)
		if len(tokens) > longest && len(args) >= len(tokens) && slices.Equal(tokens, args[:len(tokens)]) {
			matched, longest = v, len(tokens)
		}
	}
	if longest == 0 {
		return verb{}, nil, false
	}
	return matched, args[longest:], true
}

func runVerb(ctx context.Context, v verb, args []string, stdout, stderr io.Writer) (exitCode int) {
	started := time.Now()
	output := &cliUsageWriter{Writer: stdout}
	stdout = output
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

		fmt.Fprintf(stderr, "brw %s: --tab does not apply to this verb\n", v.name)
		return ExitUsage
	}

	baseURL, err := resolveBaseURL(opts)
	if err != nil {
		fmt.Fprintf(stderr, "brw: %v\n", err)
		return ExitNoDaemon
	}

	deadline := opts.timeout
	if v.serverTimeout {
		deadline += clientHeadroom
	}
	ctrl, err := httpclient.New(baseURL, deadline)
	if err != nil {
		fmt.Fprintf(stderr, "brw: %v\n", err)
		return ExitNoDaemon
	}
	ctrl.UseOwnerUnlessSet(cliOwner())
	ctx = usagelog.WithRequestID(ctx, usagelog.NewID())
	var usageErr error
	defer func() { recordCLIUsage(ctx, ctrl, v, args, output, started, exitCode, opts.json, usageErr) }()

	if opts.tab != "" {
		ctx = browser.WithTabID(ctx, opts.tab)
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	body, err := v.call(ctx, ctrl, req)
	if err != nil {
		usageErr = err
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

	var rendered string
	if opts.json {
		if _, err := stdout.Write(append([]byte(strings.TrimRight(string(body), "\n")), '\n')); err != nil {
			usageErr = err
			fmt.Fprintf(stderr, "brw %s: %v\n", v.name, err)
			return ExitActionFailed
		}
	} else {
		var out bytes.Buffer
		if err := v.render(&out, opts, body); err != nil {
			usageErr = err
			fmt.Fprintf(stderr, "brw %s: %v\n", v.name, err)
			return ExitActionFailed
		}
		rendered = out.String()
		if _, err := stdout.Write(out.Bytes()); err != nil {
			usageErr = err
			fmt.Fprintf(stderr, "brw %s: %v\n", v.name, err)
			return ExitActionFailed
		}
	}

	if reason, failed := actionFailed(v, body); failed {
		usageErr = errors.New(reason)
		if reason != "" && !strings.Contains(rendered, reason) {
			fmt.Fprintf(stderr, "brw %s: %s\n", v.name, reason)
		}
		return ExitActionFailed
	}
	return ExitOK
}

func (v verb) call(ctx context.Context, ctrl *httpclient.Controller, req request) (json.RawMessage, error) {
	if v.exactBody {
		return ctrl.RequestExact(ctx, v.method, v.path, req.Query, req.Body)
	}
	return ctrl.Request(ctx, v.method, v.path, req.Query, req.Body)
}

func actionFailed(v verb, body []byte) (string, bool) {
	if message, failed := envelopeRefused(body); failed {
		return message, true
	}
	if v.unsupported == nil {
		return "", false
	}
	return v.unsupported(body)
}

func unreachable(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return false
	}

	return !urlErr.Timeout()
}

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
straight into the next command: every brw run by one OS user shares one
working tab. Set BRW_OWNER_ID to give a script or agent its own tab.

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
  run <recipe-id>            run one recipe non-interactively for a scheduler
                             (JSON on stdout; see docs/scheduling.md)
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

brw run adds 4 (a postcondition did not hold), 5 (site permissions refused) and
6 (another run holds this profile). See brw run --help.
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

type request struct {
	Query url.Values
	Body  any
}

func cliOwner() string {
	name := strconv.Itoa(os.Getuid())
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	return "brw-cli:" + name
}
