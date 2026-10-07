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
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Don-Works/brw/internal/approvalgate"
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
	"github.com/Don-Works/brw/internal/pagewatch"
	"github.com/Don-Works/brw/internal/plugin"
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/profileroster"
	"github.com/Don-Works/brw/internal/recipe"
	"github.com/Don-Works/brw/internal/sessionstate"
	"github.com/Don-Works/brw/internal/setup"
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
	var pageWatchRoot string
	flag.StringVar(&pageWatchRoot, "page-watch-root", envDefault("BRW_PAGE_WATCH_ROOT", "auto"), "persistent page watcher store: auto uses an owner-only workspace/profile directory outside the repository; off disables watchers; otherwise an absolute directory. Sampling runs on the browser host, independently of MCP clients.")
	log.SetOutput(os.Stderr)

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
	var pacingValue string
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
	var httpTokenFile string
	var upstreamTokenFile string
	var pluginDir string
	var proxyServer string
	var proxyBypassList string
	var ignoreHTTPSErrors bool
	var caCertFile string
	var siteConsent bool
	var siteConsentConfig string
	var siteConsentPrompt bool
	var confirmActions bool
	var approvals bool
	var approvalTokenFile string
	var approvalStorePath string
	var approvalMode string
	var approvalModeSet bool
	var contentNavGuard bool
	var chromeOptIn bool
	var exitOnUpgrade string
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
	flag.StringVar(&httpTokenFile, "http-token-file", os.Getenv("BRW_HTTP_TOKEN_FILE"), "absolute path to an owner-only file holding a bearer token of at least 32 characters; every HTTP request must then send Authorization: Bearer <token>. Use it whenever --http binds a non-loopback address. Never logged.")
	flag.StringVar(&upstreamTokenFile, "upstream-token-file", os.Getenv("BRW_UPSTREAM_TOKEN_FILE"), "absolute path to an owner-only file holding the bearer token the --upstream-http daemon requires. BRW_UPSTREAM_TOKEN supplies the token itself when no file is given. Never logged.")
	flag.StringVar(&cfg.RemoteURL, "remote", os.Getenv("BRW_REMOTE_URL"), "attach to an existing CDP endpoint, for example http://127.0.0.1:9222, or \"auto\" to find one: brw reads DevToolsActivePort in the user data directory (which is the only place an ephemeral port is written) and then tries the conventional loopback debugging ports, attaching only to something that answers /json/version as a browser. An endpoint brw cannot prove is on this machine is the off-host-cdp transport, not direct-cdp: downloads, uploads, the clipboard and brw_state are refused by name there, because each of them belongs to the host the browser runs on.")
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
	flag.DurationVar(&timeout, "timeout", 20*time.Second, "default browser operation timeout; 0 removes the fixed limit, so an operation ends only by its own step timeouts or the caller's cancellation (a profile's operation_timeout sets it when this flag is not given)")
	flag.BoolVar(&printSystemPrompt, "print-system-prompt", false, "print the recommended agent system prompt to stdout and exit")
	flag.StringVar(&blockedDomains, "blocked-domains", os.Getenv("BRW_BLOCKED_DOMAINS"), "comma-separated domains the agent may never open (subdomains included); guardrail enforced on brw_open and brw_replay_request")
	flag.StringVar(&allowedDomains, "allowed-domains", os.Getenv("BRW_ALLOWED_DOMAINS"), "comma-separated allowlist; when set, the agent may ONLY open these domains (and subdomains)")
	flag.StringVar(&pacingValue, "pacing", os.Getenv("BRW_PACING"), "space agent actions and type text like a person: human or off. Defaults to human on the extension bridge, which drives a signed-in browser, and off elsewhere. A profile may set \"pacing\" instead.")
	flag.BoolVar(&enableWebMCP, "enable-webmcp", envBool("BRW_ENABLE_WEBMCP"), "install a fallback WebMCP runtime (document.modelContext) at document-start, on direct CDP and the extension bridge alike, so cooperating sites can register page tools brw_page_tools/brw_call_page_tool can use; a native WebMCP implementation is used without this flag")
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
	flag.BoolVar(&approvals, "approvals", false, "enable asynchronous operator approval on this browser-host daemon. Requires --site-consent and loopback --http, implies --confirm-actions, and cannot use --site-consent-prompt or --upstream-http.")
	flag.StringVar(&approvalTokenFile, "approval-token-file", "", "absolute path to an operator-only non-symlink 0600 file containing a random token of at least 32 characters. Required with --approvals; must live outside the artifact directory; never logged.")
	flag.StringVar(&approvalStorePath, "approval-store", "", "absolute approval request store path; defaults to approvals/requests.json beside the profile policy. Its dedicated parent must be 0700 and outside the artifact directory.")
	flag.StringVar(&approvalMode, "approval-mode", "risky", "approval policy: risky (default, best-effort risk detection) or all (every mutation). Requires --approvals when explicitly set.")
	flag.BoolVar(&contentNavGuard, "content-nav-guard", envBool("BRW_CONTENT_NAV_GUARD"), "refuse a top-level navigation that page content initiated to another site (an injected link click, a meta refresh, a script location assignment). What the agent asked for still works: the destination it named, and the navigation its own click or keypress causes. Any CDP transport; not the extension bridge or an upstream HTTP proxy.")
	flag.BoolVar(&chromeOptIn, "chrome-opt-in", envBool("BRW_CHROME_OPT_IN"), "attach to a Chrome 144+ instance whose user has turned on remote debugging at chrome://inspect/#remote-debugging. This is full browser-target CDP against the real signed-in profile, with none of the extension bridge's incognito or cookie limits and no extension at all. Downloads are reported but not routed, and brw_state is refused: brw will not move or seal what belongs to the browser's own user. A profile policy grants this lane with chrome_opt_in_allowed: true. brw never turns the opt-in on: it is a human action by design, and with it off the daemon says so and exits rather than launching a browser with a debugging flag. Chrome 144+ also asks you to approve each debugging connection in the browser window; brw waits two minutes for that and then exits naming the prompt, rather than hanging.")
	flag.StringVar(&chromeOptInBrowser, "chrome-opt-in-browser", envDefault("BRW_CHROME_OPT_IN_BROWSER", "chrome"), "which browser's user data directory --chrome-opt-in looks in for the endpoint (chrome, chromium, edge, brave, vivaldi)")
	flag.StringVar(&chromeOptInUserDataDir, "chrome-opt-in-user-data-dir", os.Getenv("BRW_CHROME_OPT_IN_USER_DATA_DIR"), "explicit user data directory for --chrome-opt-in, when the browser is not one brw knows the default path for. brw only reads from it.")
	flag.StringVar(&configPath, "config", os.Getenv("BRW_CONFIG"), "brw.json holding this machine's daemon defaults, with optional per-profile sections. Defaults to brw.json in the user config directory, which is read when it exists and ignored when it does not. It is the weakest source: a flag on the command line wins, then the environment, then the file.")
	flag.StringVar(&exitOnUpgrade, "exit-on-upgrade", envDefault("BRW_EXIT_ON_UPGRADE", "auto"), "extension bridge daemon: exit once an install replaces this binary and no request is in flight, so the service manager restarts it on the new build. auto (the default) turns it on only under launchd or systemd, which restart it; on and off force it.")
	flag.Parse()
	approvalModeSet = flagWasSet("approval-mode")

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
				approvalModeSet = approvalModeSet || setting.Flag == "approval-mode"
				settings = append(settings, setting.String())
			}
			log.Printf("config %s supplied %s", configFile, strings.Join(settings, ", "))
		}
	}

	mcpIdleExitTyped := flagWasSet("mcp-idle-exit") || strings.TrimSpace(os.Getenv("BRW_MCP_IDLE_EXIT")) != ""
	mcpIdleExit = effectiveMCPIdleExit(mcpIdleExit, mcpMode, upstreamHTTP, mcpIdleExitTyped)

	if printSystemPrompt {
		fmt.Println(mcp.AgentSystemPrompt)
		return
	}

	approvalOpts := approvalOptions{
		enabled: approvals, tokenFile: approvalTokenFile, storePath: approvalStorePath,
		mode: approvalMode, modeSet: approvalModeSet, siteConsent: siteConsent,
		prompt: siteConsentPrompt, httpAddr: httpAddr, upstream: upstreamHTTP,
		policyPath: profilePolicyPath, artifactDir: artifactDir,
	}
	if err := validateApprovalOptions(approvalOpts); err != nil {
		log.Fatalf("approvals: %v", err)
	}
	confirmActions = confirmActions || approvals

	cfg.Extensions = extensions
	cfg.ChromeArgs = chromeArgs
	timeout = browser.OperationTimeout(timeout)
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

		cfg.UserDataDir = ""
		cfg.ProfileDirectory = ""
	}

	var profile profilepolicy.Profile
	haveProfilePolicy := false

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

			cfg.UserDataDir = profile.UserDataDir
			cfg.ProfileDirectory = profile.ProfileDirectory
		}
		if cfg.ChromePath == "" && upstreamHTTP == "" && !bridgeMode && !chromeOptIn && cfg.RemoteURL == "" {
			cfg.ChromePath = profileBrowserExecutable(profile.Kind)
		}
		if profile.Pacing != "" && !flagWasSet("pacing") && os.Getenv("BRW_PACING") == "" {
			pacingValue = profile.Pacing
		}
		if profile.OperationTimeout != "" && !flagWasSet("timeout") {
			parsed, err := browser.ParseOperationTimeout(profile.OperationTimeout)
			if err != nil {
				log.Fatalf("profile %q: %v", profile.Name, err)
			}
			timeout = parsed
			cfg.Timeout = timeout
		}
		if profile.Headless && upstreamHTTP == "" {
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

		identityExpected.Mode = ""
		identityExpected.Transport = ""
		identityExpected.Headless = false
		identityExpected.IgnoreHTTPSErrors = false
		log.Printf("using workspace profile %q (%s)", profile.Name, profile.Kind)
	}

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

	if cdplaunch.IsAutoConnect(cfg.RemoteURL) {
		if bridgeMode || upstreamHTTP != "" {

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

	if strings.TrimSpace(cfg.RemoteURL) != "" && !brwidentity.BrowserRunsOnThisHost(cfg.RemoteURL) {
		log.Printf("WARNING: --remote %s names a browser that is not on this machine; brw reports transport %s and refuses downloads, uploads, the clipboard and brw_state, because each of those belongs to the host the browser runs on",
			browser.RedactEndpointURL(cfg.RemoteURL), brwidentity.TransportOffHostCDP)
	}
	cfg.Headless = headless

	if localTransport(upstreamHTTP, cfg.RemoteURL, bridgeMode, chromeOptIn, useBrowserProvider) == brwidentity.TransportOffHostCDP {
		runtimeIdentity.Transport = brwidentity.TransportOffHostCDP
	}
	usageIdentity := resolveIdentity(identityInputs{
		Runtime:           runtimeIdentity,
		UpstreamHTTP:      upstreamHTTP,
		RemoteURL:         cfg.RemoteURL,
		Bridge:            bridgeMode,
		ChromeOptIn:       chromeOptIn,
		BrowserProvider:   useBrowserProvider,
		Headless:          headless,
		IgnoreHTTPSErrors: ignoreHTTPSErrors,
		OptInUserDataDir:  chromeOptInEndpoint.UserDataDir,
	})

	approvalOpts.identity = runtimeIdentity
	approvalOpts.policyPath = profilePolicyPath
	approvalStore, approvalOperatorToken, err := buildApprovalStore(approvalOpts)
	if err != nil {
		log.Fatalf("approvals: %v", err)
	}
	if approvalStore != nil {
		defer func() {
			if err := approvalStore.Close(); err != nil {
				log.Printf("close approval store: %v", err)
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	var bridgeHandshakeToken string

	if upstreamHTTP != "" {
		upstream, err := httpclient.New(upstreamHTTP, timeout)
		if err != nil {
			log.Fatalf("upstream HTTP controller: %v", err)
		}
		upstreamToken, err := resolveUpstreamToken(upstreamTokenFile, os.Getenv("BRW_UPSTREAM_TOKEN"))
		if err != nil {
			log.Fatalf("upstream HTTP controller: %v", err)
		}
		upstream.SetAuthToken(upstreamToken)
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

		bridge.SetRaiseWindowOnFocus(bridgeRaiseWindow)
		bridge.SetDefaultGroup(bridgeTabGroup)
		bridge.SetWebMCP(enableWebMCP)
		bridge.SetPacing(resolvePacing(pacingValue, true))
		log.Printf("action pacing: %s", bridge.Pacing())

		bridge.SetFollowFocus(bridgeFollowFocus)

		bridge.SetMaxInflight(bridgeMaxInflight)

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
				releaseUnreleasedSession(remote, release)
			}
		} else {
			manager, err = browser.New(ctx, cfg)
		}
		if err != nil {
			log.Fatalf("start browser: %v", err)
		}
		manager.SetPacing(resolvePacing(pacingValue, false))
		log.Printf("action pacing: %s", manager.Pacing())
		controller = manager
		defer func() {
			if err := manager.Close(); err != nil {
				log.Printf("close browser: %v", err)
			}
		}()
	}

	if err := bridgeTokenAtLaunch(bridgeTokenFile(workspaceName), bridgeHandshakeToken); err != nil {
		log.Printf("note: %v", err)
	}

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

			log.Fatalf("--content-nav-guard needs a CDP transport: it is enforced on CDP request interception, which the extension bridge and the upstream HTTP proxy do not have")
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

		bridge.SetSiteConsent(consentGuard)
	}

	var artifactAPI artifact.API
	var recipeAPI recipe.API

	var recipeBaselines recipe.BaselineStore

	var baselineRouter recipe.BaselineRouter
	if upstreamHTTP != "" {

		artifactAPI, _ = controller.(artifact.API)
		recipeAPI, _ = controller.(recipe.API)

		baselineRouter, _ = controller.(recipe.BaselineRouter)
		if strings.TrimSpace(recipeRoot) != "" || strings.TrimSpace(recipeProviderURL) != "" || strings.TrimSpace(recipeProviderTokenFile) != "" {
			log.Printf("WARNING: recipe provider flags are ignored in --upstream-http mode; configure them on the browser-host daemon")
		}
		if strings.TrimSpace(pluginDir) != "" {

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

				go runSessionStateJanitor(ctx, store)
				log.Printf("browser-host session snapshots enabled at %s (ttl=%s, encrypted at rest)", store.Root(), store.TTL())
			}
		}
		provider, err := configureRecipeProvider(ctx, recipeRoot, recipeProviderURL, recipeProviderTokenFile)
		if err != nil {
			log.Fatalf("recipe provider: %v", err)
		}
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
		log.Printf("recipe runtime enabled (stored provider: %t; recipe bodies and inputs are never written to usage logs)", provider != nil)
		log.Printf("%s", recipeReceiptStatusLine(runner.Receipts))
		log.Printf("%s", recipeBaselineStatusLine(recipeBaselines))
	}

	var actionApprovalGate *approvalgate.Gate
	if approvalStore != nil {
		actionApprovalGate, err = approvalgate.New(controller, approvalStore, approvalMode)
		if err != nil {
			log.Fatalf("approvals: %v", err)
		}
		actionApprovalGate.SetOperatorOrigin("http://" + httpAddr)
	}

	var api *httpapi.Server
	var pageWatchAPI pagewatch.API
	if upstreamHTTP != "" {
		pageWatchAPI, _ = controller.(pagewatch.API)
	} else if !strings.EqualFold(pageWatchRoot, "off") {
		root := pageWatchRoot
		if strings.EqualFold(root, "auto") {
			watchIdentity := usageIdentity
			if watchIdentity.UserDataDir == "" {
				watchIdentity.UserDataDir = cfg.UserDataDir
				if watchIdentity.UserDataDir == "" && manager != nil && cfg.RemoteURL == "" && cfg.BrowserWSURL == "" && cfg.Remote == nil {
					watchIdentity.UserDataDir = cdplaunch.DefaultProfileDir("")
				}
			}
			if watchIdentity.ProfileDirectory == "" {
				watchIdentity.ProfileDirectory = cfg.ProfileDirectory
			}
			if watchIdentity.UserDataDir != "" {
				watchIdentity.UserDataDir, err = filepath.Abs(watchIdentity.UserDataDir)
				if err != nil {
					log.Fatalf("page watcher profile path: %v", err)
				}
			}
			if watchIdentity.Workspace == "" && watchIdentity.Profile == "" && watchIdentity.UserDataDir == "" {
				switch {
				case bridge != nil:
					watchIdentity.Profile = "bridge:" + bridgeAddr
				case cfg.Remote != nil:
					watchIdentity.Profile = "provider:" + cfg.Remote.ProviderID + ":" + cfg.Remote.SessionID
				case cfg.RemoteURL != "":
					watchIdentity.Profile = "remote:" + cfg.RemoteURL
				case cfg.BrowserWSURL != "":
					watchIdentity.Profile = "remote:" + cfg.BrowserWSURL
				}
			}
			root, err = pagewatch.DefaultRoot(watchIdentity)
			if err != nil {
				log.Fatalf("page watchers: %v", err)
			}
		}
		watchers, watchErr := pagewatch.New(ctx, controller, root, navPolicy, consentGuard)
		if watchErr != nil {
			log.Fatalf("page watchers: %v", watchErr)
		}
		defer watchers.Close()
		pageWatchAPI = watchers
		if manager != nil {
			manager.SetTabAccessGuard(watchers.CheckTabAccess)
		}
		if bridge != nil {
			bridge.SetTabAccessGuard(watchers.CheckTabAccess)
		}
	}
	if httpAddr != "" && httpAddr != "off" {

		api = httpapi.NewWithIdentity(httpAddr, controller, usageIdentity)
		if strings.TrimSpace(httpTokenFile) != "" {
			httpToken, err := readBearerTokenFile("http-token-file", httpTokenFile)
			if err != nil {
				log.Fatalf("http: %v", err)
			}
			api.SetAuthToken(httpToken)
		} else if !httpBindIsLoopback(httpAddr) {
			log.Printf("WARNING: --http %s is not loopback and no --http-token-file is set; anything that can reach it can drive this browser", httpAddr)
		}

		api.SetVersion(mcp.Version)
		api.SetPageWatchAPI(pageWatchAPI)
		api.SetNavigationPolicy(navPolicy)
		api.SetSiteConsent(consentGuard)
		api.SetApprovalGate(actionApprovalGate)
		if approvalStore != nil {
			if err := api.SetApprovalStore(approvalStore, approvalOperatorToken); err != nil {
				log.Fatalf("approvals: %v", err)
			}
		}
		api.SetUsageRecorder(usage)
		api.SetArtifactAPI(artifactAPI)
		api.SetRecipeAPI(recipeAPI)

		if recipeBaselines != nil {
			api.SetBaselineRouter(recipeBaselines)
		}
		api.SetPluginRegistry(plugins)
		api.SetProfilePolicyPath(profilePolicyPath)
		self, _ := os.Executable()
		api.SetProfileRoster(profileroster.Service{BRWDPath: self})
		if httpIdleExit > 0 {
			api.SetIdleExit(httpIdleExit)
			log.Printf("HTTP idle-exit armed: shutting down after %s with no API request", httpIdleExit)
			go func() {
				if api.WatchIdle(ctx) {
					log.Printf("no API request for %s; shutting down", httpIdleExit)

					stop()
				}
			}()
		}
		defer gracefulShutdown("HTTP API", api.Shutdown)
		go func() {
			log.Printf("HTTP API listening on %s", httpAddr)
			if err := api.ListenAndServe(); err != nil && err != http.ErrServerClosed {

				log.Printf("HTTP API failed on %s: %v", httpAddr, err)
				stop()
			}
		}()
	}

	if bridge != nil && !mcpMode {
		enabled, err := exitOnUpgradeEnabled(exitOnUpgrade, runtime.GOOS, os.Getppid(), os.Getenv)
		if err != nil {
			log.Fatalf("%v", err)
		}
		if enabled {
			go watchForUpgrade(ctx, stop, func() bool {
				return bridge.Busy() || (api != nil && api.InFlight() > 0)
			})
		}
	}

	if mcpMode {

		if !mcp.ValidToolProfile(mcpToolProfile) {
			log.Printf("unknown --mcp-tools %q; advertising the full surface (valid: %s)",
				mcpToolProfile, strings.Join(mcp.ToolProfileNames(), ", "))
		}
		log.Printf("MCP stdio server ready (tool profile: %s)", mcpToolProfile)

		go watchParentExit(stop)
		server := mcp.NewWithToolProfile(controller, mcpToolProfile)
		server.SetPageWatchAPI(pageWatchAPI)
		server.SetNavigationPolicy(navPolicy)
		server.SetSiteConsent(consentGuard)
		server.SetApprovalGate(actionApprovalGate)
		server.SetArtifactAPI(artifactAPI)
		server.SetRecipeAPI(recipeAPI)

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

		if upstreamHTTP == "" {
			server.SetUsageRecorder(usage)
		}
		if mcpIdleExit > 0 {
			server.SetIdleExit(mcpIdleExit)
			log.Printf("MCP idle-exit armed: exiting after %s without requests", mcpIdleExit)
		}

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

}

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

func daemonMode(upstreamHTTP, remoteURL string, bridgeMode, chromeOptIn, browserProvider bool) string {
	switch {
	case upstreamHTTP != "":
		return "upstream-http"
	case bridgeMode:
		return "bridge"
	case browserProvider:

		return "browser-provider"
	case chromeOptIn:
		return "chrome-opt-in"
	case strings.TrimSpace(remoteURL) != "":
		return "remote"
	default:
		return "direct"
	}
}

func httpListenerEnabled(httpAddr string) bool {
	return httpAddr != "" && httpAddr != "off"
}

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

func sameUserDataDir(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

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

type identityInputs struct {
	Runtime      brwidentity.Identity
	UpstreamHTTP string
	// RemoteURL is the endpoint --remote names.
	RemoteURL         string
	Bridge            bool
	ChromeOptIn       bool
	BrowserProvider   bool
	Headless          bool
	IgnoreHTTPSErrors bool
	// OptInUserDataDir is the directory the Chrome opt-in endpoint was discovered in.
	OptInUserDataDir string
}

func resolveIdentity(in identityInputs) brwidentity.Identity {
	out := in.Runtime
	if out.Mode == "" {
		out.Mode = daemonMode(in.UpstreamHTTP, in.RemoteURL, in.Bridge, in.ChromeOptIn, in.BrowserProvider)
	}
	if out.Transport == "" {
		out.Transport = localTransport(in.UpstreamHTTP, in.RemoteURL, in.Bridge, in.ChromeOptIn, in.BrowserProvider)
	}
	out.Headless = in.Headless
	out.IgnoreHTTPSErrors = in.IgnoreHTTPSErrors
	if in.ChromeOptIn && in.OptInUserDataDir != "" {
		out.UserDataDir = in.OptInUserDataDir
	}
	return out
}

func localTransport(upstreamHTTP, remoteURL string, bridgeMode, chromeOptIn, browserProvider bool) string {
	return brwidentity.Lane{
		UpstreamHTTP:    upstreamHTTP,
		Bridge:          bridgeMode,
		BrowserProvider: browserProvider,
		ChromeOptIn:     chromeOptIn,
		CDPEndpoint:     remoteURL,
	}.Transport()
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
			configured = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(configured, "~"), "/"))
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
		if out.Len() >= 64 {
			break
		}
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

func runtimeScopeDir(identity brwidentity.Identity) string {
	material := strings.Join([]string{
		identity.Workspace, identity.Profile, identity.UserDataDir, identity.ProfileDirectory,
	}, "\x00")

	if identity.Transport == brwidentity.TransportOffHostCDP {
		material += "\x00" + identity.Transport
	}
	if strings.Trim(material, "\x00") == "" {
		return "default"
	}
	digest := sha256.Sum256([]byte(material))
	return "runtime-" + hex.EncodeToString(digest[:8])
}

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

func recipeReceiptsFor(provider recipe.Provider) recipe.Receipts {
	if receipts, ok := provider.(recipe.Receipts); ok {
		return receipts
	}
	return nil
}

func recipeReceiptStatusLine(receipts recipe.Receipts) string {
	if receipts != nil {
		return "external-write receipts are recorded with the recipe provider, so an interrupted write survives a daemon restart"
	}
	return "this recipe provider cannot hold external-write receipts: an interrupted write leaves no record a restarted daemon can find, and a recipe declaring a site idempotency nonce is refused; --recipe-provider-url configures a provider that can"
}

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

func recipeBaselinesFor(provider recipe.Provider) recipe.BaselineStore {
	if store, ok := provider.(recipe.BaselineStore); ok {
		return store
	}
	return nil
}

func recipeBaselineStatusLine(store recipe.BaselineStore) string {
	if store == nil {
		return "this recipe provider holds no baselines: brw_baseline for its recipes falls back to --baseline-root"
	}
	return "regression baselines for this provider's own recipes are stored with it, at " + store.BaselineLocation()
}

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

func envDuration(name string, fallback time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func envInt(name string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func bridgeRequireToken() bool {
	return !envBool("BRW_BRIDGE_ALLOW_TOKENLESS")
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func profileBrowserExecutable(kind string) string {
	browser, ok := setup.LookupBrowser(kind)
	if !ok {
		return ""
	}
	return setup.BrowserExecutable(runtime.GOOS, browser, func(name string) (string, bool) {
		path, err := exec.LookPath(name)
		return path, err == nil
	})
}

func resolvePacing(value string, bridge bool) browser.PacingMode {
	if strings.TrimSpace(value) == "" {
		if bridge {
			return browser.PacingHuman
		}
		return browser.PacingOff
	}
	mode, err := browser.ParsePacing(value)
	if err != nil {
		log.Fatalf("--pacing: %v", err)
	}
	return mode
}
