package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/recipe"
	"github.com/Don-Works/brw/internal/runlock"
	"github.com/Don-Works/brw/internal/usagelog"
)

const runSchema = "brw.run/1"

// Exit codes beyond the ones every verb shares.
const (
	// ExitPostconditionFailed: brw ran the recipe and the state it asserted did not hold.
	ExitPostconditionFailed = 4
	// ExitPolicyRefused: brw refused.
	ExitPolicyRefused = 5
	// ExitBusy: another run holds this profile.
	ExitBusy = 6
)

type runOutcome struct {
	// Name is the outcome string in the JSON object.
	Name string
	// Code is the process exit code.
	Code int
	// Meaning is what the operator reads in docs/scheduling.md and in --help.
	Meaning string
	// Retry says whether running it again unchanged could succeed.
	Retry bool
}

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

	return runOutcome{Name: "failed", Code: ExitActionFailed, Meaning: "unclassified"}
}

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
	// LockKey is the profile identity the run serialised on.
	LockKey string `json:"lock_key,omitempty"`
	// LockShared says the daemon named no profile, so the key above is the one every anonymous daemon shares rather than this browser's own.
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
	file       string
	version    string
	digest     string
	inputs     inputList
	lockWait   time.Duration
	timeout    time.Duration
}

type inputList map[string]string

func (l inputList) String() string {
	if len(l) == 0 {
		return ""
	}
	return strings.Join(slices.Sorted(maps.Keys(l)), ",")
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

       brw run --file <recipe.json> [--input k=v]...

Run one recipe non-interactively and report the outcome as JSON on stdout.
Intended for launchd, systemd or cron; see docs/scheduling.md.

Only one run at a time touches a browser profile. A second run started while the
first is going waits for it (--lock-wait, default 5m) and then gives up with
exit 6 rather than interleaving with it on the same tab.

flags:
  --file <path>          pass a recipe JSON file for this run without installing it
  --recipe-version <v>    stored recipe version (required without --file)
  --digest <sha256>       stored recipe digest (required without --file)
  --input key=value       a recipe input; repeatable
  --lock-wait <duration>  how long to wait for another run on this profile to
                          finish; 0 refuses immediately (default 5m)
  --timeout <duration>    how long to wait for the run itself (default 30m)
  --daemon <url>          daemon base URL (default $BRW_URL, else the profile policy)
  --profile <name>        bridge profile to act against (default $BRW_PROFILE)
  --profile-policy <path> profile policy JSON path

exit codes:`

func newRunFlagSet(opts *runOptions) *flag.FlagSet {
	fs := flag.NewFlagSet("brw run", flag.ContinueOnError)
	fs.StringVar(&opts.daemon, "daemon", "", "daemon base URL")
	fs.StringVar(&opts.profile, "profile", os.Getenv("BRW_PROFILE"), "bridge profile to act against")
	fs.StringVar(&opts.policyPath, "profile-policy", os.Getenv("BRW_PROFILE_POLICY"), "profile policy JSON path")
	fs.StringVar(&opts.file, "file", "", "recipe JSON file to run without installing")
	fs.StringVar(&opts.version, "recipe-version", "", "recipe version")
	fs.StringVar(&opts.digest, "digest", "", "recipe digest")
	fs.Var(opts.inputs, "input", "recipe input as key=value; repeatable")
	fs.DurationVar(&opts.lockWait, "lock-wait", opts.lockWait, "how long to wait for another run on this profile")
	fs.DurationVar(&opts.timeout, "timeout", opts.timeout, "how long to wait for the run itself")
	return fs
}

func runCommandFlags() []string {
	opts := runOptions{inputs: inputList{}}
	var names []string
	newRunFlagSet(&opts).VisitAll(func(f *flag.Flag) { names = append(names, "--"+f.Name) })
	return names
}

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
	request, err := prepareRunRequest(opts, positional)
	if err != nil {
		runUsageText(stderr)
		return failRun(stdout, stderr, "usage", runReport{}, err)
	}
	report := runReport{Recipe: runRecipeRef{ID: request.ID, Version: request.Version, Digest: request.Digest}}
	if request.Recipe != nil {
		digest, err := recipe.Digest(*request.Recipe)
		if err != nil {
			return failRun(stdout, stderr, "usage", report, err)
		}
		report.Recipe = runRecipeRef{ID: request.Recipe.ID, Version: request.Recipe.Version, Digest: digest}
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

	lockKey := runlock.Key(health.Identity)
	report.Profile.LockShared = lockKey == runlock.Unidentified
	if report.Profile.LockShared {
		fmt.Fprintf(stderr, "brw run: this daemon does not name the browser profile it drives, so this run holds the shared lock every anonymous daemon holds; it is NOT serialised against an identified daemon on the same browser. Start the daemon with --workspace/--profile (brwctl setup does) for a lock keyed on the profile.\n")
	}

	report.Profile.LockKey = lockKey
	lockStarted := time.Now()

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

	if result.Status != "" || len(result.Steps) > 0 {
		report.Result = &result
	}
	report.ErrorClass = httpclient.RemoteClass(runErr)

	return finishRun(stdout, stderr, report, classifyRun(runCtx, result, runErr), runErr)
}

func prepareRunRequest(opts runOptions, positional []string) (recipe.RunRequest, error) {
	request := recipe.RunRequest{Inputs: map[string]string(opts.inputs)}
	if opts.file != "" {
		if len(positional) != 0 || opts.version != "" || opts.digest != "" {
			return request, errors.New("--file cannot be combined with a recipe id, --recipe-version or --digest")
		}
		file, err := os.Open(opts.file)
		if err != nil {
			return request, err
		}
		defer file.Close()
		body, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		if err != nil {
			return request, err
		}
		value, err := recipe.Parse(body)
		if err != nil {
			return request, fmt.Errorf("invalid recipe file: %w", err)
		}
		request.Recipe = &value
		return request, nil
	}
	if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
		return request, errors.New("run takes one recipe id or --file")
	}
	request.ID = strings.TrimSpace(positional[0])
	request.Version = strings.TrimSpace(opts.version)
	request.Digest = strings.TrimSpace(opts.digest)
	if request.Version == "" || request.Digest == "" {
		return request, errors.New("run needs --recipe-version and --digest, so a scheduled job cannot silently start running a republished recipe")
	}
	return request, nil
}

func classifyRun(ctx context.Context, result recipe.RunResult, err error) runOutcome {
	if err == nil {
		return outcome("ok")
	}

	if unreachable(ctx, err) || errors.Is(err, errNoDaemon) {
		return outcome("infrastructure")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return outcome("infrastructure")
	}

	class := httpclient.RemoteClass(err)
	if class == "policy_denied" {
		return outcome("policy_refused")
	}

	if failedStep(result) {
		return outcome("postcondition_failed")
	}

	if class != "" && usagelog.Retryable(class) {
		return outcome("infrastructure")
	}
	return outcome("failed")
}

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
	if err := json.NewEncoder(stdout).Encode(report); err != nil {
		fmt.Fprintf(stderr, "brw run: encode the run report: %v\n", err)
		return ExitActionFailed
	}
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
