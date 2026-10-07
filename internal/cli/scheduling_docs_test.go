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

func TestDocumentedLaunchdJobRuns(t *testing.T) {
	isolateLocks(t)
	document := schedulingDocument(t)
	blocks := blocksOfType(t, document, "xml")
	if len(blocks) != 1 {
		t.Fatalf("expected exactly one plist in %s, found %d", schedulingDoc, len(blocks))
	}
	plist := blocks[0]

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

	if !strings.Contains(plist, "StartCalendarInterval") {
		t.Error("the documented plist has no StartCalendarInterval, so it would never run on a schedule")
	}
	if strings.Contains(plist, "RunAtLoad") {
		t.Error("the documented plist sets RunAtLoad, which runs the job as the human logs in")
	}
}

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

	if !strings.Contains(line, ">>") || !strings.Contains(line, "2>>") {
		t.Errorf("the documented crontab line does not redirect both streams, which the paragraph above it says to do: %q", line)
	}
	return args, env
}

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

	if _, ok := cronEnv["BRW_URL"]; !ok {
		t.Error("the documented crontab line sets no BRW_URL, which is the first caveat the paragraph above it names")
	}
}
