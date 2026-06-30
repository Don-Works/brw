package main

import (
	"context"
	"flag"
	"fmt"
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

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwidentity"
	cdplaunch "github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/extensionbridge"
	httpapi "github.com/Don-Works/brw/internal/http"
	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/mcp"
	"github.com/Don-Works/brw/internal/navpolicy"
	"github.com/Don-Works/brw/internal/profilepolicy"
)

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
	var printSystemPrompt bool
	var blockedDomains string
	var allowedDomains string
	var enableWebMCP bool

	flag.StringVar(&httpAddr, "http", envDefault("BRW_HTTP_ADDR", "127.0.0.1:17310"), "HTTP listen address, or off. Defaults to loopback; bind a non-loopback address only behind SSH/Tailscale with caller auth.")
	flag.BoolVar(&mcpMode, "mcp", false, "run MCP stdio server")
	flag.StringVar(&mcpToolProfile, "mcp-tools", envDefault("BRW_MCP_TOOLS", "all"), "MCP tool surface advertised in tools/list: 'all' (full) or 'core' (lean common-flow set). All tools remain callable regardless.")
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
	flag.Parse()

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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
		if path := bridgeTokenPath(); path != "" {
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

	var api *httpapi.Server
	if httpAddr != "" && httpAddr != "off" {
		api = httpapi.NewWithIdentity(httpAddr, controller, runtimeIdentity)
		api.SetNavigationPolicy(navPolicy)
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
		log.Printf("MCP stdio server ready (tool profile: %s)", mcpToolProfile)
		server := mcp.NewWithToolProfile(controller, mcpToolProfile)
		server.SetNavigationPolicy(navPolicy)
		if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
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
// (no home dir resolvable) means "in-memory only".
func bridgeTokenPath() string {
	if override := strings.TrimSpace(os.Getenv("BRW_BRIDGE_TOKEN_FILE")); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".brw", "bridge-token")
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
