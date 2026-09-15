package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpapi "github.com/Don-Works/brw/internal/http"
	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/recipe"
)

// `brw run` fails closed on a daemon that would stop and ask a human, because
// an unattended run has nobody to answer and hangs to its timeout instead. That
// refusal was decided from the /health of whichever daemon the run points at —
// which is not the daemon that applies the consent gate when there is a proxy in
// front, the topology brw ships for MCP clients.
//
// So the chain is assembled the way brwd assembles it: a real internal/http
// daemon whose controller is a real httpclient.Controller pointing at the
// daemon behind it. Nothing here is a stand-in for the wiring under test; the
// stand-in is only the far daemon, which is what has the prompter.
func proxyInFrontOf(t *testing.T, upstream *runDaemon) *httptest.Server {
	t.Helper()
	controller, err := httpclient.New(upstream.server.URL, 10*time.Second)
	if err != nil {
		t.Fatalf("upstream controller: %v", err)
	}
	proxy := httpapi.New("", controller)
	// The proxy forwards recipe execution to the daemon behind it, exactly as
	// brwd does. Without this the run would fail for want of a route and this
	// test would pass whatever the consent posture said.
	proxy.SetRecipeAPI(recipe.API(controller))
	server := httptest.NewServer(proxy.Handler())
	t.Cleanup(server.Close)
	return server
}

func invokeRunAt(t *testing.T, daemonURL string, extra ...string) (int, runReport, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args := append([]string{"run", "fixture.recipe", "--daemon", daemonURL,
		"--recipe-version", "1", "--digest", strings.Repeat("a", 64)}, extra...)
	code := Run(context.Background(), args, &stdout, &stderr)
	var report runReport
	if stdout.Len() > 0 {
		if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &report); err != nil {
			t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout.String())
		}
	}
	return code, report, stderr.String()
}

// The bypass: a prompting daemon behind a proxy that has no prompter of its own.
// The proxy used to report its own flags as the whole chain's answer, so the run
// proceeded, the upstream blocked on a terminal read nobody was there to answer,
// and the job reported a timeout — the failure the fail-closed rule exists to
// prevent.
func TestRunRefusesAProxyWhoseUpstreamWouldPrompt(t *testing.T) {
	isolateLocks(t)
	upstream := newRunDaemon(t, &runDaemon{interactive: true})
	proxy := proxyInFrontOf(t, upstream)

	code, report, stderrText := invokeRunAt(t, proxy.URL)
	if code != ExitPolicyRefused {
		t.Fatalf("a run through a proxy in front of a prompting daemon exited %d, want %d: %+v\n%s", code, ExitPolicyRefused, report, stderrText)
	}
	if upstream.runs != 0 {
		t.Fatalf("the run reached the prompting daemon %d times; it must refuse before starting", upstream.runs)
	}
	if !strings.Contains(report.Error, "forwards to") {
		t.Fatalf("the refusal does not say the prompter is upstream: %q", report.Error)
	}
}

// A hop that cannot be asked is not a hop that said no: an upstream too old to
// carry a consent block tells the proxy nothing, and "brw does not know whether
// this would hang" has to be answered the way "yes" is.
func TestRunRefusesAProxyWhoseUpstreamPostureCannotBeRead(t *testing.T) {
	isolateLocks(t)
	upstream := newRunDaemon(t, &runDaemon{noConsent: true})
	proxy := proxyInFrontOf(t, upstream)

	code, report, _ := invokeRunAt(t, proxy.URL)
	if code != ExitPolicyRefused {
		t.Fatalf("a run through a proxy whose upstream reports no posture exited %d, want %d: %+v", code, ExitPolicyRefused, report)
	}
	if upstream.runs != 0 {
		t.Fatalf("the run reached the upstream %d times", upstream.runs)
	}
	if !strings.Contains(report.Error, "could not read") {
		t.Fatalf("the refusal does not name what was missing: %q", report.Error)
	}
}

// And the same chain with nothing that would ask still runs, so the rule above
// is a gate rather than a wall: a proxy that refused every run would pass both
// tests above and be useless.
func TestRunThroughAProxyWhoseChainCannotPromptStillRuns(t *testing.T) {
	isolateLocks(t)
	upstream := newRunDaemon(t, &runDaemon{})
	proxy := proxyInFrontOf(t, upstream)

	code, report, stderrText := invokeRunAt(t, proxy.URL)
	if code != ExitOK {
		t.Fatalf("a run through a proxy whose chain has no prompter exited %d: %+v\n%s", code, report, stderrText)
	}
	if upstream.runs != 1 {
		t.Fatalf("the upstream served %d runs", upstream.runs)
	}
}
