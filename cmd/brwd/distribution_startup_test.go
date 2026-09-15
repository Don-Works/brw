package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --remote auto under a profile the policy bars from direct CDP, run for real.
//
// The endpoint discovery finds is in brw's own default profile directory — the
// second directory autoConnectSearchDirs searches, and not the one the policy
// names. The first version of the gate exempted every DevToolsActivePort hit,
// so this exact invocation attached to a browser the policy never named and
// then reported the policy's user_data_dir and profile_directory as its
// identity.
func TestStartupRefusesAutoConnectToABrowserTheProfilePolicyDoesNotName(t *testing.T) {
	home := t.TempDir()
	policyProfileDir := filepath.Join(home, "policy-chrome")
	if err := os.MkdirAll(policyProfileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, port := fakeDevToolsEndpoint(t)
	// The browser is running out of brw's default profile, not the policy's.
	writeActivePort(t, filepath.Join(home, ".brw", "chrome-profile"), port)
	policy := writeBridgeOnlyPolicy(t, home, policyProfileDir)

	output, code := runBrwdUntilItStops(t, []string{
		"--profile", "chrome-work", "--profile-policy", policy,
		"--remote", "auto", "--http", "off",
	}, startupEnvironment(home), 60*time.Second)

	if code == 0 {
		t.Fatalf("brwd attached to a browser the policy does not name and started:\n%s", output)
	}
	for _, want := range []string{"allowed only through the extension bridge", filepath.Join(home, ".brw", "chrome-profile")} {
		if !strings.Contains(output, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "--remote auto attached to") {
		t.Errorf("the daemon attached before it refused:\n%s", output)
	}
}

// The other side of the same gate: the browser running out of the directory the
// policy named IS that profile's browser, whatever the policy says about direct
// CDP launches, so discovery is allowed to attach to it. Without this the fix
// would be a gate that refuses everything.
func TestStartupAttachesToTheProfilesOwnBrowser(t *testing.T) {
	home := t.TempDir()
	policyProfileDir := filepath.Join(home, "policy-chrome")
	_, port := fakeDevToolsEndpoint(t)
	writeActivePort(t, policyProfileDir, port)
	policy := writeBridgeOnlyPolicy(t, home, policyProfileDir)

	output, code := runBrwdUntilItStops(t, []string{
		"--profile", "chrome-work", "--profile-policy", policy,
		"--remote", "auto", "--http", "off", "--timeout", "5s",
	}, startupEnvironment(home), 90*time.Second)

	// It still fails: the stub answers discovery but serves no DevTools
	// WebSocket. What matters is where it failed.
	if !strings.Contains(output, "--remote auto attached to") {
		t.Fatalf("the daemon never got past the policy gate for its own browser (exit %d):\n%s", code, output)
	}
	if strings.Contains(output, "allowed only through the extension bridge") {
		t.Fatalf("the profile's own browser was refused:\n%s", output)
	}
}

// No profile policy is the default install, and it has never had a direct-CDP
// prohibition to honour. This is the row a unit table cannot tell apart from
// "a direct-CDP profile", because both reach the same branch with the same
// argument; at startup they are different invocations.
func TestStartupAutoConnectsWithNoProfilePolicyAtAll(t *testing.T) {
	home := t.TempDir()
	_, port := fakeDevToolsEndpoint(t)
	// Deliberately NOT the directory this daemon was pointed at: with a policy
	// barring direct CDP that is the refusal above, so what is exercised here
	// is only "no policy, no prohibition".
	writeActivePort(t, filepath.Join(home, ".brw", "chrome-profile"), port)
	elsewhere := filepath.Join(home, "some-other-chrome")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}

	output, _ := runBrwdUntilItStops(t, []string{
		"--remote", "auto", "--http", "off", "--timeout", "5s",
		"--user-data-dir", elsewhere,
	}, startupEnvironment(home), 90*time.Second)

	if !strings.Contains(output, "--remote auto attached to") {
		t.Fatalf("a daemon with no profile policy did not attach to the browser it found:\n%s", output)
	}
	if strings.Contains(output, "allowed only through the extension bridge") {
		t.Fatalf("a daemon with no profile policy was refused by a policy it does not have:\n%s", output)
	}
}

// --idle-exit with no HTTP listener was made a startup failure, and that broke
// working machines: BRW_IDLE_EXIT is one of the environment-sourced defaults,
// so an exported variable turned an ordinary `brwd --mcp --http off` into a
// daemon that would not start. It is a note now, and startup continues.
//
// The invocation is deliberately one that fails LATER (--headless with
// --bridge): log.Fatalf stops at the first refusal, so seeing the later one is
// what proves the daemon got past the idle-exit decision.
func TestStartupDoesNotRefuseIdleExitWithoutAnHTTPListener(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		env      []string
		wantNote string
	}{
		{
			name:     "a stdio daemon arms the watcher that works there",
			args:     []string{"--mcp", "--http", "off", "--idle-exit", "20m", "--headless", "--bridge"},
			wantNote: "arming the stdio idle exit",
		},
		{
			name:     "the environment default a machine already exports",
			args:     []string{"--mcp", "--http", "off", "--headless", "--bridge"},
			env:      []string{"BRW_IDLE_EXIT=20m"},
			wantNote: "arming the stdio idle exit",
		},
		{
			name:     "nothing measures use at all, and it says so",
			args:     []string{"--http", "off", "--idle-exit", "20m", "--headless", "--bridge"},
			wantNote: "never fire",
		},
		// A disposable proxy inherits a 90-minute --mcp-idle-exit nobody typed.
		// That must not quietly outrank the duration the operator did type.
		{
			name:     "the proxy default does not outrank the typed duration",
			args:     []string{"--mcp", "--upstream-http", "http://127.0.0.1:1", "--http", "off", "--idle-exit", "20m", "--headless", "--bridge"},
			wantNote: "arming the stdio idle exit",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			output, code := runBrwdUntilItStops(t, tc.args, startupEnvironment(home, tc.env...), 60*time.Second)
			if code == 0 {
				t.Fatalf("the invocation was expected to fail on --headless with --bridge:\n%s", output)
			}
			if !strings.Contains(output, "--headless cannot be combined with --bridge") {
				t.Fatalf("startup stopped before the flag check that follows the idle-exit decision, so --idle-exit is still fatal:\n%s", output)
			}
			if !strings.Contains(output, tc.wantNote) {
				t.Errorf("the daemon did not report what it did with --idle-exit (%q):\n%s", tc.wantNote, output)
			}
		})
	}
}

// An --upstream-http proxy and the daemon behind it are one browser reached
// through two URLs, and everything an unattended caller asks the proxy has to
// be answered for the chain: which profile it drives, and whether anything in
// it would stop and ask a human.
//
// Both are wired inside main() and neither has a unit seam, so this starts the
// real daemon in front of a stub upstream and reads its /health.
func TestProxyStartupReportsTheUpstreamsProfileAndConsentPosture(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"ok":true,"identity":{"workspace":"work","profile":"chrome-work","user_data_dir":"/profiles/work","profile_directory":"Profile 1","mode":"bridge","transport":"extension-bridge","headless":true},"consent":{"enabled":true,"interactive":true}}`)
	}))
	defer upstream.Close()

	health := startProxyAndReadHealth(t, upstream.URL)

	identity, _ := health["identity"].(map[string]any)
	if identity == nil {
		t.Fatalf("the proxy reports no identity at all: %v", health)
	}
	for field, want := range map[string]string{
		"workspace":         "work",
		"profile":           "chrome-work",
		"user_data_dir":     "/profiles/work",
		"profile_directory": "Profile 1",
		"transport":         "extension-bridge",
	} {
		if got, _ := identity[field].(string); got != want {
			t.Errorf("the proxy reports %s = %q, want the upstream's %q; the run lock keys on these, so a proxy that reports none takes a different lock from the daemon behind it", field, got, want)
		}
	}
	// Its own mode is what it is. "upstream-http" says how the agent reaches
	// brw; adopting the upstream's would make the proxy claim to be the bridge.
	if got, _ := identity["mode"].(string); got != "upstream-http" {
		t.Errorf("the proxy adopted the upstream's mode: %q", got)
	}

	consent, _ := health["consent"].(map[string]any)
	if consent == nil {
		t.Fatalf("the proxy reports no consent posture: %v", health)
	}
	if interactive, _ := consent["interactive"].(bool); !interactive {
		t.Errorf("the proxy reports interactive = false for a chain whose upstream prompts on its terminal: %v", consent)
	}
}

// A hop that cannot be asked is not a hop that said no. The proxy here has no
// prompter of its own and the daemon behind it answers nothing, so the honest
// answer to "could this stop and ask" is "brw does not know" — which an
// unattended caller has to treat the way it treats "yes".
func TestProxyStartupReportsAnUnreadableUpstreamPostureAsUnknown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		// A daemon built before /health carried a consent block.
		fmt.Fprint(w, `{"ok":true,"identity":{"workspace":"work","profile":"chrome-work","user_data_dir":"/profiles/work","profile_directory":"Profile 1","mode":"direct","transport":"direct-cdp"}}`)
	}))
	defer upstream.Close()

	health := startProxyAndReadHealth(t, upstream.URL)
	consent, _ := health["consent"].(map[string]any)
	if consent == nil {
		t.Fatalf("the proxy reports no consent posture: %v", health)
	}
	if unknown, _ := consent["unknown"].(bool); !unknown {
		t.Errorf("the proxy answered for an upstream it could not read: %v", consent)
	}
	if reason, _ := consent["unknown_reason"].(string); !strings.Contains(reason, "consent posture") {
		t.Errorf("the unknown posture does not say what could not be asked: %q", reason)
	}
}

// startProxyAndReadHealth runs a real brwd as an --upstream-http proxy and
// returns the decoded /health it serves. No browser is launched in this mode:
// the controller is the upstream daemon.
func startProxyAndReadHealth(t *testing.T, upstreamURL string) map[string]any {
	t.Helper()
	home := t.TempDir()
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, brwdBinary(t), "--upstream-http", upstreamURL, "--http", addr, "--usage-log", "off")
	cmd.Env = startupEnvironment(home)
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the proxy: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for {
		response, err := client.Get("http://" + addr + "/health")
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr != nil {
				t.Fatalf("read /health: %v", readErr)
			}
			var health map[string]any
			if err := json.Unmarshal(body, &health); err != nil {
				t.Fatalf("/health is not JSON: %v\n%s", err, body)
			}
			return health
		}
		if time.Now().After(deadline) {
			t.Fatalf("the proxy never served /health on %s: %v\n%s", addr, err, output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// An MCP tool call never crosses the HTTP mux, so the --idle-exit watcher armed
// on that mux counts a busy stdio session as silence. main() bridges the two by
// reporting each MCP request to the HTTP server's activity tracker; without
// that, this daemon shuts the browser down under the agent driving it.
//
// Both halves are asserted, because "still running" alone would also pass on a
// daemon whose idle watcher never ran at all.
func TestStdioRequestsPostponeTheHTTPIdleExit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	for _, tc := range []struct {
		name        string
		keepBusy    bool
		wantStillUp bool
	}{
		{name: "a silent stdio session is an idle daemon", keepBusy: false},
		{name: "a busy stdio session is not", keepBusy: true, wantStillUp: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, brwdBinary(t),
				"--mcp", "--upstream-http", upstream.URL, "--http", addr,
				"--idle-exit", "3s", "--usage-log", "off")
			cmd.Env = startupEnvironment(home)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr strings.Builder
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatalf("start brwd --mcp: %v", err)
			}
			go func() { _, _ = io.Copy(io.Discard, bufio.NewReader(stdout)) }()

			stop := make(chan struct{})
			if tc.keepBusy {
				go func() {
					id := 0
					for {
						select {
						case <-stop:
							return
						default:
						}
						id++
						if _, err := fmt.Fprintf(stdin, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":\"tools/list\"}\n", id); err != nil {
							return
						}
						time.Sleep(400 * time.Millisecond)
					}
				}()
			}

			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()

			select {
			case <-exited:
				close(stop)
				if tc.wantStillUp {
					t.Fatalf("the daemon exited on its idle timer while the stdio session was making calls:\n%s", stderr.String())
				}
			case <-time.After(9 * time.Second):
				close(stop)
				_ = stdin.Close()
				_ = cmd.Process.Kill()
				<-exited
				if !tc.wantStillUp {
					t.Fatalf("the daemon never exited, so --idle-exit is not armed here and the other half of this test proves nothing:\n%s", stderr.String())
				}
			}
		})
	}
}
