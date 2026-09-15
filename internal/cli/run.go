package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/recipe"
	"github.com/Don-Works/brw/internal/runlock"
	"github.com/Don-Works/brw/internal/usagelog"
)

// `brw run` is the entry point a scheduler drives.
//
// brw ships no scheduler and does not intend to: launchd, systemd and cron
// already run things on a clock, survive reboots, and are what an operator's
// other jobs use. What brw owes them is a contract, and until now it had none —
// no documented invocation, no machine-readable result, no exit code that told
// a policy refusal apart from a broken daemon, and nothing stopping this run
// from landing on the tab the last one is still using.
//
// The contract is: one JSON object on stdout, human diagnostics on stderr, an
// exit code from the table below, and one run at a time per browser profile.

// runSchema versions the stdout contract. A scheduler parses this object, so a
// field that changes meaning has to change this string with it.
const runSchema = "brw.run/1"

// Exit codes beyond the ones every verb shares. A scheduler sees nothing but
// the exit code, so each of these has to mean one thing.
const (
	// ExitPostconditionFailed: brw ran the recipe and the state it asserted did
	// not hold. The machine, the daemon and the browser are all fine; the work
	// did not land. Running it again on the next tick is reasonable; paging
	// someone is not, until it repeats.
	ExitPostconditionFailed = 4
	// ExitPolicyRefused: brw refused. A missing site grant, a revoked one, a
	// blocked category, or an action that needs confirmation with nobody to
	// confirm it. Retrying changes nothing until a human grants something.
	ExitPolicyRefused = 5
	// ExitBusy: another run holds this profile. Nothing was attempted. The next
	// tick is the right time to try again.
	ExitBusy = 6
)

// runOutcome is one row of the exit-code contract.
type runOutcome struct {
	// Name is the outcome string in the JSON object.
	Name string
	// Code is the process exit code.
	Code int
	// Meaning is what the operator reads in docs/scheduling.md and in --help.
	Meaning string
	// Retry says whether running it again unchanged could succeed. It is in the
	// JSON so a scheduler wrapper does not have to hard-code the table.
	Retry bool
}

// runOutcomes is the whole contract. It is a table rather than a switch because
// the exit code is the entire interface a scheduler has: an outcome that is
// classified in one place and not another is a code an operator's wrapper reads
// as something else. TestEveryRunOutcomeIsDistinctAndDocumented enumerates it.
var runOutcomes = []runOutcome{
	{Name: "ok", Code: ExitOK, Meaning: "the recipe ran and every step reached its asserted state", Retry: false},
	{Name: "failed", Code: ExitActionFailed, Meaning: "the run failed for a reason that is none of the others; read error", Retry: false},
	{Name: "usage", Code: ExitUsage, Meaning: "the invocation is wrong; the scheduler's command line needs fixing", Retry: false},
	{Name: "infrastructure", Code: ExitNoDaemon, Meaning: "brw could not run it and nothing came back saying the recipe started: no daemon reachable, a transport failure, or a timeout with no result", Retry: true},
	{Name: "postcondition_failed", Code: ExitPostconditionFailed, Meaning: "the recipe ran and a step did not reach its asserted state, whatever class the daemon attached to the failure", Retry: true},
	{Name: "policy_refused", Code: ExitPolicyRefused, Meaning: "site permissions or the confirmation gate refused; a human has to grant something", Retry: false},
	{Name: "busy", Code: ExitBusy, Meaning: "another run holds this browser profile; nothing was attempted", Retry: true},
}

func outcome(name string) runOutcome {
	for _, row := range runOutcomes {
		if row.Name == name {
			return row
		}
	}
	// Unreachable through any call site in this file; a typo would otherwise
	// exit 0 on a failure, which is the one thing a scheduler must never see.
	return runOutcome{Name: "failed", Code: ExitActionFailed, Meaning: "unclassified"}
}

// runReport is the object on stdout. Everything a scheduler's wrapper needs is
// here, so it never has to parse a human sentence.
type runReport struct {
	Schema     string            `json:"schema"`
	OK         bool              `json:"ok"`
	Outcome    string            `json:"outcome"`
	ExitCode   int               `json:"exit_code"`
	Retryable  bool              `json:"retryable"`
	Daemon     string            `json:"daemon,omitempty"`
	Profile    runProfile        `json:"profile"`
	Recipe     runRecipeRef      `json:"recipe"`
	StartedAt  time.Time         `json:"started_at"`
	DurationMS int64             `json:"duration_ms"`
	LockWaitMS int64             `json:"lock_wait_ms"`
	Result     *recipe.RunResult `json:"result,omitempty"`
	Error      string            `json:"error,omitempty"`
	ErrorClass string            `json:"error_class,omitempty"`
	Diagnostic string            `json:"diagnostic,omitempty"`
}

type runProfile struct {
	Workspace string `json:"workspace,omitempty"`
	Profile   string `json:"profile,omitempty"`
	// LockKey is the profile identity the run serialised on. Two runs that
	// report the same lock key can never have overlapped.
	LockKey string `json:"lock_key,omitempty"`
	// LockShared says the daemon named no profile, so the key above is the one
	// every anonymous daemon shares rather than this browser's own. Runs through
	// such daemons still serialise against each other; they do not serialise
	// against an identified daemon driving the same browser.
	LockShared bool `json:"lock_shared,omitempty"`
}

type runRecipeRef struct {
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
	Digest  string `json:"digest,omitempty"`
}

type runOptions struct {
	daemon     string
	profile    string
	policyPath string
	version    string
	digest     string
	inputs     inputList
	lockWait   time.Duration
	timeout    time.Duration
}

// inputList collects repeated --input key=value flags.
type inputList map[string]string

func (l inputList) String() string {
	if len(l) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(l))
	for key := range l {
		pairs = append(pairs, key)
	}
	return strings.Join(pairs, ",")
}

func (l inputList) Set(value string) error {
	key, val, ok := strings.Cut(value, "=")
	key = strings.TrimSpace(key)
	if !ok || key == "" {
		return fmt.Errorf("--input takes key=value, got %q", value)
	}
	if _, duplicate := l[key]; duplicate {
		return fmt.Errorf("--input %s was given twice", key)
	}
	l[key] = val
	return nil
}

const runUsage = `usage: brw run <recipe-id> --recipe-version <v> --digest <sha256> [--input k=v]...

Run one recipe non-interactively and report the outcome as JSON on stdout.
Intended for launchd, systemd or cron; see docs/scheduling.md.

Only one run at a time touches a browser profile. A second run started while the
first is going waits for it (--lock-wait, default 5m) and then gives up with
exit 6 rather than interleaving with it on the same tab.

flags:
  --recipe-version <v>    the recipe version to run (required)
  --digest <sha256>       the recipe digest to run (required)
  --input key=value       a recipe input; repeatable
  --lock-wait <duration>  how long to wait for another run on this profile to
                          finish; 0 refuses immediately (default 5m)
  --timeout <duration>    how long to wait for the run itself (default 30m)
  --daemon <url>          daemon base URL (default $BRW_URL, else the profile policy)
  --profile <name>        bridge profile to act against (default $BRW_PROFILE)
  --profile-policy <path> profile policy JSON path

exit codes:`

// newRunFlagSet registers every flag `brw run` accepts.
//
// Split out because the completion scripts enumerate it. `brw run` takes none
// of the global flags — its output is a fixed JSON contract, so --json means
// nothing to it — and a shell that offered them would be offering words this
// FlagSet rejects with exit 2.
func newRunFlagSet(opts *runOptions) *flag.FlagSet {
	fs := flag.NewFlagSet("brw run", flag.ContinueOnError)
	fs.StringVar(&opts.daemon, "daemon", "", "daemon base URL")
	fs.StringVar(&opts.profile, "profile", os.Getenv("BRW_PROFILE"), "bridge profile to act against")
	fs.StringVar(&opts.policyPath, "profile-policy", os.Getenv("BRW_PROFILE_POLICY"), "profile policy JSON path")
	fs.StringVar(&opts.version, "recipe-version", "", "recipe version")
	fs.StringVar(&opts.digest, "digest", "", "recipe digest")
	fs.Var(opts.inputs, "input", "recipe input as key=value; repeatable")
	fs.DurationVar(&opts.lockWait, "lock-wait", opts.lockWait, "how long to wait for another run on this profile")
	fs.DurationVar(&opts.timeout, "timeout", opts.timeout, "how long to wait for the run itself")
	return fs
}

// runCommandFlags is the flag list the completion scripts emit for `brw run`.
func runCommandFlags() []string {
	opts := runOptions{inputs: inputList{}}
	var names []string
	newRunFlagSet(&opts).VisitAll(func(f *flag.Flag) { names = append(names, "--"+f.Name) })
	sort.Strings(names)
	return names
}

// runCommand is the whole non-interactive entry point. It returns the process
// exit code and writes exactly one JSON object to stdout.
func runCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts := runOptions{inputs: inputList{}, lockWait: 5 * time.Minute, timeout: recipe.DefaultMaxRunDuration}
	fs := newRunFlagSet(&opts)
	fs.SetOutput(stderr)
	fs.Usage = func() { runUsageText(stderr) }

	flagArgs, positional, err := splitFlags(fs, args)
	if err != nil {
		return failRun(stdout, stderr, "usage", runReport{}, err)
	}
	if err := fs.Parse(flagArgs); err != nil {
		return failRun(stdout, stderr, "usage", runReport{}, err)
	}
	if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
		runUsageText(stderr)
		return failRun(stdout, stderr, "usage", runReport{}, errors.New("run takes exactly one recipe id"))
	}
	request := recipe.RunRequest{
		ID:      strings.TrimSpace(positional[0]),
		Version: strings.TrimSpace(opts.version),
		Digest:  strings.TrimSpace(opts.digest),
	}
	if len(opts.inputs) > 0 {
		request.Inputs = map[string]string(opts.inputs)
	}
	report := runReport{Recipe: runRecipeRef{ID: request.ID, Version: request.Version, Digest: request.Digest}}
	// Version and digest are what pin the run to one immutable recipe. Leaving
	// either off would let a scheduled job silently start running a recipe
	// somebody republished, which is the failure the digest exists to prevent.
	if request.Version == "" || request.Digest == "" {
		runUsageText(stderr)
		return failRun(stdout, stderr, "usage", report, errors.New("run needs --recipe-version and --digest, so a scheduled job cannot silently start running a republished recipe"))
	}

	baseURL, err := resolveBaseURL(&options{daemon: opts.daemon, profile: opts.profile, policyPath: opts.policyPath})
	if err != nil {
		return failRun(stdout, stderr, "infrastructure", report, err)
	}
	report.Daemon = baseURL
	ctrl, err := httpclient.New(baseURL, opts.timeout+clientHeadroom)
	if err != nil {
		return failRun(stdout, stderr, "infrastructure", report, err)
	}

	healthCtx, cancelHealth := context.WithTimeout(ctx, defaultTimeout)
	health, err := ctrl.Health(healthCtx)
	cancelHealth()
	if err != nil {
		return failRun(stdout, stderr, "infrastructure", report, fmt.Errorf("%w: %v", errNoDaemon, err))
	}
	report.Profile = runProfile{Workspace: health.Identity.Workspace, Profile: health.Identity.Profile}

	// Fail closed on anything that would stop and ask, and on a daemon that will
	// not say whether it would. A daemon started with a prompter on its terminal
	// blocks on a read nobody is going to answer, so the run hangs to its timeout
	// and reports a timeout, which is not what happened; a daemon too old to
	// carry the consent block cannot be distinguished from one that answered
	// "no prompter", so it is refused by the same rule rather than trusted.
	//
	// The posture is the whole chain's. A proxy merges the posture of the daemon
	// it forwards to into its own before reporting it, so "unknown" here means
	// some hop could not be asked — which answers "could this hang" the same way
	// "yes" does.
	switch {
	case health.Consent == nil:
		return failRun(stdout, stderr, "policy_refused", report,
			errors.New("this daemon's /health does not report a consent posture, so brw cannot tell whether an un-granted origin would block it waiting for an answer nobody is there to give; upgrade the daemon to a build that reports it"))
	case health.Consent.Unknown:
		return failRun(stdout, stderr, "policy_refused", report,
			fmt.Errorf("this daemon forwards to another one whose consent posture brw could not read, so it cannot tell whether an un-granted origin would block the run waiting for an answer nobody is there to give: %s", health.Consent.Reason))
	case health.Consent.Interactive:
		return failRun(stdout, stderr, "policy_refused", report,
			errors.New("this daemon, or one it forwards to, was started with --site-consent-prompt, so an un-granted origin would block it waiting for an answer nobody is there to give; run the scheduled job against a daemon without the prompt and grant origins ahead of time with brwctl grants allow"))
	}

	// The lock is keyed on the profile the daemon names at /health. A daemon
	// that names none takes the shared "unidentified" key: that serialises it
	// against every other anonymous daemon, but NOT against an identified daemon
	// on the same Chrome, which takes the profile's own key. The gap is reported
	// rather than refused — a daemon started without --workspace/--profile is
	// the default install, and a run that cannot start at all is worse than one
	// that says which guarantee it has.
	lockKey := runlock.Key(health.Identity)
	report.Profile.LockShared = lockKey == runlock.Unidentified
	if report.Profile.LockShared {
		fmt.Fprintf(stderr, "brw run: this daemon does not name the browser profile it drives, so this run holds the shared lock every anonymous daemon holds; it is NOT serialised against an identified daemon on the same browser. Start the daemon with --workspace/--profile (brwctl setup does) for a lock keyed on the profile.\n")
	}

	report.Profile.LockKey = lockKey
	lockStarted := time.Now()
	// The lock directory is not an argument. A run pointed at a directory of its
	// own would serialise against nothing while reporting that it had.
	lock, err := runlock.Acquire(ctx, "", report.Profile.LockKey, opts.lockWait)
	report.LockWaitMS = time.Since(lockStarted).Milliseconds()
	switch {
	case errors.Is(err, runlock.ErrBusy):
		return failRun(stdout, stderr, "busy", report, err)
	case err != nil:
		return failRun(stdout, stderr, "infrastructure", report, err)
	}
	defer func() {
		if releaseErr := lock.Release(); releaseErr != nil {
			fmt.Fprintf(stderr, "brw run: release the profile lock: %v\n", releaseErr)
		}
	}()

	report.StartedAt = time.Now().UTC()
	runCtx, cancelRun := context.WithTimeout(ctx, opts.timeout)
	defer cancelRun()
	result, runErr := ctrl.RunRecipe(runCtx, request)
	report.DurationMS = time.Since(report.StartedAt).Milliseconds()
	// Only when a run actually happened. A refusal that never reached the runner
	// answers with a zero RunResult, and reporting that would put a started_at of
	// year 1 and an empty status into a scheduler's ledger as though a run had
	// been attempted.
	if result.Status != "" || len(result.Steps) > 0 {
		report.Result = &result
	}
	report.ErrorClass = httpclient.RemoteClass(runErr)

	return finishRun(stdout, stderr, report, classifyRun(runCtx, result, runErr), runErr)
}

// classifyRun maps a finished run onto the contract.
//
// Every branch reads a property, never a sentence: whether the call reached a
// daemon at all, the class the daemon itself attached to the refusal, and
// whether the structured result says a step failed. A message-matching
// classifier would silently reclassify the day somebody rewords an error.
func classifyRun(ctx context.Context, result recipe.RunResult, err error) runOutcome {
	if err == nil {
		return outcome("ok")
	}
	// The call never got an answer: no daemon, or the machine's network gave up.
	if unreachable(ctx, err) || errors.Is(err, errNoDaemon) {
		return outcome("infrastructure")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return outcome("infrastructure")
	}
	// A refusal is settled however far the run got: the consent gate can stop a
	// recipe part-way, and retrying it unchanged still cannot succeed until a
	// human grants something.
	class := httpclient.RemoteClass(err)
	if class == "policy_denied" {
		return outcome("policy_refused")
	}
	// The daemon answered with the run's own result: the recipe started, and a
	// step did not reach the state it asserted. That is a different thing from
	// brw being unable to run it, and a scheduler treats it differently.
	//
	// Asked BEFORE the daemon's error class, and that order is the contract. The
	// class names the innermost failure, and a postcondition that did not hold
	// is a wait that ran out — so the classes the compiler's default
	// postconditions produce are "timeout", which is retryable, which used to
	// classify a failed step as an infrastructure failure and exit 3. A
	// structured result carrying a failed step is positive evidence that the
	// recipe ran, and no error class can outrank it.
	if failedStep(result) {
		return outcome("postcondition_failed")
	}
	// Nothing ran, or the daemon returned nothing that says otherwise. The daemon
	// computes the class; whether it is worth another attempt is already decided
	// in one place, so ask that rather than re-listing them.
	if class != "" && usagelog.Retryable(class) {
		return outcome("infrastructure")
	}
	return outcome("failed")
}

// failedStep reports whether the run reached the browser and a step failed.
func failedStep(result recipe.RunResult) bool {
	if result.Status != "failed" {
		return false
	}
	for _, step := range result.Steps {
		if step.Status == "failed" {
			return true
		}
	}
	return false
}

func failRun(stdout, stderr io.Writer, name string, report runReport, err error) int {
	return finishRun(stdout, stderr, report, outcome(name), err)
}

func finishRun(stdout, stderr io.Writer, report runReport, decision runOutcome, err error) int {
	report.Schema = runSchema
	report.Outcome = decision.Name
	report.ExitCode = decision.Code
	report.Retryable = decision.Retry
	report.OK = decision.Code == ExitOK
	report.Diagnostic = decision.Meaning
	if err != nil {
		report.Error = err.Error()
	}
	if report.StartedAt.IsZero() {
		report.StartedAt = time.Now().UTC()
	}
	encoded, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		// stdout is the contract; if it cannot be written the run has not
		// reported at all, and an exit code alone would be read as a result.
		fmt.Fprintf(stderr, "brw run: encode the run report: %v\n", marshalErr)
		return ExitActionFailed
	}
	fmt.Fprintf(stdout, "%s\n", encoded)

	// stderr is for the human reading the scheduler's log.
	if err != nil {
		fmt.Fprintf(stderr, "brw run %s: %s (exit %d): %v\n", report.Recipe.ID, decision.Name, decision.Code, err)
	} else {
		fmt.Fprintf(stderr, "brw run %s: %s in %dms\n", report.Recipe.ID, decision.Name, report.DurationMS)
	}
	return decision.Code
}

func runUsageText(w io.Writer) {
	fmt.Fprintln(w, runUsage)
	for _, row := range runOutcomes {
		fmt.Fprintf(w, "  %d  %-22s %s\n", row.Code, row.Name, row.Meaning)
	}
}
