package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/baseline"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwconfig"
	"github.com/Don-Works/brw/internal/brwidentity"
	cdplaunch "github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/chromeoptin"
	"github.com/Don-Works/brw/internal/extensionbridge"
	httpapi "github.com/Don-Works/brw/internal/http"
	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/mcp"
	"github.com/Don-Works/brw/internal/navpolicy"
	"github.com/Don-Works/brw/internal/plugin"
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/recipe"
	"github.com/Don-Works/brw/internal/sessionstate"
	"github.com/Don-Works/brw/internal/usagelog"
)

const defaultProxyIdleExit = 90 * time.Minute

type stringList []string

func (s *stringList) String() string {
	return strings.Join(*s, ",")
}

func (s *stringList) Set(value string) error {
	if value == "" {
		return nil
	}
	*s = append(*s, value)
	return nil
}

func main() {
	log.SetOutput(os.Stderr)
	// An exported HAR names the brw that produced it, so the build's version has
	// to reach the artifact package as well as the usage ledger.
	artifact.SetVersion(mcp.Version)

	var extensions stringList
	var chromeArgs stringList
	var cfg browser.Config
	var httpAddr string
	var mcpMode bool
	var bridgeMode bool
	var bridgeAddr string
	var bridgeRaiseWindow bool
	var bridgeTabGroup string
	var bridgeFollowFocus bool
	var bridgeMaxInflight int
	var timeout time.Duration
	var profileName string
	var workspaceName string
	var profilePolicyPath string
	var unsafeAllowDefaultProfileCDP bool
	var unsafeRealProfile bool
	var bridgeExtensionID string
	var headless bool
	var loginMode bool
	var upstreamHTTP string
	var mcpToolProfile string
	var mcpIdleExit time.Duration
	var httpIdleExit time.Duration
	var printSystemPrompt bool
	var blockedDomains string
	var allowedDomains string
	var enableWebMCP bool
	var usageLog string
	var usageLogMaxMB int
	var usageLogBackups int
	var artifactDir string
	var artifactMaxMB int
	var artifactTotalMB int
	var artifactTTL time.Duration
	var artifactEncrypt string
	var artifactKeyFile string
	var stateRoot string
	var stateKeyFile string
	var baselineRoot string
	var artifactFailureBundles string
	var artifactFailureBundleTTL time.Duration
	var recipeRoot string
	var recipeProviderURL string
	var recipeProviderTokenFile string
	var pluginDir string
	var proxyServer string
	var proxyBypassList string
	var ignoreHTTPSErrors bool
	var caCertFile string
	var siteConsent bool
	var siteConsentConfig string
	var siteConsentPrompt bool
	var confirmActions bool
	var contentNavGuard bool
	var chromeOptIn bool
	var chromeOptInBrowser string
	var chromeOptInUserDataDir string
	var configPath string

	flag.StringVar(&httpAddr, "http", envDefault("BRW_HTTP_ADDR", "127.0.0.1:17310"), "HTTP listen address, or off. Defaults to loopback; bind a non-loopback address only behind SSH/Tailscale with caller auth.")
	flag.BoolVar(&mcpMode, "mcp", false, "run MCP stdio server")
	flag.StringVar(&mcpToolProfile, "mcp-tools", envDefault("BRW_MCP_TOOLS", "auto"), "MCP tool surface advertised in tools/list: 'all' (full), 'core' (lean common-flow set), 'minimal' (smallest surface that still completes ordinary web work), or 'auto' (default; starts minimal and grows as the agent discovers tools with brw_tools). The catalogue is re-sent on every request, so a narrower profile is a per-turn context saving. All tools remain callable regardless.")
	flag.DurationVar(&mcpIdleExit, "mcp-idle-exit", envDuration("BRW_MCP_IDLE_EXIT", 0), "exit the --mcp stdio server cleanly after this long with no requests; upstream HTTP proxies default to 90m unless this flag or BRW_MCP_IDLE_EXIT is explicitly set (0 disables). Prevents abandoned clients from accumulating disposable proxy processes.")
	flag.DurationVar(&httpIdleExit, "idle-exit", envDuration("BRW_IDLE_EXIT", 0), "shut this daemon down cleanly after this long with no use; 0 (the default) keeps it persistent. For a daemon started for one job — a scheduled run, a CI step — that would otherwise hold a browser and a loopback port until the machine reboots. Use means a request to the HTTP API, or an MCP tool call in --mcp mode; a /health poll does not, so a supervisor cannot keep an abandoned daemon alive. Requires the HTTP listener: with --http off, use --mcp-idle-exit instead.")
	flag.BoolVar(&bridgeMode, "bridge", false, "use installed Chrome extension bridge instead of direct CDP")
	flag.BoolVar(&headless, "headless", envBool("BRW_HEADLESS"), "direct CDP: launch Chrome with no visible window (--headless=new). Extensions, the persistent profile and the full CDP surface still work. Incompatible with --bridge and --remote, which attach to a browser brw did not launch. A profile may set \"headless\": true instead.")
	flag.StringVar(&bridgeAddr, "bridge-addr", envDefault("BRW_BRIDGE_ADDR", "127.0.0.1:17311"), "extension bridge WebSocket listen address")
	flag.BoolVar(&bridgeRaiseWindow, "bridge-raise-window", envBool("BRW_BRIDGE_RAISE_WINDOW"), "bridge: raise the Chrome window to the OS foreground on focus_tab. Off by default so automation never steals your focus while you work elsewhere.")
	flag.StringVar(&bridgeTabGroup, "bridge-tab-group", envDefault("BRW_BRIDGE_TAB_GROUP", "brw"), "bridge: tab-group title brw_open uses when no group is given, so the agent's tabs stay corralled in one labelled group. Set empty to disable default grouping.")
	flag.BoolVar(&bridgeFollowFocus, "bridge-follow-focus", envBool("BRW_BRIDGE_FOLLOW_FOCUS"), "bridge: follow the user's manually-focused Chrome tab for no-tab_id actions (legacy behavior). OFF by default: brw works in its own tab group on tabs it opened (opening a fresh one when needed) and never touches your existing tabs unless you pass tab_id. Turn on for an interactive session where you want brw to act on whatever tab you have selected.")
	flag.IntVar(&bridgeMaxInflight, "bridge-max-inflight", envInt("BRW_BRIDGE_MAX_INFLIGHT", 6), "bridge: max concurrent operations on the shared extension socket. Excess calls queue and, past the deadline, fail fast with a busy signal. Caps load on the single Chrome extension worker so many parallel agents can't wedge it. 0 disables the cap.")
	flag.StringVar(&upstreamHTTP, "upstream-http", os.Getenv("BRW_UPSTREAM_HTTP"), "proxy MCP/HTTP control to an existing local brw HTTP daemon")
	flag.StringVar(&cfg.RemoteURL, "remote", os.Getenv("BRW_REMOTE_URL"), "attach to an existing CDP endpoint, for example http://127.0.0.1:9222, or \"auto\" to find one: brw reads DevToolsActivePort in the user data directory (which is the only place an ephemeral port is written) and then tries the conventional loopback debugging ports, attaching only to something that answers /json/version as a browser.")
	flag.StringVar(&profileName, "profile", os.Getenv("BRW_PROFILE"), "workspace-allowed browser profile name")
	flag.StringVar(&workspaceName, "workspace", os.Getenv("BRW_WORKSPACE"), "workspace binding name for default/restricted profiles")
	flag.StringVar(&profilePolicyPath, "profile-policy", os.Getenv("BRW_PROFILE_POLICY"), "profile policy JSON path; defaults to standard brw config discovery")
	flag.BoolVar(&unsafeAllowDefaultProfileCDP, "unsafe-allow-default-profile-cdp", false, "diagnostic override for profiles marked direct_cdp_allowed=false")
	flag.BoolVar(&unsafeRealProfile, "unsafe-real-profile", envBool("BRW_UNSAFE_REAL_PROFILE"), "diagnostic override allowing a direct-CDP launch against the user's real browser profile dir. Dangerous: a second Chrome on a live profile corrupts it (lost logins, won't reopen).")
	flag.StringVar(&cfg.ChromePath, "chrome-path", os.Getenv("BRW_CHROME_PATH"), "Chrome/Chromium executable path")
	flag.StringVar(&cfg.UserDataDir, "user-data-dir", envDefault("BRW_USER_DATA_DIR", cdplaunch.DefaultProfileDir("")), "persistent Chrome user data directory")
	flag.StringVar(&cfg.ProfileDirectory, "profile-directory", os.Getenv("BRW_PROFILE_DIRECTORY"), "Chrome profile directory within user data dir, for example 'Profile 1'")
	flag.IntVar(&cfg.Port, "remote-debugging-port", 0, "remote debugging port for launched Chrome; 0 chooses a free local port")
	flag.Var(&extensions, "extension", "extension directory to load; repeatable")
	flag.BoolVar(&loginMode, "login", false, "direct CDP: force a headed window even on a headless profile, so you can sign in once. The profile keeps the session; later headless runs on the same --user-data-dir are still signed in. Stop this daemon before starting the headless one — one Chrome per profile directory.")
	flag.Var(&chromeArgs, "chrome-arg", "extra Chrome argument; repeatable")
	flag.DurationVar(&timeout, "timeout", 20*time.Second, "default browser operation timeout")
	flag.BoolVar(&printSystemPrompt, "print-system-prompt", false, "print the recommended agent system prompt to stdout and exit")
	flag.StringVar(&blockedDomains, "blocked-domains", os.Getenv("BRW_BLOCKED_DOMAINS"), "comma-separated domains the agent may never open (subdomains included); guardrail enforced on brw_open and brw_replay_request")
	flag.StringVar(&allowedDomains, "allowed-domains", os.Getenv("BRW_ALLOWED_DOMAINS"), "comma-separated allowlist; when set, the agent may ONLY open these domains (and subdomains)")
	flag.BoolVar(&enableWebMCP, "enable-webmcp", envBool("BRW_ENABLE_WEBMCP"), "expose a WebMCP runtime (navigator.modelContext) so cooperating sites can register page tools brw_page_tools/brw_call_page_tool can use")
	flag.StringVar(&usageLog, "usage-log", envDefault("BRW_USAGE_LOG", "auto"), "privacy-safe metadata usage ledger path; auto writes under the user config directory, off disables. Never records tool arguments, typed text, page content, URLs, headers, or response bodies.")
	flag.IntVar(&usageLogMaxMB, "usage-log-max-mb", envInt("BRW_USAGE_LOG_MAX_MB", 20), "rotate the usage ledger at this many MiB; 0 disables size rotation")
	flag.IntVar(&usageLogBackups, "usage-log-backups", envInt("BRW_USAGE_LOG_BACKUPS", 7), "number of rotated usage-ledger files to retain")
	flag.StringVar(&artifactDir, "artifact-dir", envDefault("BRW_ARTIFACT_DIR", "auto"), "browser-host artifact directory: auto uses the user cache outside source checkouts; off disables. Explicit paths must be absolute and outside any git checkout.")
	flag.IntVar(&artifactMaxMB, "artifact-max-mb", envInt("BRW_ARTIFACT_MAX_MB", 128), "maximum size of one browser artifact in MiB")
	flag.IntVar(&artifactTotalMB, "artifact-total-mb", envInt("BRW_ARTIFACT_TOTAL_MB", 2048), "maximum total browser artifact cache size in MiB")
	flag.DurationVar(&artifactTTL, "artifact-ttl", envDuration("BRW_ARTIFACT_TTL", 24*time.Hour), "maximum artifact retention; individual captures may request a shorter TTL")
	flag.StringVar(&artifactEncrypt, "artifact-encrypt", envDefault("BRW_ARTIFACT_ENCRYPT", "off"), "encrypt stored artifacts at rest: off (default), recipe (only captures made during a private-recipe run), or all. Anything but off requires --artifact-key-file.")
	flag.StringVar(&artifactKeyFile, "artifact-key-file", os.Getenv("BRW_ARTIFACT_KEY_FILE"), "0600 regular file holding at least 32 bytes of artifact encryption key material. Must live outside the artifact directory; never logged.")
	flag.StringVar(&artifactFailureBundles, "artifact-failure-bundles", envDefault("BRW_ARTIFACT_FAILURE_BUNDLES", "off"), "collect a failure evidence bundle (action trace, console summary, bounded network metadata, semantic snapshot, screenshot) when a recipe step fails: off (default), recipe (honour the recipe's capture_on_failure), or all. The failed call returns only the manifest artifact id.")
	flag.DurationVar(&artifactFailureBundleTTL, "artifact-failure-bundle-ttl", envDuration("BRW_ARTIFACT_FAILURE_BUNDLE_TTL", time.Hour), "retention for failure evidence artifacts, clamped to --artifact-ttl. Evidence is the most sensitive thing the store holds, so it expires sooner than an ordinary capture.")
	flag.StringVar(&stateRoot, "state-root", envDefault("BRW_STATE_ROOT", "auto"), "browser-host directory for brw_state session snapshots: auto uses the user cache, off disables. Snapshots never leave this host and are always encrypted, so this does nothing without --state-key-file.")
	flag.StringVar(&stateKeyFile, "state-key-file", os.Getenv("BRW_STATE_KEY_FILE"), "0600 regular file holding at least 32 bytes of key material for brw_state session snapshots. Must live outside the state root; never logged. Without it brw_state refuses to save rather than writing a signed-in session in the clear.")
	flag.StringVar(&baselineRoot, "baseline-root", envDefault("BRW_BASELINE_ROOT", "off"), "directory holding brw_baseline regression baselines: auto uses the user cache, off (default) disables. An explicit path must be absolute and outside any source checkout, because a baseline is a screenshot of a page and must never be committable.")
	flag.StringVar(&recipeRoot, "recipe-root", os.Getenv("BRW_RECIPE_ROOT"), "absolute 0700 directory containing private 0600 recipe JSON files; must live outside the brw source repository")
	flag.StringVar(&recipeProviderURL, "recipe-provider-url", os.Getenv("BRW_RECIPE_PROVIDER_URL"), "HTTPS private recipe-provider base URL (loopback HTTP allowed); use instead of --recipe-root")
	flag.StringVar(&recipeProviderTokenFile, "recipe-provider-token-file", os.Getenv("BRW_RECIPE_PROVIDER_TOKEN_FILE"), "0600 regular file containing the private recipe-provider bearer token; never logged")
	flag.StringVar(&pluginDir, "plugin-dir", os.Getenv("BRW_PLUGIN_DIR"), "directory of *.json plugin manifests. brw does not sandbox a plugin, so the directory and its manifests must not be group- or other-writable. Grants only the capabilities in docs/plugins.md: credential.read, which lets a recipe resolve a secret:// reference at execution, and browser.provider, which supplies a CDP websocket URL so brw drives a browser on another machine instead of launching one here. A loaded browser.provider plugin is incompatible with --bridge, --remote, --upstream-http, --login, --headless and a workspace profile; each is refused at startup rather than ignored.")
	flag.StringVar(&proxyServer, "proxy-server", os.Getenv("BRW_PROXY_SERVER"), "direct CDP: route the launched browser through this proxy, for example http://127.0.0.1:8080 or socks5://127.0.0.1:1080. Chrome takes a proxy only at launch, so this cannot be changed on a running browser.")
	flag.StringVar(&proxyBypassList, "proxy-bypass-list", os.Getenv("BRW_PROXY_BYPASS_LIST"), "direct CDP: semicolon-separated hosts that bypass --proxy-server and go direct, for example \"<local>;*.internal\". Requires --proxy-server.")
	flag.BoolVar(&ignoreHTTPSErrors, "ignore-https-errors", envBool("BRW_IGNORE_HTTPS_ERRORS"), "direct CDP: launch Chrome with certificate validation OFF for every site. Opt-in per launch and reported by brw_identity as ignore_https_errors, because an agent reading a page over this daemon otherwise cannot tell a valid site from an intercepted one. Prefer --ca-cert, which trusts one private CA instead of everything.")
	flag.StringVar(&caCertFile, "ca-cert", os.Getenv("BRW_CA_CERT"), "direct CDP: PEM bundle whose certificates' public keys Chrome should stop reporting errors for, so a private-CA site loads without turning validation off everywhere. It does NOT install the CA anywhere: nothing outside this browser instance is affected, and the connection remains an error Chrome was told to overlook rather than a validated one.")
	flag.BoolVar(&siteConsent, "site-consent", envBool("BRW_SITE_CONSENT"), "require a recorded per-origin grant before brw opens, reads or acts on a site. Grants live beside the profile policy, are MAC'd against a 0600 key so a local process cannot write itself one, and are listed/revoked with `brwctl grants`. Without a grant the daemon refuses and names the origin and the missing scope.")
	flag.StringVar(&siteConsentConfig, "site-consent-config", os.Getenv("BRW_SITE_CONSENT_CONFIG"), "admin consent config JSON (allowed_origins, blocked_origins, category_domains, confirm_actions, default_grant_ttl). Defaults to site-consent.json beside the profile policy, so a managed machine needs no UI.")
	flag.BoolVar(&siteConsentPrompt, "site-consent-prompt", envBool("BRW_SITE_CONSENT_PROMPT"), "ask on this terminal when an un-granted origin comes up, and record the answer. Requires a terminal on stdin and is incompatible with --mcp (which owns stdin); without it the daemon is non-interactive and refuses instead of asking.")
	flag.BoolVar(&confirmActions, "confirm-actions", envBool("BRW_CONFIRM_ACTIONS"), "require confirmation before a high-risk action (publishing, purchasing, submitting a form carrying personal data, anything on a blocklisted category). Fails CLOSED: with nobody to ask, the action is refused rather than approved. Requires --site-consent.")
	flag.BoolVar(&contentNavGuard, "content-nav-guard", envBool("BRW_CONTENT_NAV_GUARD"), "refuse a top-level navigation that page content initiated to another site (an injected link click, a meta refresh, a script location assignment). What the agent asked for still works: the destination it named, and the navigation its own click or keypress causes. Not available on the extension bridge or an upstream HTTP proxy.")
	flag.BoolVar(&chromeOptIn, "chrome-opt-in", envBool("BRW_CHROME_OPT_IN"), "attach to a Chrome 144+ instance whose user has turned on remote debugging at chrome://inspect/#remote-debugging. This is full browser-target CDP against the real signed-in profile, with none of the extension bridge's incognito or cookie limits and no extension at all. Downloads are reported but not routed, and brw_state is refused: brw will not move or seal what belongs to the browser's own user. A profile policy grants this lane with chrome_opt_in_allowed: true. brw never turns the opt-in on: it is a human action by design, and with it off the daemon says so and exits rather than launching a browser with a debugging flag. Chrome 144+ also asks you to approve each debugging connection in the browser window; brw waits two minutes for that and then exits naming the prompt, rather than hanging.")
	flag.StringVar(&chromeOptInBrowser, "chrome-opt-in-browser", envDefault("BRW_CHROME_OPT_IN_BROWSER", "chrome"), "which browser's user data directory --chrome-opt-in looks in for the endpoint (chrome, chromium, edge, brave, vivaldi)")
	flag.StringVar(&chromeOptInUserDataDir, "chrome-opt-in-user-data-dir", os.Getenv("BRW_CHROME_OPT_IN_USER_DATA_DIR"), "explicit user data directory for --chrome-opt-in, when the browser is not one brw knows the default path for. brw only reads from it.")
	flag.StringVar(&configPath, "config", os.Getenv("BRW_CONFIG"), "brw.json holding this machine's daemon defaults, with optional per-profile sections. Defaults to brw.json in the user config directory, which is read when it exists and ignored when it does not. It is the weakest source: a flag on the command line wins, then the environment, then the file.")
	flag.Parse()

	// brw.json is applied after Parse and before anything reads a flag, and it
	// only fills in what the command line and the environment left alone. The
	// set of flags the operator actually typed is what makes that possible:
	// brwd reads its environment as each flag's default, so a flag's value alone
	// cannot say whether anybody chose it.
	if config, configFile, err := brwconfig.Load(configPath); err != nil {
		log.Fatalf("config: %v", err)
	} else if config != nil {
		applied, err := config.Apply(flag.CommandLine, profileName, flagsSetOnCommandLine(), os.LookupEnv)
		if err != nil {
			log.Fatalf("config %s: %v", configFile, err)
		}
		if len(applied) > 0 {
			settings := make([]string, 0, len(applied))
			for _, setting := range applied {
				settings = append(settings, setting.String())
			}
			log.Printf("config %s supplied %s", configFile, strings.Join(settings, ", "))
		}
	}

	// Whether the operator chose the stdio idle exit themselves, as opposed to
	// inheriting the disposable-proxy default. Both this and the --http off
	// fallback below have to tell those apart, or a duration nobody typed
	// silently outranks one they did.
	mcpIdleExitTyped := flagWasSet("mcp-idle-exit") || strings.TrimSpace(os.Getenv("BRW_MCP_IDLE_EXIT")) != ""
	mcpIdleExit = effectiveMCPIdleExit(mcpIdleExit, mcpMode, upstreamHTTP, mcpIdleExitTyped)

	if printSystemPrompt {
		fmt.Println(mcp.AgentSystemPrompt)
		return
	}

	cfg.Extensions = extensions
	cfg.ChromeArgs = chromeArgs
	cfg.Timeout = timeout
	cfg.WebMCP = enableWebMCP
	cfg.Network = cdplaunch.NetworkEnvironment{
		ProxyServer:       proxyServer,
		ProxyBypassList:   proxyBypassList,
		IgnoreHTTPSErrors: ignoreHTTPSErrors,
	}
	if caCertFile != "" {
		bundle, err := os.ReadFile(caCertFile)
		if err != nil {
			log.Fatalf("read --ca-cert %s: %v", caCertFile, err)
		}
		fingerprints, err := cdplaunch.SPKIFingerprintsFromPEM(bundle)
		if err != nil {
			log.Fatalf("--ca-cert %s: %v", caCertFile, err)
		}
		cfg.Network.TrustedSPKI = fingerprints
	}
	if err := cfg.Network.Validate(); err != nil {
		log.Fatalf("launch network settings: %v", err)
	}
	if ignoreHTTPSErrors {
		log.Printf("WARNING: --ignore-https-errors is active; this browser accepts ANY certificate, so an intercepted or impostor site is indistinguishable from a real one")
	}
	cfg.AllowRealProfile = unsafeRealProfile
	if unsafeRealProfile {
		log.Printf("WARNING: --unsafe-real-profile is active; brw may launch Chrome against your real browser profile, which can corrupt it (lost logins, won't reopen)")
	}
	// Loaded before anything can call it, and before the transport is chosen: a
	// plugin holding browser.provider decides WHICH browser this daemon drives,
	// so every mode check below has to be able to see it. A misconfigured plugin
	// directory stays a startup failure rather than a daemon that silently holds
	// no provider and fails a login three steps into a recipe.
	plugins, err := plugin.Load(pluginDir)
	if err != nil {
		log.Fatalf("plugin directory: %v", err)
	}
	for _, status := range plugins.Plugins() {
		log.Printf("plugin %s %s loaded with capabilities %v", status.ID, status.Version, status.Capabilities)
	}
	useBrowserProvider := plugins.ProbeBrowserProvider() == nil
	if useBrowserProvider {
		if err := refuseWithProvider(providerLaunch{
			Bridge:       bridgeMode,
			UpstreamHTTP: upstreamHTTP,
			ChromeOptIn:  chromeOptIn,
			Login:        loginMode,
			Headless:     headless,
			Profile:      profileName,
			Workspace:    workspaceName,
			Config:       cfg,
		}); err != nil {
			log.Fatalf("%v", err)
		}
		if explicitProfileFlags() {
			log.Fatalf("--user-data-dir/--profile-directory with a browser.provider plugin: %v", browser.RemoteUnavailableError("profile_reuse"))
		}
		// Cleared rather than left at their flag defaults, so browser.New sees
		// the configuration this daemon actually has. The explicit spellings
		// were already refused above; what is left is the default profile dir,
		// which describes a machine the browser is not on.
		cfg.UserDataDir = ""
		cfg.ProfileDirectory = ""
	}

	// The profile is resolved before the opt-in block, and only resolved: this
	// lane has to be gated by policy and pointed at the profile's own browser
	// and directory, and both need the profile in hand before discovery runs.
	// Everything the policy CHANGES is still applied below.
	var profile profilepolicy.Profile
	haveProfilePolicy := false
	// The policy's direct-CDP verdict, kept outside the block that resolves it
	// because --remote auto is resolved further down and has to honour it. No
	// policy means no prohibition, which is what a daemon started without
	// --profile has always had.
	policyProfileName := ""
	directCDPAllowed := true

	if profileName != "" || workspaceName != "" {
		policy, err := profilepolicy.Load(profilePolicyPath)
		if err != nil {
			log.Fatalf("load profile policy: %v", err)
		}
		resolved, err := policy.ResolveProfile(workspaceName, profileName)
		if err != nil {
			log.Fatalf("profile policy: %v", err)
		}
		profile = resolved
		haveProfilePolicy = true
	}

	var chromeOptInEndpoint chromeoptin.Endpoint
	if chromeOptIn {
		if err := checkChromeOptInFlags(chromeOptInFlags{
			Bridge:                  bridgeMode,
			RemoteURL:               cfg.RemoteURL,
			UpstreamHTTP:            upstreamHTTP,
			Headless:                headless,
			Login:                   loginMode,
			LaunchNetworking:        !cfg.Network.Empty(),
			Extensions:              len(extensions),
			ChromeArgs:              len(chromeArgs),
			ChromePath:              cfg.ChromePath,
			Port:                    cfg.Port,
			UserDataDirSet:          flagWasSet("user-data-dir") || os.Getenv("BRW_USER_DATA_DIR") != "",
			ProfileDirectorySet:     flagWasSet("profile-directory") || os.Getenv("BRW_PROFILE_DIRECTORY") != "",
			UnsafeRealProfile:       unsafeRealProfile,
			UnsafeDefaultProfileCDP: unsafeAllowDefaultProfileCDP,
		}); err != nil {
			log.Fatal(err)
		}
		browserKind := chromeOptInBrowserKind(
			chromeOptInBrowser,
			flagWasSet("chrome-opt-in-browser") || os.Getenv("BRW_CHROME_OPT_IN_BROWSER") != "",
			profile.Kind,
		)
		dir := chromeOptInDiscoveryDir(chromeOptInUserDataDir, profile.UserDataDir, runtime.GOOS, browserKind)
		optInCfg, endpoint, err := configureChromeOptIn(context.Background(), cfg, chromeOptInRequest{
			UserDataDir: dir,
			Profile:     profile,
			HavePolicy:  haveProfilePolicy,
		})
		if err != nil {
			// Fatal, and deliberately not a fallback: the alternative to a
			// missing opt-in endpoint is brw starting a browser with a
			// debugging flag, which is the access Chrome asks a human to grant.
			log.Fatalf("--chrome-opt-in: %v", err)
		}
		cfg = optInCfg
		chromeOptInEndpoint = endpoint
		log.Printf("chrome opt-in endpoint discovered: %s (%s, user data dir %s)", endpoint.HTTPURL, endpoint.BrowserLabel(), endpoint.UserDataDir)
	}
	runtimeIdentity := brwidentity.Identity{}
	identityExpected := brwidentity.Identity{}

	if haveProfilePolicy {
		policyProfileName = profile.Name
		directCDPAllowed = profile.DirectCDPAllowed
		if profile.BridgeHTTPAddr != "" && !flagWasSet("http") && os.Getenv("BRW_HTTP_ADDR") == "" {
			httpAddr = profile.BridgeHTTPAddr
		}
		if profile.BridgeWSAddr != "" && !flagWasSet("bridge-addr") && os.Getenv("BRW_BRIDGE_ADDR") == "" {
			bridgeAddr = profile.BridgeWSAddr
		}
		if upstreamHTTP != "" {
			if !profile.ExtensionBridgeAllowed && !profile.DirectCDPAllowed {
				log.Fatalf("profile %q is not allowed for upstream HTTP control by workspace policy", profile.Name)
			}
		} else if bridgeMode {
			if !profile.ExtensionBridgeAllowed {
				log.Fatalf("profile %q is not allowed through extension bridge by workspace policy", profile.Name)
			}
			bridgeExtensionID = profile.BridgeExtensionID
			if bridgeExtensionID == "" {
				bridgeExtensionID = profilepolicy.DefaultBridgeExtensionID
			}
		} else if !profile.DirectCDPAllowed && cfg.RemoteURL == "" && !unsafeAllowDefaultProfileCDP {
			log.Fatalf("profile %q is allowed only through extension bridge, not direct CDP launch; use a direct-CDP profile, --remote, or --unsafe-allow-default-profile-cdp for diagnostics", profile.Name)
		}
		if unsafeAllowDefaultProfileCDP {
			log.Printf("WARNING: --unsafe-allow-default-profile-cdp is active; profile policy bypass is enabled for diagnostics")
		}
		if !chromeOptIn {
			// Not on the opt-in lane: there the directory is the browser's own,
			// the Manager writes under whatever it is given, and the daemon
			// attaches to a running browser rather than choosing a profile
			// inside it.
			cfg.UserDataDir = profile.UserDataDir
			cfg.ProfileDirectory = profile.ProfileDirectory
		}
		if profile.Headless {
			headless = true
		}
		mode := daemonMode(upstreamHTTP, cfg.RemoteURL, bridgeMode, chromeOptIn, useBrowserProvider)
		runtimeIdentity = brwidentity.Identity{
			Workspace:         workspaceName,
			Profile:           profile.Name,
			UserDataDir:       profile.UserDataDir,
			ProfileDirectory:  profile.ProfileDirectory,
			Mode:              mode,
			Transport:         localTransport(upstreamHTTP, cfg.RemoteURL, bridgeMode, chromeOptIn, useBrowserProvider),
			Headless:          headless,
			IgnoreHTTPSErrors: ignoreHTTPSErrors,
		}
		identityExpected = runtimeIdentity
		// Mode, Transport, Headless and the certificate policy are all properties
		// of the daemon answering, not of the workspace/profile binding being
		// verified. A proxy learns the last three from its upstream rather than
		// asserting them, so pinning them here would reject every healthy upstream.
		identityExpected.Mode = ""
		identityExpected.Transport = ""
		identityExpected.Headless = false
		identityExpected.IgnoreHTTPSErrors = false
		log.Printf("using workspace profile %q (%s)", profile.Name, profile.Kind)
	}
	// The profile policy above is the last thing that can change httpAddr, so
	// this is where the answer is final.
	resolvedMCPIdleExit, idleExitNote := resolveIdleExit(httpIdleExit, mcpIdleExit, httpAddr, mcpMode, mcpIdleExitTyped)
	mcpIdleExit = resolvedMCPIdleExit
	if idleExitNote != "" {
		log.Printf("%s", idleExitNote)
	}
	if loginMode {
		switch {
		case bridgeMode, upstreamHTTP != "", cfg.RemoteURL != "":
			log.Fatalf("--login only applies to a direct-CDP profile brw launches itself; the bridge, --remote and --upstream-http all attach to a browser you sign into directly")
		}
		if headless {
			log.Printf("--login: launching headed despite the profile's headless setting; sign in, then stop this daemon before starting the headless one")
			headless = false
		}
	}
	if headless {
		switch {
		case bridgeMode:
			log.Fatalf("--headless cannot be combined with --bridge: the bridge drives the browser you are already running, so there is no window for brw to suppress")
		case chromeOptIn:
			// Reachable through a profile's "headless": true, since the flag
			// itself is already refused alongside --chrome-opt-in. Named
			// separately so the operator is not sent looking at --remote.
			log.Fatalf("--chrome-opt-in cannot run headless: it attaches to the Chrome you turned remote debugging on in, which is the window you are looking at; drop \"headless\": true from the profile or use a direct-CDP profile")
		case cfg.RemoteURL != "":
			log.Fatalf("--headless cannot be combined with --remote: brw attaches to a browser it did not launch, so headlessness was decided by whoever started it")
		case upstreamHTTP != "":
			log.Fatalf("--headless cannot be combined with --upstream-http: this process proxies to a daemon that already launched the browser; set --headless on that daemon")
		}
	}
	if !cfg.Network.Empty() {
		switch {
		case bridgeMode:
			log.Fatalf("--proxy-server, --ignore-https-errors and --ca-cert cannot be combined with --bridge: they are Chrome launch switches, and the bridge drives a browser you started yourself")
		case cfg.RemoteURL != "":
			log.Fatalf("--proxy-server, --ignore-https-errors and --ca-cert cannot be combined with --remote: brw attaches to a browser it did not launch, so its proxy and certificate policy were fixed by whoever started it")
		case upstreamHTTP != "":
			log.Fatalf("--proxy-server, --ignore-https-errors and --ca-cert cannot be combined with --upstream-http: set them on the daemon that launches the browser")
		}
	}
	// --remote auto is resolved here, after the profile policy has decided which
	// user data directory this daemon is for: the browser's own
	// DevToolsActivePort lives in that directory, and it is the only place an
	// ephemeral debugging port is ever written down.
	if cdplaunch.IsAutoConnect(cfg.RemoteURL) {
		if bridgeMode || upstreamHTTP != "" {
			// Named rather than quietly ignored: neither of these modes uses a
			// CDP endpoint, so discovering one would probe the machine's ports
			// and then throw the answer away.
			log.Fatalf("--remote auto cannot be combined with --bridge or --upstream-http: neither drives the browser over a CDP endpoint, so there is nothing to attach")
		}
		discoverCtx, cancelDiscover := context.WithTimeout(context.Background(), 15*time.Second)
		endpoint, err := cdplaunch.AutoConnect(discoverCtx, cdplaunch.AutoConnectOptions{
			UserDataDirs: autoConnectSearchDirs(cfg.UserDataDir),
		})
		cancelDiscover()
		if err != nil {
			log.Fatalf("--remote auto: %v", err)
		}
		if err := autoConnectRefusal(endpoint, policyProfileName, cfg.UserDataDir, directCDPAllowed, unsafeAllowDefaultProfileCDP); err != nil {
			log.Fatalf("--remote auto: %v", err)
		}
		cfg.RemoteURL = endpoint.URL
		log.Printf("--remote auto attached to %s (%s) found by %s", endpoint.URL, endpoint.Browser, endpoint.Source)
	}
	cfg.Headless = headless
	usageIdentity := runtimeIdentity
	if usageIdentity.Mode == "" {
		usageIdentity.Mode = daemonMode(upstreamHTTP, cfg.RemoteURL, bridgeMode, chromeOptIn, useBrowserProvider)
	}
	if usageIdentity.Transport == "" {
		usageIdentity.Transport = localTransport(upstreamHTTP, cfg.RemoteURL, bridgeMode, chromeOptIn, useBrowserProvider)
	}
	usageIdentity.Headless = headless
	usageIdentity.IgnoreHTTPSErrors = ignoreHTTPSErrors
	// Report the directory the endpoint was discovered in, always: it is the
	// profile this daemon is driving, and a policy value that disagreed was
	// already refused above. It goes in the identity only — the Manager never
	// receives it, because that directory is the browser's, not brw's.
	if chromeOptIn && chromeOptInEndpoint.UserDataDir != "" {
		usageIdentity.UserDataDir = chromeOptInEndpoint.UserDataDir
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The long-lived daemon is the canonical usage-ledger writer. Disposable
	// --upstream-http MCP proxies forward correlation headers and are deliberately
	// not additional writers, avoiding duplicate records and cross-process
	// rotation races. The upstream daemon records every browser operation it
	// actually receives.
	var usage *usagelog.Recorder
	if upstreamHTTP == "" {
		maxBytes, err := usageLogMaxBytes(usageLogMaxMB)
		if err != nil {
			log.Fatalf("usage log: %v", err)
		}
		if usageLogBackups < 0 {
			log.Fatalf("usage log: usage-log-backups must be non-negative")
		}
		usagePath, pathErr := resolveUsageLogPath(usageLog, usageIdentity)
		if pathErr != nil {
			// Observability must never become a new browser-control outage.
			log.Printf("WARNING: usage ledger disabled: %v", pathErr)
		} else if usagePath != "" {
			usage, err = usagelog.New(usagelog.Config{
				Path: usagePath, MaxBytes: maxBytes, Backups: usageLogBackups,
				Version: mcp.Version, Identity: usageIdentity,
			})
			if err != nil {
				log.Printf("WARNING: usage ledger disabled: %v", err)
			}
		}
		if usage != nil {
			defer func() {
				if err := usage.Close(); err != nil {
					log.Printf("close usage log: %v", err)
				}
			}()
			log.Printf("privacy-safe usage ledger enabled at %s (metadata only; no arguments, content, or URLs)", usage.Path())
		}
	}

	// gracefulShutdown drains a server with a bounded timeout. Registered as a
	// defer so it runs on EVERY exit path, including the MCP-mode early return —
	// the previous trailing shutdown block was dead code in MCP mode (the most
	// common mode), so --mcp --bridge dropped the extension connection abruptly.
	gracefulShutdown := func(name string, fn func(context.Context) error) {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := fn(shutdownCtx); err != nil && err != http.ErrServerClosed {
			log.Printf("%s shutdown: %v", name, err)
		}
	}

	var controller browser.Controller
	var bridge *extensionbridge.Bridge
	var manager *browser.Manager
	// bridgeHandshakeToken is empty on every mode but the extension bridge. The
	// file it would live in is dealt with after the branch either way.
	var bridgeHandshakeToken string

	if upstreamHTTP != "" {
		upstream, err := httpclient.New(upstreamHTTP, timeout)
		if err != nil {
			log.Fatalf("upstream HTTP controller: %v", err)
		}
		verifyCtx, cancel := context.WithTimeout(context.Background(), timeout)
		health, healthErr := upstream.Health(verifyCtx)
		cancel()
		if haveProfilePolicy {
			if healthErr != nil {
				log.Fatalf("verify upstream identity: %v", healthErr)
			}
			if health.Identity.Empty() {
				log.Fatalf("upstream HTTP controller %s does not expose workspace/profile identity; refusing to proxy workspace %q profile %q", upstreamHTTP, identityExpected.Workspace, identityExpected.Profile)
			}
			if mismatches := health.Identity.Mismatches(identityExpected); len(mismatches) > 0 {
				log.Fatalf("upstream HTTP controller %s identity mismatch: %s", upstreamHTTP, strings.Join(mismatches, "; "))
			}
		}
		// Without adoption every bridge daemon looked identical to every
		// direct-CDP one from inside a tool call, and the documented advice was
		// to shell out and grep ps for --bridge.
		//
		// usageIdentity is the one that is read in this mode: it is what /health
		// serves and what the run lock is keyed on. runtimeIdentity feeds the
		// artifact root, the session-state store and the bridge, none of which
		// this process builds while it proxies — it is adopted anyway so the two
		// never disagree about the browser behind them.
		if healthErr == nil {
			runtimeIdentity = adoptUpstreamIdentity(runtimeIdentity, health.Identity, haveProfilePolicy)
			usageIdentity = adoptUpstreamIdentity(usageIdentity, health.Identity, haveProfilePolicy)
		} else {
			log.Printf("WARNING: upstream %s health unavailable (%v); transport, headless state and the profile this proxy drives will be reported empty", upstreamHTTP, healthErr)
		}
		controller = upstream
		log.Printf("using upstream HTTP controller %s", upstreamHTTP)
	} else if bridgeMode {
		bridge = extensionbridge.NewWithIdentity(bridgeAddr, timeout, bridgeExtensionID, runtimeIdentity)
		bridge.SetUsageRecorder(usage)
		// Seamless defaults: never raise the Chrome window on focus (no focus
		// theft) and corral the agent's tabs into one labelled group.
		bridge.SetRaiseWindowOnFocus(bridgeRaiseWindow)
		bridge.SetDefaultGroup(bridgeTabGroup)
		// Isolation by default: work in brw's own tab group on tabs it opened,
		// never the user's focused/existing tabs. --bridge-follow-focus restores
		// the legacy follow-the-user's-tab behavior.
		bridge.SetFollowFocus(bridgeFollowFocus)
		// Cap concurrent ops on the single shared extension socket so a fan-out of
		// parallel agents queues cleanly instead of flooding the MV3 worker until it
		// stops responding (the high-throughput "bridge becomes unresponsive" mode).
		bridge.SetMaxInflight(bridgeMaxInflight)
		// Provision a per-launch handshake secret so the real extension can prove
		// itself: the daemon serves it over the loopback /status endpoint (a web
		// page cannot read it cross-origin) and the 0.2.0+ extension presents it.
		//
		// Required by default. The grace period was for extensions older than
		// 0.2.0; the bundled extension is far past that and `brwctl setup` installs
		// it, so the only remaining effect of accepting a tokenless hello was that
		// every default install authenticated nothing. The Origin check rejects web
		// pages but not a local process, which can forge that header — so tokenless
		// meant any process running as this user could take the bridge and drive
		// the signed-in browser. BRW_BRIDGE_ALLOW_TOKENLESS=1 restores the old
		// behaviour for anyone genuinely pinned to a pre-0.2.0 extension.
		token, err := extensionbridge.NewAuthToken()
		if err != nil {
			log.Fatalf("generate extension bridge auth token: %v", err)
		}
		bridge.SetAuthToken(token)
		bridge.SetRequireToken(bridgeRequireToken())
		bridgeHandshakeToken = token
		controller = bridge
		defer gracefulShutdown("extension bridge", bridge.Shutdown)
		go func() {
			if bridgeExtensionID != "" {
				log.Printf("extension bridge listening on %s for extension %s", bridgeAddr, bridgeExtensionID)
			} else {
				log.Printf("extension bridge listening on %s", bridgeAddr)
			}
			if err := bridge.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("extension bridge stopped: %v", err)
				stop()
			}
		}()
	} else {
		var err error
		if useBrowserProvider {
			remote, release, openErr := openProviderBrowser(ctx, plugins)
			if openErr != nil {
				log.Fatalf("%v", openErr)
			}
			cfg.Remote = remote
			manager, err = browser.New(ctx, cfg)
			if err != nil {
				// Give the browser back before dying. Manager.Close is what
				// normally releases it, and there is no manager on this path, so
				// without this the provider keeps billing for a session nothing
				// will ever connect to.
				releaseCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if releaseErr := release(releaseCtx); releaseErr != nil {
					log.Printf("release the plugin-supplied browser session: %v", releaseErr)
				}
				cancel()
			}
		} else {
			manager, err = browser.New(ctx, cfg)
		}
		if err != nil {
			log.Fatalf("start browser: %v", err)
		}
		controller = manager
		defer func() {
			if err := manager.Close(); err != nil {
				log.Printf("close browser: %v", err)
			}
		}()
	}

	// Whatever mode this launch chose, deal with the handshake token on disk. It
	// is deliberately outside the branch above: only the bridge mints a token,
	// but a machine that upgrades and then runs direct-CDP or upstream-proxy
	// still has the file the last bridge launch left in ~/.brw, and a launch is
	// the only pass that will ever collect it. docs/auth-model.md says every
	// launch sweeps, and one call site on the path every launch takes is what
	// makes that true rather than aspirational.
	if err := bridgeTokenAtLaunch(bridgeTokenFile(workspaceName), bridgeHandshakeToken); err != nil {
		log.Printf("note: %v", err)
	}

	// Parse the navigation guardrail once and apply it to EVERY agent-facing
	// surface. Both the MCP server and the HTTP API share the same controller,
	// so a policy installed on only one of them is a silent bypass via the other.
	navPolicy := navpolicy.Parse(allowedDomains, blockedDomains)
	if !navPolicy.Empty() {
		log.Printf("navigation guardrail active (allow=%d, block=%d domains)", len(navPolicy.Allowed), len(navPolicy.Blocked))
	}
	if manager != nil {
		manager.SetNavigationPolicy(navPolicy)
	}
	if bridge != nil {
		bridge.SetNavigationPolicy(navPolicy)
	}

	if contentNavGuard {
		if manager == nil {
			// Named, not silently ignored. The boundary is implemented on the
			// direct-CDP transport's request interception; the extension bridge
			// has no equivalent, and a flag that quietly does nothing is worse
			// than one that refuses.
			log.Fatalf("--content-nav-guard needs a DevTools Protocol transport: it is enforced on CDP request interception, which the extension bridge and the upstream HTTP proxy do not have")
		}
		manager.SetContentNavigationGuard(true)
		log.Printf("content navigation boundary active: a page-initiated top-level navigation to another site is refused")
	}

	consentGuard, err := buildSiteConsent(siteConsentOptions{
		enabled:    siteConsent,
		configPath: siteConsentConfig,
		policyPath: profilePolicyPath,
		prompt:     siteConsentPrompt,
		mcpMode:    mcpMode,
		confirm:    confirmActions,
	})
	if err != nil {
		log.Fatalf("site consent: %v", err)
	}
	if bridge != nil {
		// The extension's options page is the only consent surface a user of the
		// signed-in browser has, and it reaches the daemon through the bridge.
		bridge.SetSiteConsent(consentGuard)
	}

	var artifactAPI artifact.API
	var recipeAPI recipe.API
	// recipeBaselines is the provider's baseline side when it has one. It is
	// declared out here because the provider is configured inside the
	// browser-host branch below and read again when the MCP server is built.
	var recipeBaselines recipe.BaselineStore
	// baselineRouter answers where a capture belongs. On a browser host it is
	// recipeBaselines; on a proxy it is the upstream hop, which can ask the
	// question without being able to store the answer.
	var baselineRouter recipe.BaselineRouter
	if upstreamHTTP != "" {
		// The proxy controller implements both optional APIs and forwards them to
		// the canonical browser host. Never create a second cache/provider here.
		artifactAPI, _ = controller.(artifact.API)
		recipeAPI, _ = controller.(recipe.API)
		// brw_baseline runs on THIS daemon (it has no HTTP route), while the
		// private recipe provider lives upstream. Without the routing hop every
		// capture here would be written to this process's --baseline-root,
		// including captures of pages the provider's recipes reach.
		baselineRouter, _ = controller.(recipe.BaselineRouter)
		if strings.TrimSpace(recipeRoot) != "" || strings.TrimSpace(recipeProviderURL) != "" || strings.TrimSpace(recipeProviderTokenFile) != "" {
			log.Printf("WARNING: recipe provider flags are ignored in --upstream-http mode; configure them on the browser-host daemon")
		}
		if strings.TrimSpace(pluginDir) != "" {
			// Loaded and listed, because plugin.Load ran above and the registry is
			// handed to the HTTP routes. What never happens in this mode is the
			// wiring into recipe.Runner.Credentials, which is on the else branch:
			// the recipe runner lives on the browser host, so that is where a
			// credential is resolved.
			log.Printf("WARNING: --plugin-dir manifests are loaded and listed by /api/plugins in --upstream-http mode, but never consulted; the recipe runner that resolves a credential lives on the browser-host daemon, so configure it there")
		}
	} else {
		if !strings.EqualFold(strings.TrimSpace(artifactDir), "off") {
			root := strings.TrimSpace(artifactDir)
			if root == "" || strings.EqualFold(root, "auto") {
				var err error
				root, err = defaultArtifactRoot(runtimeIdentity)
				if err != nil {
					log.Fatalf("artifact store: %v", err)
				}
			}
			maxArtifactBytes, err := mebibytes(artifactMaxMB)
			if err != nil {
				log.Fatalf("artifact store: %v", err)
			}
			maxTotalBytes, err := mebibytes(artifactTotalMB)
			if err != nil {
				log.Fatalf("artifact store: %v", err)
			}
			encryptionPolicy, err := artifact.ParseEncryptionPolicy(artifactEncrypt)
			if err != nil {
				log.Fatalf("artifact store: %v", err)
			}
			var encryptionKey []byte
			if strings.TrimSpace(artifactKeyFile) != "" {
				encryptionKey, err = artifact.LoadEncryptionKey(artifactKeyFile, root)
				if err != nil {
					log.Fatalf("artifact encryption key: %v", err)
				}
			}
			store, err := artifact.NewStore(artifact.Config{
				Root: root, MaxArtifactBytes: maxArtifactBytes,
				MaxTotalBytes: maxTotalBytes, TTL: artifactTTL,
				EncryptionKey: encryptionKey,
			})
			if err != nil {
				log.Fatalf("artifact store: %v", err)
			}
			service, err := artifact.NewService(store, controller)
			if err != nil {
				log.Fatalf("artifact service: %v", err)
			}
			if err := service.SetEncryptionPolicy(encryptionPolicy); err != nil {
				log.Fatalf("artifact encryption: %v", err)
			}
			failurePolicy, err := artifact.ParseFailureCapturePolicy(artifactFailureBundles)
			if err != nil {
				log.Fatalf("artifact failure bundles: %v", err)
			}
			if err := service.SetFailureCapturePolicy(failurePolicy); err != nil {
				log.Fatalf("artifact failure bundles: %v", err)
			}
			if err := service.SetFailureBundleTTL(artifactFailureBundleTTL); err != nil {
				log.Fatalf("artifact failure bundles: %v", err)
			}
			artifactAPI = service
			janitorInterval := min(5*time.Minute, max(time.Second, artifactTTL/2))
			go store.RunJanitor(ctx, janitorInterval, func(err error) {
				log.Printf("artifact retention janitor: %v", err)
			})
			log.Printf("browser-host artifact store enabled at %s (max=%d MiB, total=%d MiB, ttl=%s, encryption=%s, failure-bundles=%s)",
				store.Root(), artifactMaxMB, artifactTotalMB, artifactTTL, encryptionPolicy, failurePolicy)
		}

		if manager != nil && !strings.EqualFold(strings.TrimSpace(stateRoot), "off") {
			store, err := configureSessionStateStore(stateRoot, stateKeyFile, runtimeIdentity)
			if err != nil {
				log.Fatalf("session state store: %v", err)
			}
			if store != nil {
				manager.SetSessionStateStore(store)
				// The janitor is what makes the TTL a property of the disk rather
				// than of whoever happens to call brw_state next.
				go runSessionStateJanitor(ctx, store)
				log.Printf("browser-host session snapshots enabled at %s (ttl=%s, encrypted at rest)", store.Root(), store.TTL())
			}
		}
		provider, err := configureRecipeProvider(ctx, recipeRoot, recipeProviderURL, recipeProviderTokenFile)
		if err != nil {
			log.Fatalf("recipe provider: %v", err)
		}
		if provider != nil {
			runner := recipe.Runner{
				Surface:     &recipe.BrowserSurface{Browser: controller, Artifacts: artifactAPI},
				Credentials: plugins,
				Receipts:    recipeReceiptsFor(provider),
			}
			service, err := recipe.NewService(provider, runner)
			if err != nil {
				log.Fatalf("recipe service: %v", err)
			}
			recipeAPI = service
			recipeBaselines = recipeBaselinesFor(provider)
			baselineRouter = recipeBaselines
			log.Printf("private recipe provider enabled (recipe bodies and inputs are never written to usage logs)")
			log.Printf("%s", recipeReceiptStatusLine(runner.Receipts))
			log.Printf("%s", recipeBaselineStatusLine(recipeBaselines))
		}
	}

	var api *httpapi.Server
	if httpAddr != "" && httpAddr != "off" {
		// usageIdentity, not runtimeIdentity: the resolved one carries Mode,
		// Transport and Headless, which this process derives from its own flags
		// and always knows. runtimeIdentity is only populated by a profile
		// policy, so a daemon started without one reported an Empty() identity
		// and /health omitted the block entirely — including the transport. Two
		// surfaces on one daemon then disagreed about what they were driving,
		// and a caller gating on transport silently got no answer.
		api = httpapi.NewWithIdentity(httpAddr, controller, usageIdentity)
		// /api/skill serves this binary's own copy of the agent manual, and the
		// version is what lets a caller tell it apart from the copy on disk.
		api.SetVersion(mcp.Version)
		api.SetNavigationPolicy(navPolicy)
		api.SetSiteConsent(consentGuard)
		api.SetUsageRecorder(usage)
		api.SetArtifactAPI(artifactAPI)
		api.SetRecipeAPI(recipeAPI)
		// Only on the browser host: recipeBaselines is nil in proxy mode, and a
		// proxy answering the routing question for another proxy would answer
		// "nobody owns this" for a provider it cannot see.
		if recipeBaselines != nil {
			api.SetBaselineRouter(recipeBaselines)
		}
		api.SetPluginRegistry(plugins)
		if httpIdleExit > 0 {
			api.SetIdleExit(httpIdleExit)
			log.Printf("HTTP idle-exit armed: shutting down after %s with no API request", httpIdleExit)
			go func() {
				if api.WatchIdle(ctx) {
					log.Printf("no API request for %s; shutting down", httpIdleExit)
					// stop(), not os.Exit: the deferred browser close is the
					// only thing that detaches the debugger and tears Chrome
					// down, and an idle exit that orphaned a browser would be
					// worse than staying up.
					stop()
				}
			}()
		}
		defer gracefulShutdown("HTTP API", api.Shutdown)
		if !isLoopback(httpAddr) {
			log.Printf("WARNING: HTTP API bound to non-loopback address %s; no authentication is enforced — ensure caller auth is in place (SSH/Tailscale)", httpAddr)
		}
		go func() {
			log.Printf("HTTP API listening on %s", httpAddr)
			if err := api.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				// Fail loudly, but via stop() (cancel the root ctx) rather than
				// log.Fatalf: os.Exit would skip the deferred manager.Close(), which
				// is the only thing that detaches the CDP debugger and tears Chrome
				// down — a port clash would otherwise orphan the just-launched
				// browser. Mirrors the extension-bridge goroutine below/above.
				log.Printf("HTTP API failed on %s: %v", httpAddr, err)
				stop()
			}
		}()
	}

	if mcpMode {
		// An unrecognised profile still serves the full surface, but silently
		// doing so would hide a config typo that was meant to shrink the
		// catalogue — say so on the way past.
		if !mcp.ValidToolProfile(mcpToolProfile) {
			log.Printf("unknown --mcp-tools %q; advertising the full surface (valid: %s)",
				mcpToolProfile, strings.Join(mcp.ToolProfileNames(), ", "))
		}
		log.Printf("MCP stdio server ready (tool profile: %s)", mcpToolProfile)
		// A stdio MCP child's lifetime is its session's lifetime. Watch for
		// orphaning (parent died without closing our stdin) so we never
		// outlive an abandoned session.
		go watchParentExit(stop)
		server := mcp.NewWithToolProfile(controller, mcpToolProfile)
		server.SetNavigationPolicy(navPolicy)
		server.SetSiteConsent(consentGuard)
		server.SetArtifactAPI(artifactAPI)
		server.SetRecipeAPI(recipeAPI)
		// usageIdentity is the fully-resolved workspace/profile/mode for this
		// process, cross-checked against the upstream daemon's /health at startup
		// when a profile policy is set. Handing it to the MCP server lets
		// brw_identity answer "which browser does this namespace drive?" without a
		// live HTTP round-trip.
		server.SetIdentity(usageIdentity)
		if store, err := configureBaselineStore(baselineRoot); err != nil {
			log.Fatalf("baseline store: %v", err)
		} else if store != nil {
			server.SetBaselineStore(store)
			log.Printf("regression baselines enabled at %s", store.Root())
		}
		if line, err := installBaselineDestinations(server, recipeBaselines, upstreamHTTP, baselineRouter); err != nil {
			log.Fatalf("%v", err)
		} else if line != "" {
			log.Printf("%s", line)
		}

		// A direct/bridge MCP process has no upstream HTTP middleware to record its
		// calls, so record them here. Upstream proxies intentionally rely on the
		// canonical daemon ledger and do not create duplicate local records.
		if upstreamHTTP == "" {
			server.SetUsageRecorder(usage)
		}
		if mcpIdleExit > 0 {
			server.SetIdleExit(mcpIdleExit)
			log.Printf("MCP idle-exit armed: exiting after %s without requests", mcpIdleExit)
		}
		// The HTTP idle watcher measures the HTTP mux, and an MCP tool call never
		// crosses it — it reaches the controller directly. Reporting each call to
		// the same tracker is what keeps --idle-exit from shutting the browser
		// down under the agent that is using it over stdio.
		if api != nil && api.IdleExit() > 0 {
			server.SetActivityHook(api.NoteActivity)
		}
		err := server.Serve(ctx, os.Stdin, os.Stdout)
		switch {
		case errors.Is(err, mcp.ErrIdleExit):
			log.Printf("mcp server: %v", err)
		case err != nil && ctx.Err() == nil:
			log.Fatalf("mcp server: %v", err)
		}
		return
	}

	fmt.Fprintf(os.Stderr, "brwd ready")
	if httpAddr != "" && httpAddr != "off" {
		fmt.Fprintf(os.Stderr, " at http://127.0.0.1%s", normalizeAddr(httpAddr))
	}
	fmt.Fprintln(os.Stderr)
	<-ctx.Done()
	// HTTP API and extension-bridge graceful shutdown run via the deferred
	// gracefulShutdown calls registered at construction, so they fire on every
	// exit path (including the MCP-mode early return).
}

// autoConnectSearchDirs is where --remote auto looks for a browser's own
// DevToolsActivePort file, most specific first: the directory this daemon was
// configured for, then brw's own default profile directory. A user data
// directory is where the browser writes its ephemeral debugging port, so a
// directory the operator named is a far better answer than any port guess.
func autoConnectSearchDirs(configured string) []string {
	var dirs []string
	if trimmed := strings.TrimSpace(configured); trimmed != "" {
		dirs = append(dirs, trimmed)
	}
	if fallback := cdplaunch.DefaultProfileDir(""); fallback != "" && fallback != strings.TrimSpace(configured) {
		dirs = append(dirs, fallback)
	}
	return dirs
}

func effectiveMCPIdleExit(configured time.Duration, mcpMode bool, upstreamHTTP string, explicitlyConfigured bool) time.Duration {
	if !mcpMode || strings.TrimSpace(upstreamHTTP) == "" || explicitlyConfigured || configured != 0 {
		return configured
	}
	return defaultProxyIdleExit
}

// extensionSearchPaths lists where an installed brw extension lives, most
// specific first. Exposed as a variable so tests can point it at a temp dir.
//
// brw does NOT load its extension into a direct-CDP Chrome by default. Loading
// it there buys nothing today: the extension reaches brw over the bridge
// WebSocket, and a direct-CDP daemon runs no bridge listener, so chrome.tabGroups
// stays unreachable and manager_tabgroups.go still returns
// ErrTabGroupingUnsupported. File-chooser interception, the other thing the
// bridge had, is plain CDP (page.SetInterceptFileChooserDialog, see
// manager.go uploadFileViaChooser) and already works without any extension.
// Serving tab groups on direct CDP needs a hybrid daemon that runs the bridge
// listener alongside CDP; until that exists, --extension stays explicit.
var extensionSearchPaths = func() []string {
	var out []string
	if dir := strings.TrimSpace(os.Getenv("BRW_EXTENSION_DIR")); dir != "" {
		out = append(out, dir)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out,
			filepath.Join(home, "Library", "Application Support", "brw", "extension"),
			filepath.Join(home, ".local", "share", "brw", "extension"),
		)
	}
	return append(out, filepath.Join("/usr", "share", "brw", "extension"))
}

// findInstalledExtension returns the first search path holding a manifest.
// A directory without one is somebody else's folder, not our extension.
func findInstalledExtension() (string, bool) {
	for _, dir := range extensionSearchPaths() {
		if dir == "" {
			continue
		}
		if info, err := os.Stat(filepath.Join(dir, "manifest.json")); err == nil && !info.IsDir() {
			return dir, true
		}
	}
	return "", false
}

// daemonMode is the human-facing label for how this daemon reached the browser.
// It mirrors localTransport so /health's mode and transport can never disagree
// about which lane is running.
func daemonMode(upstreamHTTP, remoteURL string, bridgeMode, chromeOptIn, browserProvider bool) string {
	switch {
	case upstreamHTTP != "":
		return "upstream-http"
	case bridgeMode:
		return "bridge"
	case browserProvider:
		// Not "direct": that says brw launched Chrome here, and nothing on this
		// daemon did.
		return "browser-provider"
	case chromeOptIn:
		return "chrome-opt-in"
	case strings.TrimSpace(remoteURL) != "":
		return "remote"
	default:
		return "direct"
	}
}

// httpListenerEnabled reports whether this daemon serves the HTTP API at all.
func httpListenerEnabled(httpAddr string) bool {
	return httpAddr != "" && httpAddr != "off"
}

// resolveIdleExit decides what --idle-exit means on a daemon that serves no
// HTTP API, and returns the --mcp-idle-exit to use plus a line for the log.
//
// The idle watcher is armed on the HTTP server and postponed by the requests
// that reach it, plus the MCP calls the stdio server reports to it. With no
// listener there is no watcher, so the flag on its own does nothing — which is
// the opposite of what an operator asking a daemon to stop itself wants.
//
// It is NOT a startup failure. BRW_IDLE_EXIT is one of the environment-sourced
// defaults, so an exported variable plus an ordinary `brwd --mcp --http off`
// became a daemon that refused to start, and a refusal is a far worse answer
// than the silence it replaced. What an operator asking for an idle exit means
// is the same thing in both modes, so on a stdio daemon the duration is carried
// over to the watcher that does work there.
//
// A --mcp-idle-exit the operator TYPED is left alone, including a typed zero
// that turns the stdio watcher off. The disposable-proxy default is not: it is
// a value nobody chose, and letting it outrank a duration somebody did type is
// how `--http off --idle-exit 20m` became a 90-minute daemon.
func resolveIdleExit(idleExit, mcpIdleExit time.Duration, httpAddr string, mcpMode, mcpIdleExitTyped bool) (time.Duration, string) {
	if idleExit <= 0 || httpListenerEnabled(httpAddr) {
		return mcpIdleExit, ""
	}
	if !mcpMode {
		return mcpIdleExit, fmt.Sprintf("WARNING: --idle-exit %s is armed on the HTTP API and this daemon has --http off with no --mcp, so nothing measures use and it will never fire", idleExit)
	}
	if mcpIdleExitTyped {
		return mcpIdleExit, fmt.Sprintf("--idle-exit %s does nothing with --http off; this stdio daemon follows the --mcp-idle-exit you set (%s) instead", idleExit, mcpIdleExit)
	}
	return idleExit, fmt.Sprintf("--idle-exit %s is armed on the HTTP API and this daemon has --http off; arming the stdio idle exit for the same duration instead", idleExit)
}

// autoConnectRefusal decides whether a resolved --remote auto endpoint may be
// driven, given the profile policy.
//
// The question is about the BROWSER, never about how discovery found it: is
// this the browser running out of the user data directory this daemon's policy
// named, or some other browser on the machine? A profile marked
// direct_cdp_allowed=false is a browser a human is signed into, and the daemon
// reports the policy's own user_data_dir and profile_directory as the identity
// of whatever it attached to — which is what brw_identity answers and what the
// run lock keys on. Attaching to a different browser under that identity is the
// damage, and it is the same damage however the port was discovered.
//
// So the gate is one comparison of directories, and nothing here reads the
// discovery Source. The first version of this check exempted every
// DevToolsActivePort hit on the premise that the file "came out of the user
// data directory the policy itself named" — untrue, because
// autoConnectSearchDirs also searches brw's own default profile directory, so a
// browser in ~/.brw/chrome-profile was accepted under a bridge-only policy and
// reported as the policy's profile. A source added later is covered by the same
// comparison with no edit here: an endpoint that cannot name the directory it
// came out of cannot be the policy's browser.
func autoConnectRefusal(endpoint cdplaunch.AutoEndpoint, profileName, policyUserDataDir string, directCDPAllowed, unsafeOverride bool) error {
	if directCDPAllowed || unsafeOverride {
		return nil
	}
	if sameUserDataDir(endpoint.From, policyUserDataDir) {
		return nil
	}
	found := fmt.Sprintf("found a browser on port %d that does not say which user data directory it belongs to (discovered by %s)", endpoint.Port, endpoint.Source)
	if strings.TrimSpace(endpoint.From) != "" {
		found = fmt.Sprintf("found a browser on port %d belonging to %s", endpoint.Port, endpoint.From)
	}
	wanted := strings.TrimSpace(policyUserDataDir)
	if wanted == "" {
		wanted = "a user data directory the policy does not name"
	}
	return fmt.Errorf("%s, and profile %q is allowed only through the extension bridge, not direct CDP, on the browser in %s; pass --remote %s explicitly if that is the browser you mean",
		found, profileName, wanted, endpoint.URL)
}

// sameUserDataDir reports whether two user data directory spellings name the
// same directory. An unnamed directory is never the same as anything: "brw does
// not know which browser this is" has to answer the question the same way "a
// different browser" does.
func sameUserDataDir(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// adoptUpstreamIdentity folds the identity of the daemon behind an
// --upstream-http proxy into the proxy's own.
//
// Transport, headlessness and the certificate policy are always adopted: this
// process is a disposable MCP proxy whose own Mode says how the AGENT reaches
// brw and nothing about how brw reaches Chrome.
//
// The four profile fields are adopted only when this process has no profile
// policy naming them, and they are what the run lock is keyed on. A proxy that
// reported none of them handed `brw run` the shared "unidentified" key while
// the daemon behind it handed out the profile's own — two locks, one Chrome,
// and the interleaving on a single tab that the lock exists to prevent. With a
// policy they are already set and verified against this same upstream at
// startup, so adopting would only overwrite equals.
func adoptUpstreamIdentity(local, upstream brwidentity.Identity, haveProfilePolicy bool) brwidentity.Identity {
	if upstream.Empty() {
		return local
	}
	local.Transport = upstream.Transport
	local.Headless = upstream.Headless
	local.IgnoreHTTPSErrors = upstream.IgnoreHTTPSErrors
	if !haveProfilePolicy {
		local.Workspace = upstream.Workspace
		local.Profile = upstream.Profile
		local.UserDataDir = upstream.UserDataDir
		local.ProfileDirectory = upstream.ProfileDirectory
	}
	return local
}

// localTransport names how THIS process reaches the browser. A proxy cannot
// know until it asks its upstream, so it reports empty here and adopts the
// answer from the upstream's health response.
//
// The Chrome opt-in lane is reported as its own transport rather than as direct
// CDP, because the catalogue differs in both directions: it has the cookie and
// incognito access the bridge lacks, and it is still the browser the user is
// signed into, so brw_state is refused there and is not on direct CDP. A caller
// told "direct-cdp" would read both of those wrongly.
//
// --remote is its own transport for the same reason and a sharper one: direct
// CDP means a browser brw started, and brw may point a browser it started at a
// staging directory it later deletes. On --remote it is somebody else's
// browser, the refusal is enforced in internal/browser whatever this reports,
// and reporting direct-cdp would advertise brw_set_download_path on a lane that
// always refuses it.
//
// A plugin-supplied browser is a fourth: --remote shares this machine's disk
// and clipboard, and that one does not, so the two cannot answer the same way
// about a path or an upload.
func localTransport(upstreamHTTP, remoteURL string, bridgeMode, chromeOptIn, browserProvider bool) string {
	switch {
	case upstreamHTTP != "":
		return ""
	case bridgeMode:
		return brwidentity.TransportExtensionBridge
	case browserProvider:
		// Its own transport, and specifically NOT remote-cdp: --remote is
		// pointed at a loopback endpoint, where this machine's filesystem
		// and clipboard are the browser's too. Here they are not, and a
		// caller that cannot tell the two apart cannot avoid asking a
		// browser on another machine for this machine's files.
		return brwidentity.TransportOffHostCDP
	case chromeOptIn:
		return brwidentity.TransportChromeOptIn
	case strings.TrimSpace(remoteURL) != "":
		return brwidentity.TransportRemoteCDP
	default:
		return brwidentity.TransportDirectCDP
	}
}

func resolveUsageLogPath(configured string, identity brwidentity.Identity) (string, error) {
	configured = strings.TrimSpace(configured)
	switch strings.ToLower(configured) {
	case "off", "disabled", "none":
		return "", nil
	case "", "auto":
		configDir, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("resolve user config directory: %w", err)
		}
		return filepath.Join(configDir, "brw", "usage", usageLogFileName(identity)), nil
	default:
		if configured == "~" || strings.HasPrefix(configured, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve home directory: %w", err)
			}
			configured = filepath.Join(home, strings.TrimPrefix(configured, "~/"))
		}
		return filepath.Clean(configured), nil
	}
}

func usageLogFileName(identity brwidentity.Identity) string {
	parts := make([]string, 0, 3)
	for _, value := range []string{identity.Workspace, identity.Profile, identity.Mode} {
		if part := safeFilenamePart(value); part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "brw")
	}
	return strings.Join(parts, "-") + ".ndjson"
}

func safeFilenamePart(value string) string {
	value = strings.TrimSpace(value)
	var out strings.Builder
	lastDash := false
	for _, r := range value {
		allowed := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_'
		if allowed {
			out.WriteRune(r)
			lastDash = false
			continue
		}
		if out.Len() > 0 && !lastDash {
			out.WriteByte('-')
			lastDash = true
		}
		if out.Len() >= 64 {
			break
		}
	}
	return strings.Trim(out.String(), "-.")
}

func usageLogMaxBytes(megabytes int) (int64, error) {
	if megabytes < 0 {
		return 0, errors.New("usage-log-max-mb must be non-negative")
	}
	const mib = int64(1024 * 1024)
	maxInt64 := int64(^uint64(0) >> 1)
	if int64(megabytes) > maxInt64/mib {
		return 0, errors.New("usage-log-max-mb is too large")
	}
	return int64(megabytes) * mib, nil
}

func mebibytes(value int) (int64, error) {
	if value <= 0 {
		return 0, errors.New("artifact size limits must be positive")
	}
	const mib = int64(1024 * 1024)
	maxInt64 := int64(^uint64(0) >> 1)
	if int64(value) > maxInt64/mib {
		return 0, errors.New("artifact size limit is too large")
	}
	return int64(value) * mib, nil
}

func defaultArtifactRoot(identity brwidentity.Identity) (string, error) {
	root, err := artifact.DefaultRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, runtimeScopeDir(identity)), nil
}

// runtimeScopeDir names a per-runtime subdirectory so two daemons driving
// different profiles never share a store.
func runtimeScopeDir(identity brwidentity.Identity) string {
	material := strings.Join([]string{
		identity.Workspace, identity.Profile, identity.UserDataDir, identity.ProfileDirectory,
	}, "\x00")
	if strings.Trim(material, "\x00") == "" {
		return "default"
	}
	digest := sha256.Sum256([]byte(material))
	return "runtime-" + hex.EncodeToString(digest[:8])
}

// configureSessionStateStore builds the browser host's brw_state store. No key
// means no store: a snapshot of a signed-in session is exactly the thing that
// must not be written in the clear, so the absence of a key disables the
// feature rather than degrading it.
func configureSessionStateStore(root, keyFile string, identity brwidentity.Identity) (*sessionstate.Store, error) {
	if strings.TrimSpace(keyFile) == "" {
		return nil, nil
	}
	resolved := strings.TrimSpace(root)
	if resolved == "" || strings.EqualFold(resolved, "auto") {
		var err error
		resolved, err = defaultSessionStateRoot(identity)
		if err != nil {
			return nil, err
		}
	}
	if !filepath.IsAbs(resolved) {
		return nil, errors.New("--state-root must be absolute")
	}
	key, err := sessionstate.LoadKey(keyFile, resolved)
	if err != nil {
		return nil, err
	}
	return sessionstate.NewStore(sessionstate.Config{Root: resolved, Key: key})
}

func defaultSessionStateRoot(identity brwidentity.Identity) (string, error) {
	root, err := sessionstate.DefaultRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, runtimeScopeDir(identity)), nil
}

// runSessionStateJanitor expires lapsed snapshots on a timer.
func runSessionStateJanitor(ctx context.Context, store *sessionstate.Store) {
	interval := min(15*time.Minute, max(time.Minute, store.TTL()/4))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := store.Sweep(); err != nil {
				log.Printf("session snapshot retention janitor: %v", err)
			}
		}
	}
}

func configureBaselineStore(root string) (*baseline.Store, error) {
	resolved := strings.TrimSpace(root)
	if resolved == "" || strings.EqualFold(resolved, "off") {
		return nil, nil
	}
	if strings.EqualFold(resolved, "auto") {
		var err error
		resolved, err = baseline.DefaultRoot()
		if err != nil {
			return nil, err
		}
	}
	if !filepath.IsAbs(resolved) {
		return nil, errors.New("--baseline-root must be absolute")
	}
	return baseline.NewStore(resolved)
}

// recipeReceiptsFor returns the write ledger for this deployment, or nil.
//
// A receipt exists to be readable after the daemon that wrote it has died, so
// it has to live with a party that outlives the daemon. An HTTP provider is
// one; a local directory of JSON files is not — it is this machine, and a
// receipt kept on the machine that crashed answers nothing. Those deployments
// therefore run without receipts rather than with a record that only looks
// like one, and the runner refuses any recipe declaring a mechanism it cannot
// honour instead of running it with the mechanism quietly switched off.
func recipeReceiptsFor(provider recipe.Provider) recipe.Receipts {
	if receipts, ok := provider.(recipe.Receipts); ok {
		return receipts
	}
	return nil
}

// recipeReceiptStatusLine says at startup which of the two deployments this is.
//
// The absence of receipts is invisible at run time for any recipe that does not
// declare a site nonce: writes still run, and the difference only shows up after
// a crash, when there is no record of the one that was in flight. An operator
// finding that out then is finding it out too late, so it is said here, with the
// flag that changes it.
func recipeReceiptStatusLine(receipts recipe.Receipts) string {
	if receipts != nil {
		return "external-write receipts are recorded with the recipe provider, so an interrupted write survives a daemon restart"
	}
	return "this recipe provider cannot hold external-write receipts: an interrupted write leaves no record a restarted daemon can find, and a recipe declaring a site idempotency nonce is refused; --recipe-provider-url configures a provider that can"
}

// installBaselineDestinations gives the MCP server the places a capture may go,
// and is where a daemon that cannot decide refuses to start.
//
// The provider's own store is installed whether or not a local root is
// configured: a baseline for a recipe the provider owns never goes to the local
// root, so a daemon with a provider and no --baseline-root can still gate those
// recipes. On a proxy there is no provider here to install — brw_baseline has
// no HTTP route, so it runs on this daemon while the provider is upstream — and
// only the routing question is wired, so a capture that belongs with the
// provider is refused by name instead of written to this machine's disk.
//
// A proxy whose upstream controller cannot answer that question would route
// every capture to its own --baseline-root with the tool description saying it
// could not, so it fails to start. It returns the startup line rather than
// printing it, because what an operator is told has to be decided in the same
// place as what was wired.
func installBaselineDestinations(server *mcp.Server, recipeBaselines recipe.BaselineStore, upstreamHTTP string, router recipe.BaselineRouter) (string, error) {
	switch {
	case recipeBaselines != nil:
		server.SetRecipeBaselines(recipeBaselines)
		return "", nil
	case strings.TrimSpace(upstreamHTTP) == "":
		return "", nil
	case router == nil:
		return "", fmt.Errorf("the upstream controller cannot answer baseline routing; brw_baseline would write every capture to --baseline-root, including captures of pages the private recipe provider's recipes reach")
	default:
		server.SetBaselineRouter(router)
		return proxyBaselineStatusLine(), nil
	}
}

// recipeBaselinesFor returns the provider's baseline side, or nil.
//
// Same shape as recipeReceiptsFor and for a related reason: the capability is
// the provider's, not the daemon's. Both shipped providers implement it, so nil
// here means a custom provider that does not — in which case its recipes' own
// baselines fall back to the local root, which is the behaviour that existed
// before there was anywhere else to put them.
func recipeBaselinesFor(provider recipe.Provider) recipe.BaselineStore {
	if store, ok := provider.(recipe.BaselineStore); ok {
		return store
	}
	return nil
}

// recipeBaselineStatusLine says at startup where a private recipe's baselines
// will land, because the answer differs per deployment and a screenshot of a
// signed-in page landing somewhere unexpected is the failure this routing
// exists to prevent.
func recipeBaselineStatusLine(store recipe.BaselineStore) string {
	if store == nil {
		return "this recipe provider holds no baselines: brw_baseline for its recipes falls back to --baseline-root"
	}
	return "regression baselines for this provider's own recipes are stored with it, at " + store.BaselineLocation()
}

// proxyBaselineStatusLine says at startup how a proxying daemon decides where a
// baseline belongs, because the answer is the difference between gating a
// private recipe and writing its page to this machine's disk.
func proxyBaselineStatusLine() string {
	return "brw_baseline asks the browser host where each capture belongs; one that belongs with its private recipe provider is refused here rather than written to --baseline-root"
}

func configureRecipeProvider(ctx context.Context, directory, providerURL, tokenFile string) (recipe.Provider, error) {
	directory = strings.TrimSpace(directory)
	providerURL = strings.TrimSpace(providerURL)
	tokenFile = strings.TrimSpace(tokenFile)
	if directory != "" && providerURL != "" {
		return nil, errors.New("use either --recipe-root or --recipe-provider-url, not both")
	}
	if tokenFile != "" && providerURL == "" {
		return nil, errors.New("--recipe-provider-token-file requires --recipe-provider-url")
	}
	if directory != "" {
		if !filepath.IsAbs(directory) {
			return nil, errors.New("recipe root must be absolute")
		}
		return recipe.NewDirectoryProvider(ctx, recipe.DirectoryConfig{
			Root: directory, RepositoryRoot: currentRepositoryRoot(),
		})
	}
	if providerURL == "" {
		return nil, nil
	}
	token := ""
	if tokenFile != "" {
		var err error
		token, err = readPrivateTokenFile(tokenFile)
		if err != nil {
			return nil, err
		}
	}
	return recipe.NewHTTPProvider(recipe.HTTPProviderConfig{BaseURL: providerURL, Token: token})
}

// currentRepositoryRoot is a runtime safety belt for source-tree launches. It
// lets the directory provider reject a recipe root nested in the checkout the
// daemon was started from; release CI separately enforces that no recipe corpus
// is tracked at all.
func currentRepositoryRoot() string {
	directory, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if info, err := os.Lstat(filepath.Join(directory, ".git")); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return ""
		}
		directory = parent
	}
}

func readPrivateTokenFile(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("recipe provider token file must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("recipe provider token file must be a regular file, not a symlink")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("recipe provider token file permissions %o are too broad; require 0600 or stricter", info.Mode().Perm())
	}
	if info.Size() > 1<<20 {
		return "", errors.New("recipe provider token file exceeds 1 MiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) || openedInfo.Mode().Perm()&0o077 != 0 {
		return "", errors.New("recipe provider token file changed before it was read")
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return "", err
	}
	if len(data) > 1<<20 {
		return "", errors.New("recipe provider token file exceeds 1 MiB")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("recipe provider token file is empty")
	}
	return token, nil
}

func normalizeAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return addr
	}
	if strings.HasPrefix(addr, "127.0.0.1:") {
		return strings.TrimPrefix(addr, "127.0.0.1")
	}
	return "/" + addr
}

// flagsSetOnCommandLine is the set of flags the operator actually typed. It is
// what keeps brw.json weaker than the command line: a flag's value cannot say
// whether anybody chose it, because brwd reads the environment as the default.
func flagsSetOnCommandLine() map[string]bool {
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

func flagWasSet(name string) bool {
	wasSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			wasSet = true
		}
	})
	return wasSet
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// watchParentExit polls the parent pid and calls stop when this process is
// reparented (orphaned): the session that spawned us is gone, so a stdio MCP
// child has nothing left to serve. Normally the parent's death also closes our
// stdin and Serve exits on EOF; this covers parents that leak the pipe to
// other processes or otherwise die without it closing.
func watchParentExit(stop func()) {
	parent := os.Getppid()
	if parent <= 1 {
		return
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if os.Getppid() != parent {
			log.Printf("parent process %d exited; shutting down orphaned MCP stdio server", parent)
			stop()
			return
		}
	}
}

// envDuration returns the duration value of an environment variable, or
// fallback when it is unset, empty, or not a valid Go duration string.
func envDuration(name string, fallback time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

// envInt returns the integer value of an environment variable, or fallback when
// it is unset, empty, or not a valid integer.
func envInt(name string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// bridgeRequireToken reports whether the extension bridge must reject a hello
// that carries no handshake token. It is on unless the operator opts out, which
// only a pre-0.2.0 extension needs.
func bridgeRequireToken() bool {
	return !envBool("BRW_BRIDGE_ALLOW_TOKENLESS")
}

// envBool reports whether an environment variable is set to a truthy value
// (1/true/yes/on, case-insensitive). Unset or empty is false.
func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// isLoopback reports whether addr binds to a loopback address (127.0.0.1 or
// localhost). Bare ":port" binds to all interfaces and is NOT loopback.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Try treating the whole string as a host.
		host = addr
	}
	host = strings.TrimSpace(host)
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		return false
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
