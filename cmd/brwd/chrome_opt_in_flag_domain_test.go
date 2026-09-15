package main

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// notAboutWhichBrowser lists brwd's flags that cannot decide which browser the
// daemon drives or how it is started, plus the lane's own three arguments.
// Everything else has to be refused alongside --chrome-opt-in, because this
// lane attaches to the Chrome whose user turned remote debugging on and to no
// other.
//
// The reason strings are the point of the list: a flag is excused by an
// argument, and a new flag added without one fails the test below rather than
// being accepted and silently ignored. These are the daemon's own surface —
// what it listens on, what it records, what it lets an agent do once the
// browser is reached — and none of them reaches the browser.
var notAboutWhichBrowser = map[string]string{
	"http":                        "daemon's own HTTP listen address",
	"mcp":                         "serve MCP over stdio; no browser decision",
	"mcp-tools":                   "which tools tools/list advertises",
	"mcp-idle-exit":               "when an idle stdio server exits",
	"bridge-addr":                 "bridge listener address, unused on this lane",
	"bridge-raise-window":         "bridge behaviour, unused on this lane",
	"bridge-tab-group":            "bridge behaviour, unused on this lane",
	"bridge-follow-focus":         "bridge behaviour, unused on this lane",
	"bridge-max-inflight":         "bridge concurrency, unused on this lane",
	"profile":                     "names the policy profile, which this lane reads and checks",
	"workspace":                   "names the policy workspace binding",
	"profile-policy":              "where the policy file lives",
	"timeout":                     "default operation timeout",
	"print-system-prompt":         "prints text and exits",
	"blocked-domains":             "navigation guardrail",
	"allowed-domains":             "navigation guardrail",
	"enable-webmcp":               "page-side runtime brw installs after attaching",
	"usage-log":                   "usage ledger path",
	"usage-log-max-mb":            "usage ledger rotation",
	"usage-log-backups":           "usage ledger retention",
	"artifact-dir":                "artifact store location",
	"artifact-max-mb":             "artifact store bound",
	"artifact-total-mb":           "artifact store bound",
	"artifact-ttl":                "artifact retention",
	"artifact-encrypt":            "artifact encryption policy",
	"artifact-key-file":           "artifact key material",
	"artifact-failure-bundles":    "failure evidence policy",
	"artifact-failure-bundle-ttl": "failure evidence retention",
	"state-root":                  "snapshot store location",
	"state-key-file":              "snapshot key material",
	"baseline-root":               "baseline store location",
	"recipe-root":                 "recipe source",
	"recipe-provider-url":         "recipe source",
	"recipe-provider-token-file":  "recipe provider credential",
	"plugin-dir":                  "plugin manifests",
	"site-consent":                "per-origin consent gate",
	"site-consent-config":         "per-origin consent config",
	"site-consent-prompt":         "per-origin consent prompting",
	"confirm-actions":             "high-risk action confirmation",
	"content-nav-guard":           "navigation interception, applied after attaching",
	// The lane's own three arguments. These do decide which browser is driven,
	// but they are how the lane is told, so they cannot conflict with it.
	"chrome-opt-in":               "the lane itself",
	"chrome-opt-in-browser":       "this lane's own argument for which browser to look in",
	"chrome-opt-in-user-data-dir": "this lane's own argument for which directory to look in",
}

// Every flag brwd registers is either refused alongside --chrome-opt-in or
// listed above with a reason it cannot apply. The domain is scanned out of
// main.go, so a flag added there lands in neither set and fails here — which is
// the only thing that keeps the hand-maintained conflict table complete.
func TestEveryBrwdFlagIsClassifiedAgainstTheOptInLane(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	registered := map[string]bool{}
	for _, m := range regexp.MustCompile(`flag\.\w+\(&?[^,]+, "([a-z0-9-]+)"`).FindAllStringSubmatch(string(source), -1) {
		registered[m[1]] = true
	}
	if len(registered) < 40 {
		t.Fatalf("found only %d flags in main.go; the scan is broken, not the table", len(registered))
	}

	refused := refusedFlagNames()
	var unclassified []string
	for name := range registered {
		if refused[name] {
			continue
		}
		if _, excused := notAboutWhichBrowser[name]; excused {
			continue
		}
		unclassified = append(unclassified, "--"+name)
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Fatalf("brwd registers %v, which --chrome-opt-in neither refuses nor excuses; add a chromeOptInFlags field that refuses it, or a reason in notAboutWhichBrowser saying why it cannot decide which browser is driven", unclassified)
	}

	// The excuse list must not outlive the flags it excuses, or it silently
	// starts excusing nothing while looking complete.
	for name := range notAboutWhichBrowser {
		if !registered[name] {
			t.Errorf("notAboutWhichBrowser excuses --%s, which brwd no longer registers", name)
		}
		if refused[name] {
			t.Errorf("--%s is both refused and excused; one of the two is wrong", name)
		}
	}
}

// refusedFlagNames expands the conflict table into the flag names it reports,
// splitting the one arm that covers several launch switches at once.
func refusedFlagNames() map[string]bool {
	out := map[string]bool{}
	typ := reflect.TypeOf(chromeOptInFlags{})
	for i := range typ.NumField() {
		value := reflect.New(typ).Elem()
		set := value.Field(i)
		switch set.Kind() {
		case reflect.Bool:
			set.SetBool(true)
		case reflect.String:
			set.SetString("x")
		case reflect.Int:
			set.SetInt(1)
		}
		for _, named := range strings.Split(value.Interface().(chromeOptInFlags).conflict(), "/") {
			if named = strings.TrimPrefix(strings.TrimSpace(named), "--"); named != "" {
				out[named] = true
			}
		}
	}
	return out
}
