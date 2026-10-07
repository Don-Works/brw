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

func proxyInFrontOf(t *testing.T, upstream *runDaemon) *httptest.Server {
	t.Helper()
	controller, err := httpclient.New(upstream.server.URL, 10*time.Second)
	if err != nil {
		t.Fatalf("upstream controller: %v", err)
	}
	proxy := httpapi.New("", controller)

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
