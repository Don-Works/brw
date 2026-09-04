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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwidentity"
	cdplaunch "github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/extensionbridge"
	httpapi "github.com/Don-Works/brw/internal/http"
	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/mcp"
	"github.com/Don-Works/brw/internal/navpolicy"
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/recipe"
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
	var upstreamHTTP string
	var mcpToolProfile string
	var mcpIdleExit time.Duration
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
	var recipeRoot string
	var recipeProviderURL string
	var recipeProviderTokenFile string

	flag.StringVar(&httpAddr, "http", envDefault("BRW_HTTP_ADDR", "127.0.0.1:17310"), "HTTP listen address, or off. Defaults to loopback; bind a non-loopback address only behind SSH/Tailscale with caller auth.")
	flag.BoolVar(&mcpMode, "mcp", false, "run MCP stdio server")
	flag.StringVar(&mcpToolProfile, "mcp-tools", envDefault("BRW_MCP_TOOLS", "auto"), "MCP tool surface advertised in tools/list: 'all' (full), 'core' (lean common-flow set), 'minimal' (smallest surface that still completes ordinary web work), or 'auto' (default; starts minimal and grows as the agent discovers tools with brw_tools). The catalogue is re-sent on every request, so a narrower profile is a per-turn context saving. All tools remain callable regardless.")
	flag.DurationVar(&mcpIdleExit, "mcp-idle-exit", envDuration("BRW_MCP_IDLE_EXIT", 0), "exit the --mcp stdio server cleanly after this long with no requests; upstream HTTP proxies default to 90m unless this flag or BRW_MCP_IDLE_EXIT is explicitly set (0 disables). Prevents abandoned clients from accumulating disposable proxy processes.")
	flag.BoolVar(&bridgeMode, "bridge", false, "use installed Chrome extension bridge instead of direct CDP")
	flag.StringVar(&bridgeAddr, "bridge-addr", envDefault("BRW_BRIDGE_ADDR", "127.0.0.1:17311"), "extension bridge WebSocket listen address")
	flag.BoolVar(&bridgeRaiseWindow, "bridge-raise-window", envBool("BRW_BRIDGE_RAISE_WINDOW"), "bridge: raise the Chrome window to the OS foreground on focus_tab. Off by default so automation never steals your focus while you work elsewhere.")
	flag.StringVar(&bridgeTabGroup, "bridge-tab-group", envDefault("BRW_BRIDGE_TAB_GROUP", "brw"), "bridge: tab-group title brw_open uses when no group is given, so the agent's tabs stay corralled in one labelled group. Set empty to disable default grouping.")
	flag.BoolVar(&bridgeFollowFocus, "bridge-follow-focus", envBool("BRW_BRIDGE_FOLLOW_FOCUS"), "bridge: follow the user's manually-focused Chrome tab for no-tab_id actions (legacy behavior). OFF by default: brw works in its own tab group on tabs it opened (opening a fresh one when needed) and never touches your existing tabs unless you pass tab_id. Turn on for an interactive session where you want brw to act on whatever tab you have selected.")
	flag.IntVar(&bridgeMaxInflight, "bridge-max-inflight", envInt("BRW_BRIDGE_MAX_INFLIGHT", 6), "bridge: max concurrent operations on the shared extension socket. Excess calls queue and, past the deadline, fail fast with a busy signal. Caps load on the single Chrome extension worker so many parallel agents can't wedge it. 0 disables the cap.")
	flag.StringVar(&upstreamHTTP, "upstream-http", os.Getenv("BRW_UPSTREAM_HTTP"), "proxy MCP/HTTP control to an existing local brw HTTP daemon")
	flag.StringVar(&cfg.RemoteURL, "remote", os.Getenv("BRW_REMOTE_URL"), "attach to existing CDP endpoint, for example http://127.0.0.1:9222")
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
	flag.StringVar(&recipeRoot, "recipe-root", os.Getenv("BRW_RECIPE_ROOT"), "absolute 0700 directory containing private 0600 recipe JSON files; must live outside the brw source repository")
	flag.StringVar(&recipeProviderURL, "recipe-provider-url", os.Getenv("BRW_RECIPE_PROVIDER_URL"), "HTTPS private recipe-provider base URL (loopback HTTP allowed); use instead of --recipe-root")
	flag.StringVar(&recipeProviderTokenFile, "recipe-provider-token-file", os.Getenv("BRW_RECIPE_PROVIDER_TOKEN_FILE"), "0600 regular file containing the private recipe-provider bearer token; never logged")
	flag.Parse()

	mcpIdleExit = effectiveMCPIdleExit(
		mcpIdleExit,
		mcpMode,
		upstreamHTTP,
		flagWasSet("mcp-idle-exit") || strings.TrimSpace(os.Getenv("BRW_MCP_IDLE_EXIT")) != "",
	)

	if printSystemPrompt {
		fmt.Println(mcp.AgentSystemPrompt)
		return
	}

	cfg.Extensions = extensions
	cfg.ChromeArgs = chromeArgs
	cfg.Timeout = timeout
	cfg.WebMCP = enableWebMCP
	cfg.AllowRealProfile = unsafeRealProfile
	if unsafeRealProfile {
		log.Printf("WARNING: --unsafe-real-profile is active; brw may launch Chrome against your real browser profile, which can corrupt it (lost logins, won't reopen)")
	}
	runtimeIdentity := brwidentity.Identity{}
	identityExpected := brwidentity.Identity{}
	haveProfilePolicy := false

	if profileName != "" || workspaceName != "" {
		policy, err := profilepolicy.Load(profilePolicyPath)
		if err != nil {
			log.Fatalf("load profile policy: %v", err)
		}
		profile, err := policy.ResolveProfile(workspaceName, profileName)
		if err != nil {
			log.Fatalf("profile policy: %v", err)
		}
		haveProfilePolicy = true
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
		cfg.UserDataDir = profile.UserDataDir
		cfg.ProfileDirectory = profile.ProfileDirectory
		mode := "direct"
		if upstreamHTTP != "" {
			mode = "upstream-http"
		} else if bridgeMode {
			mode = "bridge"
		}
		runtimeIdentity = brwidentity.Identity{
			Workspace:        workspaceName,
			Profile:          profile.Name,
			UserDataDir:      profile.UserDataDir,
			ProfileDirectory: profile.ProfileDirectory,
			Mode:             mode,
		}
		identityExpected = runtimeIdentity
		identityExpected.Mode = ""
		log.Printf("using workspace profile %q (%s)", profile.Name, profile.Kind)
	}
	usageIdentity := runtimeIdentity
	if usageIdentity.Mode == "" {
		switch {
		case upstreamHTTP != "":
			usageIdentity.Mode = "upstream-http"
		case bridgeMode:
			usageIdentity.Mode = "bridge"
		default:
			usageIdentity.Mode = "direct"
		}
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

	if upstreamHTTP != "" {
		upstream, err := httpclient.New(upstreamHTTP, timeout)
		if err != nil {
			log.Fatalf("upstream HTTP controller: %v", err)
		}
		if haveProfilePolicy {
			verifyCtx, cancel := context.WithTimeout(context.Background(), timeout)
			health, err := upstream.Health(verifyCtx)
			cancel()
			if err != nil {
				log.Fatalf("verify upstream identity: %v", err)
			}
			if health.Identity.Empty() {
				log.Fatalf("upstream HTTP controller %s does not expose workspace/profile identity; refusing to proxy workspace %q profile %q", upstreamHTTP, identityExpected.Workspace, identityExpected.Profile)
			}
			if mismatches := health.Identity.Mismatches(identityExpected); len(mismatches) > 0 {
				log.Fatalf("upstream HTTP controller %s identity mismatch: %s", upstreamHTTP, strings.Join(mismatches, "; "))
			}
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
		// This is NON-BREAKING by default — an older extension that sends no token
		// still connects (logged once), so upgrading the daemon never bricks an
		// already-installed extension; a WRONG token is always rejected. Once every
		// extension is reloaded to 0.2.0, set BRW_BRIDGE_REQUIRE_TOKEN=1 to make the
		// token mandatory.
		token, err := extensionbridge.NewAuthToken()
		if err != nil {
			log.Fatalf("generate extension bridge auth token: %v", err)
		}
		bridge.SetAuthToken(token)
		bridge.SetRequireToken(envBool("BRW_BRIDGE_REQUIRE_TOKEN"))
		if path := bridgeTokenPath(workspaceName); path != "" {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				log.Printf("note: could not create bridge token dir %s: %v", filepath.Dir(path), err)
			} else if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
				log.Printf("note: could not persist bridge token to %s: %v", path, err)
			}
		}
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
		manager, err = browser.New(ctx, cfg)
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

	var artifactAPI artifact.API
	var recipeAPI recipe.API
	if upstreamHTTP != "" {
		// The proxy controller implements both optional APIs and forwards them to
		// the canonical browser host. Never create a second cache/provider here.
		artifactAPI, _ = controller.(artifact.API)
		recipeAPI, _ = controller.(recipe.API)
		if strings.TrimSpace(recipeRoot) != "" || strings.TrimSpace(recipeProviderURL) != "" || strings.TrimSpace(recipeProviderTokenFile) != "" {
			log.Printf("WARNING: recipe provider flags are ignored in --upstream-http mode; configure them on the browser-host daemon")
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
			store, err := artifact.NewStore(artifact.Config{
				Root: root, MaxArtifactBytes: maxArtifactBytes,
				MaxTotalBytes: maxTotalBytes, TTL: artifactTTL,
			})
			if err != nil {
				log.Fatalf("artifact store: %v", err)
			}
			service, err := artifact.NewService(store, controller)
			if err != nil {
				log.Fatalf("artifact service: %v", err)
			}
			artifactAPI = service
			janitorInterval := min(5*time.Minute, max(time.Second, artifactTTL/2))
			go store.RunJanitor(ctx, janitorInterval, func(err error) {
				log.Printf("artifact retention janitor: %v", err)
			})
			log.Printf("browser-host artifact store enabled at %s (max=%d MiB, total=%d MiB, ttl=%s)", store.Root(), artifactMaxMB, artifactTotalMB, artifactTTL)
		}

		provider, err := configureRecipeProvider(ctx, recipeRoot, recipeProviderURL, recipeProviderTokenFile)
		if err != nil {
			log.Fatalf("recipe provider: %v", err)
		}
		if provider != nil {
			service, err := recipe.NewService(provider, recipe.Runner{Surface: &recipe.BrowserSurface{Browser: controller, Artifacts: artifactAPI}})
			if err != nil {
				log.Fatalf("recipe service: %v", err)
			}
			recipeAPI = service
			log.Printf("private recipe provider enabled (recipe bodies and inputs are never written to usage logs)")
		}
	}

	var api *httpapi.Server
	if httpAddr != "" && httpAddr != "off" {
		api = httpapi.NewWithIdentity(httpAddr, controller, runtimeIdentity)
		api.SetNavigationPolicy(navPolicy)
		api.SetUsageRecorder(usage)
		api.SetArtifactAPI(artifactAPI)
		api.SetRecipeAPI(recipeAPI)
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
		server.SetArtifactAPI(artifactAPI)
		server.SetRecipeAPI(recipeAPI)
		// usageIdentity is the fully-resolved workspace/profile/mode for this
		// process, cross-checked against the upstream daemon's /health at startup
		// when a profile policy is set. Handing it to the MCP server lets
		// brw_identity answer "which browser does this namespace drive?" without a
		// live HTTP round-trip.
		server.SetIdentity(usageIdentity)
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

func effectiveMCPIdleExit(configured time.Duration, mcpMode bool, upstreamHTTP string, explicitlyConfigured bool) time.Duration {
	if !mcpMode || strings.TrimSpace(upstreamHTTP) == "" || explicitlyConfigured || configured != 0 {
		return configured
	}
	return defaultProxyIdleExit
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
	material := strings.Join([]string{
		identity.Workspace, identity.Profile, identity.UserDataDir, identity.ProfileDirectory,
	}, "\x00")
	if strings.Trim(material, "\x00") == "" {
		return filepath.Join(root, "default"), nil
	}
	digest := sha256.Sum256([]byte(material))
	return filepath.Join(root, "runtime-"+hex.EncodeToString(digest[:8])), nil
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

// bridgeTokenPath returns the 0600 file where the per-launch extension-bridge
// handshake token is persisted (for operator inspection / future tooling), under
// the brw state dir ~/.brw/. BRW_BRIDGE_TOKEN_FILE overrides it; an empty result
// (no home dir resolvable) means "in-memory only". Workspace-bound daemons get a
// per-workspace file (bridge-token-<workspace>) — multiple bridge daemons on one
// machine previously clobbered a single shared file, last writer wins, so the
// persisted token matched only one of the running daemons.
func bridgeTokenPath(workspace string) string {
	if override := strings.TrimSpace(os.Getenv("BRW_BRIDGE_TOKEN_FILE")); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	name := "bridge-token"
	if workspace != "" {
		name += "-" + strings.Map(func(r rune) rune {
			switch r {
			case '/', '\\', ':':
				return '-'
			}
			return r
		}, workspace)
	}
	return filepath.Join(home, ".brw", name)
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
