package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docs/scheduling.md is the only instruction anyone has for running brw from a
// scheduler, and a plist or a unit file that does not work is worse than none:
// it fails at 03:00 in a log nobody reads. So the examples are not inspected,
// they are RUN — the exact argument vector out of the document, against a
// daemon that answers, with the exit code and the JSON checked. All three
// examples, launchd, systemd and cron, go through the same vector.
//
// What is not exercised: the schedulers themselves, and the /usr/local/bin/brw
// in each example, because the vector is handed to cli.Run in process rather
// than executed as a path. A job that never fires because launchd rejected the
// plist is covered by the directive assertions below and by plutil; a brw that
// is not installed at that path is not.

const schedulingDoc = "../../docs/scheduling.md"

var (
	fencedBlock       = regexp.MustCompile("(?s)```([a-zA-Z]*)\n(.*?)```")
	plistArrayPattern = regexp.MustCompile(`(?s)<key>ProgramArguments</key>\s*<array>(.*?)</array>`)
	plistEnvPattern   = regexp.MustCompile(`(?s)<key>EnvironmentVariables</key>\s*<dict>(.*?)</dict>`)
	xmlStringPattern  = regexp.MustCompile(`<string>(.*?)</string>`)
	xmlKeyValuePairs  = regexp.MustCompile(`<key>(.*?)</key>\s*<string>(.*?)</string>`)
)

func schedulingDocument(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(schedulingDoc)
	if err != nil {
		t.Fatalf("read %s: %v", schedulingDoc, err)
	}
	return string(data)
}

func blocksOfType(t *testing.T, document, language string) []string {
	t.Helper()
	var blocks []string
	for _, match := range fencedBlock.FindAllStringSubmatch(document, -1) {
		if match[1] == language {
			blocks = append(blocks, match[2])
		}
	}
	return blocks
}

// runDocumentedCommand executes one argument vector from the docs against a
// daemon that answers, and returns the exit code and the report.
func runDocumentedCommand(t *testing.T, args []string, env map[string]string, daemonURL string) (int, runReport, string) {
	t.Helper()
	if len(args) == 0 {
		t.Fatal("the documented command has no arguments")
	}
	if filepath.Base(args[0]) != "brw" {
		t.Fatalf("the documented command runs %q, not brw", args[0])
	}
	if _, ok := env["BRW_URL"]; !ok {
		t.Fatal("the documented job sets no BRW_URL, so it would have to discover a daemon from a policy the scheduler's environment may not have")
	}
	for key, value := range env {
		if key == "BRW_URL" {
			// The document's value is the default loopback daemon; this test has
			// its own. Everything else about the invocation is used verbatim.
			value = daemonURL
		}
		t.Setenv(key, value)
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args[1:], &stdout, &stderr)
	var report runReport
	if stdout.Len() > 0 {
		if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &report); err != nil {
			t.Fatalf("the documented command did not print one JSON object: %v\n%s", err, stdout.String())
		}
	}
	return code, report, stderr.String()
}

// TestDocumentedLaunchdJobRuns runs the plist's ProgramArguments.
func TestDocumentedLaunchdJobRuns(t *testing.T) {
	isolateLocks(t)
	document := schedulingDocument(t)
	blocks := blocksOfType(t, document, "xml")
	if len(blocks) != 1 {
		t.Fatalf("expected exactly one plist in %s, found %d", schedulingDoc, len(blocks))
	}
	plist := blocks[0]

	// Well-formed first: launchd rejects the whole job on a malformed plist and
	// says so only in the system log.
	decoder := xml.NewDecoder(strings.NewReader(plist))
	decoder.Strict = true
	for {
		_, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("the documented plist is not well-formed XML: %v", err)
		}
	}
	// And a real plist where the tool to say so exists.
	if plutil, err := exec.LookPath("plutil"); err == nil {
		path := filepath.Join(t.TempDir(), "job.plist")
		if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(plutil, "-lint", path).CombinedOutput(); err != nil {
			t.Fatalf("plutil -lint rejected the documented plist: %v\n%s", err, out)
		}
	}

	arrayMatch := plistArrayPattern.FindStringSubmatch(plist)
	if arrayMatch == nil {
		t.Fatal("the documented plist has no ProgramArguments array")
	}
	var args []string
	for _, match := range xmlStringPattern.FindAllStringSubmatch(arrayMatch[1], -1) {
		args = append(args, match[1])
	}
	env := map[string]string{}
	if envMatch := plistEnvPattern.FindStringSubmatch(plist); envMatch != nil {
		for _, pair := range xmlKeyValuePairs.FindAllStringSubmatch(envMatch[1], -1) {
			env[pair[1]] = pair[2]
		}
	}

	daemon := newRunDaemon(t, &runDaemon{})
	code, report, stderrText := runDocumentedCommand(t, args, env, daemon.server.URL)
	if code != ExitOK {
		t.Fatalf("the documented launchd job exited %d: %+v\n%s", code, report, stderrText)
	}
	if report.Outcome != "ok" || report.Schema != runSchema {
		t.Fatalf("the documented launchd job reported %+v", report)
	}
	if daemon.runs != 1 {
		t.Fatalf("the documented launchd job ran the recipe %d times", daemon.runs)
	}

	// A calendar job with no schedule never fires, and one with RunAtLoad fights
	// the browser the human is opening.
	if !strings.Contains(plist, "StartCalendarInterval") {
		t.Error("the documented plist has no StartCalendarInterval, so it would never run on a schedule")
	}
	if strings.Contains(plist, "RunAtLoad") {
		t.Error("the documented plist sets RunAtLoad, which runs the job as the human logs in")
	}
}

// TestDocumentedSystemdJobRuns runs the unit's ExecStart.
func TestDocumentedSystemdJobRuns(t *testing.T) {
	isolateLocks(t)
	document := schedulingDocument(t)
	var service, timer string
	for _, block := range blocksOfType(t, document, "ini") {
		switch {
		case strings.Contains(block, "ExecStart="):
			service = block
		case strings.Contains(block, "OnCalendar="):
			timer = block
		}
	}
	if service == "" || timer == "" {
		t.Fatalf("%s does not document both a service and a timer unit", schedulingDoc)
	}

	args, env := parseUnit(t, service)
	daemon := newRunDaemon(t, &runDaemon{})
	code, report, stderrText := runDocumentedCommand(t, args, env, daemon.server.URL)
	if code != ExitOK {
		t.Fatalf("the documented systemd job exited %d: %+v\n%s", code, report, stderrText)
	}
	if report.Outcome != "ok" || report.Schema != runSchema {
		t.Fatalf("the documented systemd job reported %+v", report)
	}
	if daemon.runs != 1 {
		t.Fatalf("the documented systemd job ran the recipe %d times", daemon.runs)
	}

	// A oneshot unit that reports a busy or postcondition outcome as a unit
	// failure teaches an operator to ignore the failures.
	if !strings.Contains(service, "Type=oneshot") {
		t.Error("the documented service is not Type=oneshot, so systemd would treat the finished run as a crash")
	}
	success := unitValue(service, "SuccessExitStatus")
	for _, row := range runOutcomes {
		mentioned := strings.Contains(success, fmt.Sprint(row.Code))
		switch row.Name {
		case "busy", "postcondition_failed":
			if !mentioned {
				t.Errorf("SuccessExitStatus does not include %d (%s), so an ordinary outcome is reported as a failed unit", row.Code, row.Name)
			}
		case "infrastructure", "policy_refused", "failed":
			if mentioned {
				t.Errorf("SuccessExitStatus includes %d (%s), which hides a real failure", row.Code, row.Name)
			}
		}
	}
	if !strings.Contains(timer, "Persistent=true") {
		t.Error("the documented timer is not Persistent, so a job missed while the machine slept is skipped entirely")
	}
	if !strings.Contains(timer, "WantedBy=timers.target") {
		t.Error("the documented timer has no [Install] target, so enabling it does nothing")
	}
}

// parseUnit pulls ExecStart and Environment out of a systemd unit. The values
// here carry no quoting, which the test enforces below: a quoted argument would
// need systemd's own splitting rules and this would silently mis-split it.
func parseUnit(t *testing.T, unit string) ([]string, map[string]string) {
	t.Helper()
	exec := unitValue(unit, "ExecStart")
	if exec == "" {
		t.Fatal("the documented unit has no ExecStart")
	}
	if strings.ContainsAny(exec, `"'`) {
		t.Fatal("the documented ExecStart quotes an argument; systemd's splitting rules differ from this test's, so keep the command unquoted")
	}
	env := map[string]string{}
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, "Environment="); ok {
			key, val, found := strings.Cut(value, "=")
			if found {
				env[key] = val
			}
		}
	}
	return strings.Fields(exec), env
}

func unitValue(unit, key string) string {
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// TestSchedulingDocsAgreeWithTheExitCodeTable keeps the document from
// describing a contract the binary does not have. An operator writes their
// alerting against this table.
func TestSchedulingDocsAgreeWithTheExitCodeTable(t *testing.T) {
	document := schedulingDocument(t)
	for _, row := range runOutcomes {
		if !strings.Contains(document, fmt.Sprintf("| %d | `%s` |", row.Code, row.Name)) {
			t.Errorf("%s does not document exit %d as `%s`", schedulingDoc, row.Code, row.Name)
		}
		if !strings.Contains(document, row.Meaning) {
			t.Errorf("%s does not carry the meaning of %s: %q", schedulingDoc, row.Name, row.Meaning)
		}
	}
	// And the other direction: a code in the table that the binary cannot
	// return is an operator branching on something that never happens.
	codes := regexp.MustCompile(`\n\| (\d+) \| `+"`"+`([a-z_]+)`+"`").FindAllStringSubmatch(document, -1)
	if len(codes) != len(runOutcomes) {
		t.Fatalf("%s documents %d exit codes, the binary has %d", schedulingDoc, len(codes), len(runOutcomes))
	}
	for _, row := range codes {
		found := false
		for _, known := range runOutcomes {
			if fmt.Sprint(known.Code) == row[1] && known.Name == row[2] {
				found = true
			}
		}
		if !found {
			t.Errorf("%s documents exit %s as %q, which the binary never returns", schedulingDoc, row[1], row[2])
		}
	}
}

// parseCrontab pulls the environment and the command out of the documented
// crontab line. cron takes five schedule fields, then leading KEY=VALUE
// assignments, then the command, then the shell redirections; only the command
// and its environment are runnable here.
func parseCrontab(t *testing.T, block string) ([]string, map[string]string) {
	t.Helper()
	var line string
	for _, candidate := range strings.Split(block, "\n") {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" && !strings.HasPrefix(candidate, "#") {
			line = candidate
			break
		}
	}
	if line == "" {
		t.Fatalf("%s documents an empty crontab block", schedulingDoc)
	}
	fields := strings.Fields(line)
	if len(fields) < 6 {
		t.Fatalf("the documented crontab line has %d fields, too few for a schedule and a command: %q", len(fields), line)
	}
	// cron itself parses the first five as the schedule, so anything else here
	// is a job that never fires at the hour the document claims.
	schedule := fields[:5]
	for _, field := range schedule {
		if strings.HasPrefix(field, "-") || strings.Contains(field, "=") {
			t.Fatalf("the documented crontab schedule is %v, which cron would not read as five time fields", schedule)
		}
	}
	env := map[string]string{}
	rest := fields[5:]
	for len(rest) > 0 && strings.Contains(rest[0], "=") && !strings.HasPrefix(rest[0], "-") {
		key, value, _ := strings.Cut(rest[0], "=")
		env[key] = value
		rest = rest[1:]
	}
	var args []string
	for _, field := range rest {
		if strings.HasPrefix(field, ">") || strings.HasPrefix(field, "2>") || strings.HasPrefix(field, "&>") {
			break
		}
		args = append(args, field)
	}
	// The redirections are the second of the two caveats the document names, so
	// an example that dropped them would teach the failure it warns about.
	if !strings.Contains(line, ">>") || !strings.Contains(line, "2>>") {
		t.Errorf("the documented crontab line does not redirect both streams, which the paragraph above it says to do: %q", line)
	}
	return args, env
}

// TestDocumentedCronJobRuns runs the crontab line's command. cron is the third
// documented example and was the one nobody executed, which is how it came to
// be missing an --input the other two carry.
func TestDocumentedCronJobRuns(t *testing.T) {
	isolateLocks(t)
	blocks := blocksOfType(t, schedulingDocument(t), "crontab")
	if len(blocks) != 1 {
		t.Fatalf("expected exactly one crontab block in %s, found %d", schedulingDoc, len(blocks))
	}
	args, env := parseCrontab(t, blocks[0])

	daemon := newRunDaemon(t, &runDaemon{})
	code, report, stderrText := runDocumentedCommand(t, args, env, daemon.server.URL)
	if code != ExitOK {
		t.Fatalf("the documented cron job exited %d: %+v\n%s", code, report, stderrText)
	}
	if report.Outcome != "ok" || report.Schema != runSchema {
		t.Fatalf("the documented cron job reported %+v", report)
	}
	if daemon.runs != 1 {
		t.Fatalf("the documented cron job ran the recipe %d times", daemon.runs)
	}
}

// TestDocumentedJobsRunTheSameCommand: the macOS, Linux and cron examples have
// to be the same invocation, or one of them is the one nobody tested.
func TestDocumentedJobsRunTheSameCommand(t *testing.T) {
	document := schedulingDocument(t)
	plist := blocksOfType(t, document, "xml")[0]
	var launchd []string
	if match := plistArrayPattern.FindStringSubmatch(plist); match != nil {
		for _, str := range xmlStringPattern.FindAllStringSubmatch(match[1], -1) {
			launchd = append(launchd, str[1])
		}
	}
	var service string
	for _, block := range blocksOfType(t, document, "ini") {
		if strings.Contains(block, "ExecStart=") {
			service = block
		}
	}
	systemd, _ := parseUnit(t, service)
	cron, cronEnv := parseCrontab(t, blocksOfType(t, document, "crontab")[0])
	vectors := map[string][]string{"launchd": launchd, "systemd": systemd, "cron": cron}
	for name, vector := range vectors {
		if len(vector) == 0 {
			t.Fatalf("the %s example has no command, so this comparison is reading nothing", name)
		}
		if strings.Join(vector, " ") != strings.Join(launchd, " ") {
			t.Errorf("the launchd and %s examples differ:\n  launchd: %v\n  %s: %v", name, launchd, name, vector)
		}
	}
	// cron carries its environment on the command line rather than in a unit
	// stanza, and the document's own caveat is that it has to.
	if _, ok := cronEnv["BRW_URL"]; !ok {
		t.Error("the documented crontab line sets no BRW_URL, which is the first caveat the paragraph above it names")
	}
}
