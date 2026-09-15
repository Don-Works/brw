package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/mcp"
	"github.com/Don-Works/brw/internal/recipe"
)

func TestEffectiveMCPIdleExitDefaultsOnlyDisposableUpstreamProxy(t *testing.T) {
	if got := effectiveMCPIdleExit(0, true, "http://127.0.0.1:17410", false); got != defaultProxyIdleExit {
		t.Fatalf("default = %s, want %s", got, defaultProxyIdleExit)
	}
	for name, tc := range map[string]struct {
		configured time.Duration
		mcp        bool
		upstream   string
		explicit   bool
		want       time.Duration
	}{
		"explicit zero disables": {mcp: true, upstream: "http://127.0.0.1:17410", explicit: true, want: 0},
		"explicit duration wins": {configured: 15 * time.Minute, mcp: true, upstream: "http://127.0.0.1:17410", explicit: true, want: 15 * time.Minute},
		"direct MCP unchanged":   {mcp: true, want: 0},
		"daemon unchanged":       {upstream: "http://127.0.0.1:17410", want: 0},
	} {
		t.Run(name, func(t *testing.T) {
			if got := effectiveMCPIdleExit(tc.configured, tc.mcp, tc.upstream, tc.explicit); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestUsageLogFileNameContainsOnlySafeIdentityMetadata(t *testing.T) {
	got := usageLogFileName(brwidentity.Identity{
		Workspace: "agent / brw:chromium", Profile: "Default", Mode: "bridge",
	})
	if got != "agent-brw-chromium-Default-bridge.ndjson" {
		t.Fatalf("got %q", got)
	}
	if strings.ContainsAny(got, `/\\:`) {
		t.Fatalf("unsafe filename %q", got)
	}
}

func TestResolveUsageLogPathOffAndExplicit(t *testing.T) {
	if got, err := resolveUsageLogPath("off", brwidentity.Identity{}); err != nil || got != "" {
		t.Fatalf("off = %q, %v", got, err)
	}
	explicit := filepath.Join(t.TempDir(), "usage.ndjson")
	if got, err := resolveUsageLogPath(explicit, brwidentity.Identity{}); err != nil || got != explicit {
		t.Fatalf("explicit = %q, %v", got, err)
	}
}

func TestUsageLogMaxBytes(t *testing.T) {
	if got, err := usageLogMaxBytes(20); err != nil || got != 20*1024*1024 {
		t.Fatalf("got %d, %v", got, err)
	}
	if _, err := usageLogMaxBytes(-1); err == nil {
		t.Fatal("expected negative-size error")
	}
}

func TestMebibytesRejectsNonPositiveLimits(t *testing.T) {
	if got, err := mebibytes(128); err != nil || got != 128<<20 {
		t.Fatalf("got %d, %v", got, err)
	}
	for _, value := range []int{0, -1} {
		if _, err := mebibytes(value); err == nil {
			t.Fatalf("mebibytes(%d) accepted", value)
		}
	}
}

func TestDefaultArtifactRootIsStableAndIsolatedByRuntimeIdentity(t *testing.T) {
	first, err := defaultArtifactRoot(brwidentity.Identity{Workspace: "synthetic", Profile: "one"})
	if err != nil {
		t.Fatal(err)
	}
	again, _ := defaultArtifactRoot(brwidentity.Identity{Workspace: "synthetic", Profile: "one", Mode: "bridge"})
	other, _ := defaultArtifactRoot(brwidentity.Identity{Workspace: "synthetic", Profile: "two"})
	fallback, _ := defaultArtifactRoot(brwidentity.Identity{})
	if first != again || first == other || filepath.Base(fallback) != "default" || !strings.HasPrefix(filepath.Base(first), "runtime-") {
		t.Fatalf("first=%q again=%q other=%q fallback=%q", first, again, other, fallback)
	}
}

func TestReadPrivateTokenFileRequiresOwnerOnlyRegularFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "provider-token")
	if err := os.WriteFile(path, []byte("sensitive-provider-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readPrivateTokenFile(path); err != nil || got != "sensitive-provider-token" {
		t.Fatalf("got %q, %v", got, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateTokenFile(path); err == nil {
		t.Fatal("world-readable token file accepted")
	}
	link := filepath.Join(directory, "token-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateTokenFile(link); err == nil {
		t.Fatal("symlink token file accepted")
	}
}

func TestLocalTransport(t *testing.T) {
	tests := []struct {
		name     string
		upstream string
		remote   string
		bridge   bool
		optIn    bool
		provider bool
		want     string
		wantMode string
	}{
		{"direct cdp", "", "", false, false, false, brwidentity.TransportDirectCDP, "direct"},
		{"extension bridge", "", "", true, false, false, brwidentity.TransportExtensionBridge, "bridge"},
		// The opt-in lane is its own transport, not direct CDP: it has the
		// cookie and incognito access the bridge lacks AND it drives the
		// browser the user is signed into, so its catalogue matches neither.
		{"chrome opt-in", "", "", false, true, false, brwidentity.TransportChromeOptIn, "chrome-opt-in"},
		// --remote at a loopback endpoint is its own transport for the same kind
		// of reason: direct CDP means a browser brw started and may therefore be
		// pointed at a staging directory brw later deletes, and this is somebody
		// else's browser. Everything else about it is local, so a path, an
		// upload and the clipboard all still mean what the caller meant.
		{"remote at loopback", "", "http://127.0.0.1:9222", false, false, false, brwidentity.TransportRemoteCDP, "remote"},
		{"remote at localhost", "", "http://localhost:9222", false, false, false, brwidentity.TransportRemoteCDP, "remote"},
		// A browser a plugin lent brw is a fifth lane, and specifically not the
		// --remote one above: that endpoint is on this machine. This one is
		// elsewhere, and an agent that cannot tell the two apart cannot avoid
		// asking a browser on another machine for this machine's files.
		{"browser provider", "", "", false, false, true, brwidentity.TransportOffHostCDP, "browser-provider"},
		// --remote is classified by WHERE ITS ENDPOINT POINTS, not by the flag's
		// name, so an endpoint on another machine is the same lane a provider's
		// browser is on. This is the case that reported direct-cdp while
		// brw_state, brw_downloads, brw_upload_file and brw_clipboard stayed
		// advertised, each of them answering about the wrong host.
		//
		// The MODE stays "remote": mode names how the operator reached the
		// browser and transport names where it is, and those are different
		// questions. Only the transport gates a capability.
		{"remote off this machine", "", "http://198.51.100.7:9222", false, false, false, brwidentity.TransportOffHostCDP, "remote"},
		{"remote at a hostname", "", "wss://browsers.example/devtools/browser/x", false, false, false, brwidentity.TransportOffHostCDP, "remote"},
		// The opt-in lane sets RemoteURL to the endpoint it discovered, so the
		// two must not race: it stays chrome-opt-in-cdp.
		{"opt-in keeps its lane once the endpoint is resolved", "", "http://127.0.0.1:9222", false, true, false, brwidentity.TransportChromeOptIn, "chrome-opt-in"},
		// But not when the endpoint is somewhere else. The opt-in describes a
		// browser on this machine; an endpoint that is not on this machine
		// contradicts it, and the restrictive answer is the safe one.
		{"opt-in loses to an endpoint elsewhere", "", "http://198.51.100.7:9222", false, true, false, brwidentity.TransportOffHostCDP, "chrome-opt-in"},
		// A proxy cannot know how its upstream reaches Chrome, so it reports
		// empty and adopts the upstream's answer from /health.
		{"upstream proxy defers", "http://127.0.0.1:17410", "", false, false, false, "", "upstream-http"},
		{"upstream proxy defers even with bridge set", "http://127.0.0.1:17410", "", true, false, false, "", "upstream-http"},
		{"upstream proxy defers even with opt-in set", "http://127.0.0.1:17410", "", false, true, false, "", "upstream-http"},
		{"upstream proxy defers even with a provider loaded", "http://127.0.0.1:17410", "", false, false, true, "", "upstream-http"},
		{"upstream proxy defers even with a remote endpoint", "http://127.0.0.1:17410", "http://198.51.100.7:9222", false, false, false, "", "upstream-http"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := localTransport(tt.upstream, tt.remote, tt.bridge, tt.optIn, tt.provider)
			if got != tt.want {
				t.Fatalf("localTransport(%q, %q, %v, %v, %v) = %q, want %q", tt.upstream, tt.remote, tt.bridge, tt.optIn, tt.provider, got, tt.want)
			}
			// /health serves mode and transport together; a caller that gates
			// on one and logs the other must not see two different lanes.
			if mode := daemonMode(tt.upstream, tt.remote, tt.bridge, tt.optIn, tt.provider); mode != tt.wantMode {
				t.Fatalf("daemonMode(%q, %q, %v, %v, %v) = %q, want %q", tt.upstream, tt.remote, tt.bridge, tt.optIn, tt.provider, mode, tt.wantMode)
			}
			if got != "" && !brwidentity.KnownTransport(got) {
				t.Fatalf("localTransport returned %q, which brwidentity does not classify; every tool's availability on that lane would be undefined", got)
			}
			// The identity the daemon reports is built from the same lane, so a
			// daemon cannot advertise one transport and resolve its on-disk
			// scope as another.
			identity := resolveIdentity(identityInputs{
				UpstreamHTTP:    tt.upstream,
				Bridge:          tt.bridge,
				ChromeOptIn:     tt.optIn,
				BrowserProvider: tt.provider,
				RemoteURL:       tt.remote,
			})
			if identity.Transport != tt.want {
				t.Fatalf("resolveIdentity reported transport %q for the same lane, want %q", identity.Transport, tt.want)
			}
		})
	}
	// Every transport the daemon can report must be one brwidentity declares,
	// or a table keyed by transport silently fails to classify it.
	for _, tt := range tests {
		if tt.want == "" {
			continue
		}
		if !slices.Contains(brwidentity.Transports(), tt.want) {
			t.Errorf("localTransport can report %q, which is not in %v", tt.want, brwidentity.Transports())
		}
	}
}

// Every transport brwidentity classifies has to be a lane brwd can actually
// select, and every lane brwd selects has to be classified. A transport added
// to the table with no way to reach it is a catalogue nobody runs; a lane brwd
// reports that brwidentity does not know is a lane on which every
// capability-gated tool's availability is undefined.
//
// The table below is also where a new lane declares whether brw STARTED the
// browser, which is the property the download refusal reads
// (browser.Manager.stagesDownloads). Declaring it here and comparing it against
// the transport's own RuntimeDownloadRouting is what stops the two drifting —
// drift is what let `--remote` retarget and delete a person's downloads while
// the Chrome opt-in lane refused.
func TestEveryClassifiedTransportIsALaneBrwdCanSelect(t *testing.T) {
	type lane struct {
		upstream string
		remote   string
		bridge   bool
		optIn    bool
		provider bool
		// brwStartsTheBrowser is what this lane does, not what it is called.
		brwStartsTheBrowser bool
		// browserOnThisHost is the second, independent property: whether a
		// filesystem path and the clipboard mean the same thing at both ends of
		// the socket. It is not implied by the first — brw does not start the
		// browser on three of these lanes, and shares a disk with two of them.
		browserOnThisHost bool
	}
	lanes := map[string]lane{
		brwidentity.TransportDirectCDP:       {brwStartsTheBrowser: true, browserOnThisHost: true},
		brwidentity.TransportRemoteCDP:       {remote: "http://127.0.0.1:9222", browserOnThisHost: true},
		brwidentity.TransportChromeOptIn:     {optIn: true, browserOnThisHost: true},
		brwidentity.TransportExtensionBridge: {bridge: true, browserOnThisHost: true},
		brwidentity.TransportOffHostCDP:      {provider: true},
	}
	for _, transport := range brwidentity.Transports() {
		l, ok := lanes[transport]
		if !ok {
			t.Errorf("brwidentity classifies %q but no brwd invocation here produces it: say which flags select that lane, whether brw starts the browser on it, and whether that browser is on this machine", transport)
			continue
		}
		if got := localTransport(l.upstream, l.remote, l.bridge, l.optIn, l.provider); got != transport {
			t.Errorf("the lane recorded for %q selects %q instead", transport, got)
			continue
		}
		caps, known := brwidentity.Capabilities(transport)
		if !known {
			t.Errorf("transport %q has no capabilities", transport)
			continue
		}
		if caps.RuntimeDownloadRouting != l.brwStartsTheBrowser {
			t.Errorf("transport %q declares RuntimeDownloadRouting=%v but brw %s the browser on that lane; brw may only route downloads in a browser it started",
				transport, caps.RuntimeDownloadRouting,
				map[bool]string{true: "starts", false: "does not start"}[l.brwStartsTheBrowser])
		}
		if caps.BrowserOnThisHost != l.browserOnThisHost {
			t.Errorf("transport %q declares BrowserOnThisHost=%v but the browser on that lane %s on this machine; a path or a clipboard read answers about the browser's host either way",
				transport, caps.BrowserOnThisHost,
				map[bool]string{true: "is", false: "is not"}[l.browserOnThisHost])
		}
	}
}

// The capability gates live on browser.Manager and the tools/list filter lives
// on the reported transport. They are two readings of one question — is the
// browser on the machine brwd runs on? — and a lane where they disagree is a
// daemon that advertises brw_upload_file and then refuses it, or worse, one
// that refuses nothing while reporting a transport nobody filters on.
func TestTheDaemonLaneAndTheManagerAgreeAboutWhereTheBrowserIs(t *testing.T) {
	for name, lane := range map[string]struct {
		upstream string
		bridge   bool
		optIn    bool
		provider bool
		remote   string
		config   browser.Config
	}{
		"launched here":      {config: browser.Config{}},
		"remote at loopback": {remote: "http://127.0.0.1:9222", config: browser.Config{RemoteURL: "http://127.0.0.1:9222"}},
		"remote off machine": {remote: "http://198.51.100.7:9222", config: browser.Config{RemoteURL: "http://198.51.100.7:9222"}},
		"remote at a name":   {remote: "wss://browsers.example/x", config: browser.Config{RemoteURL: "wss://browsers.example/x"}},
		"plugin-supplied":    {provider: true, config: browser.Config{Remote: &browser.RemoteTarget{WebSocketURL: "wss://browsers.example/devtools/browser/x"}}},
		"remote unparseable": {remote: "http://[::1", config: browser.Config{RemoteURL: "http://[::1"}},
	} {
		t.Run(name, func(t *testing.T) {
			transport := localTransport(lane.upstream, lane.remote, lane.bridge, lane.optIn, lane.provider)
			advertisedLocally := transport != brwidentity.TransportOffHostCDP
			if got := lane.config.BrowserOnThisHost(); got != advertisedLocally {
				t.Fatalf("the daemon reports transport %q (browser here = %v) while browser.Config says browser here = %v; the advertised surface and the refusals disagree",
					transport, advertisedLocally, got)
			}
		})
	}
}

// The whole chain the blocker went through, in one test: the flags a daemon was
// started with, the identity it therefore reports, and the tools it therefore
// offers. `brwd --remote http://198.51.100.7:9222` reported direct-cdp, so
// tools/list handed an agent brw_state, brw_downloads, brw_upload_file and
// brw_clipboard in full, and brw_state restore would have decrypted this host's
// snapshot of a session a human signed into here and installed its cookies on
// another machine.
func TestARemoteEndpointOffThisMachineDoesNotAdvertiseThisHostsCapabilities(t *testing.T) {
	offHost := resolveIdentity(identityInputs{RemoteURL: "http://198.51.100.7:9222"})
	if offHost.Transport != brwidentity.TransportOffHostCDP {
		t.Fatalf("--remote off this machine reported transport %q, want %q", offHost.Transport, brwidentity.TransportOffHostCDP)
	}
	advertised := advertisedOverMCP(t, offHost)
	for _, name := range []string{"brw_state", "brw_downloads", "brw_set_download_path", "brw_upload_file", "brw_clipboard"} {
		if advertised[name] {
			t.Errorf("a daemon driving a browser on another machine advertises %s, which answers about the wrong host", name)
		}
	}
	// The lane is narrowed, not disabled: everything that is just CDP is still
	// there, or the refusal would have cost more than the damage it prevents.
	for _, name := range []string{"brw_open", "brw_snapshot", "brw_click", "brw_cookies", "brw_open_incognito"} {
		if !advertised[name] {
			t.Errorf("a daemon driving a browser on another machine dropped %s, which works there", name)
		}
	}

	// And a loopback endpoint keeps the full local surface, so the gate did not
	// swallow every --remote setup that works today.
	onHost := resolveIdentity(identityInputs{RemoteURL: "http://127.0.0.1:9222"})
	if onHost.Transport != brwidentity.TransportRemoteCDP {
		t.Fatalf("--remote at loopback reported transport %q, want %q", onHost.Transport, brwidentity.TransportRemoteCDP)
	}
	localAdvertised := advertisedOverMCP(t, onHost)
	for _, name := range []string{"brw_state", "brw_downloads", "brw_upload_file", "brw_clipboard"} {
		if !localAdvertised[name] {
			t.Errorf("--remote at a loopback endpoint stopped advertising %s; the browser is on this machine and it works", name)
		}
	}
	// The one thing it does give up, because brw did not start that browser:
	// Browser.setDownloadBehavior is browser-context-wide, so pointing it at
	// brw's staging directory would move the files whoever started it downloads
	// by hand. That is why the lane is not direct-cdp.
	if localAdvertised["brw_set_download_path"] {
		t.Error("--remote at a loopback endpoint advertises brw_set_download_path; brw did not start that browser and must not retarget its downloads")
	}
}

// advertisedOverMCP is the tool surface an agent really sees: a tools/list
// request served by the same mcp.Server main builds, given the identity the
// daemon reports. Asked over the wire rather than through a helper, because the
// wire is where a lane that classified itself wrongly did its damage.
func advertisedOverMCP(t *testing.T, identity brwidentity.Identity) map[string]bool {
	t.Helper()
	server := mcp.NewWithToolProfile(nil, "all")
	server.SetIdentity(identity)
	var out bytes.Buffer
	request := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"
	if err := server.Serve(context.Background(), strings.NewReader(request), &out); err != nil {
		t.Fatalf("serve tools/list: %v", err)
	}
	var response struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &response); err != nil {
		t.Fatalf("decode tools/list: %v (%s)", err, out.String())
	}
	if len(response.Result.Tools) == 0 {
		t.Fatalf("tools/list returned nothing for identity %+v", identity)
	}
	names := map[string]bool{}
	for _, tool := range response.Result.Tools {
		names[tool.Name] = true
	}
	return names
}

func TestFindInstalledExtensionNeedsAManifest(t *testing.T) {
	dir := t.TempDir()
	withManifest := filepath.Join(dir, "good")
	bare := filepath.Join(dir, "bare")
	for _, d := range []string{withManifest, bare} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(withManifest, "manifest.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	orig := extensionSearchPaths
	t.Cleanup(func() { extensionSearchPaths = orig })

	extensionSearchPaths = func() []string { return []string{bare, withManifest} }
	got, ok := findInstalledExtension()
	if !ok || got != withManifest {
		t.Fatalf("findInstalledExtension() = %q,%v; want %q,true (a dir without a manifest is not our extension)", got, ok, withManifest)
	}

	extensionSearchPaths = func() []string { return []string{bare} }
	if got, ok := findInstalledExtension(); ok {
		t.Fatalf("findInstalledExtension() = %q,true; want not found", got)
	}
}

// A receipt only means something if it outlives the daemon that wrote it, so
// the write ledger is the provider — and only when the provider is a remote
// party. A directory of local JSON files is this machine, so it gets none.
func TestRecipeReceiptsOnlyComeFromAProviderThatOutlivesTheDaemon(t *testing.T) {
	remote, err := recipe.NewHTTPProvider(recipe.HTTPProviderConfig{BaseURL: "https://recipes.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if recipeReceiptsFor(remote) == nil {
		t.Fatal("an HTTP recipe provider is a write ledger and was not used as one")
	}

	local, err := recipe.NewCatalog(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := recipeReceiptsFor(local); got != nil {
		t.Fatalf("a local catalogue was used as a write ledger: %T", got)
	}

	// A deployment with no write ledger has to say so while it is starting.
	// Otherwise the first anyone hears of it is a daemon that came back with no
	// record of the write it was in the middle of.
	line := recipeReceiptStatusLine(nil)
	for _, want := range []string{"cannot hold external-write receipts", "--recipe-provider-url"} {
		if !strings.Contains(line, want) {
			t.Fatalf("startup line %q does not mention %q", line, want)
		}
	}
	if enabled := recipeReceiptStatusLine(recipe.NewMemoryReceipts()); strings.Contains(enabled, "cannot hold") {
		t.Fatalf("a deployment that does hold receipts reported %q", enabled)
	}
}
