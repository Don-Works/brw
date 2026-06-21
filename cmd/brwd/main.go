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
	"strings"
	"syscall"
	"time"

	"github.com/Don-Works/brw/internal/browser"
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
	var timeout time.Duration
	var profileName string
	var workspaceName string
	var profilePolicyPath string
	var unsafeAllowDefaultProfileCDP bool
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
	flag.StringVar(&upstreamHTTP, "upstream-http", os.Getenv("BRW_UPSTREAM_HTTP"), "proxy MCP/HTTP control to an existing local brw HTTP daemon")
	flag.StringVar(&cfg.RemoteURL, "remote", os.Getenv("BRW_REMOTE_URL"), "attach to existing CDP endpoint, for example http://127.0.0.1:9222")
	flag.StringVar(&profileName, "profile", os.Getenv("BRW_PROFILE"), "workspace-allowed browser profile name")
	flag.StringVar(&workspaceName, "workspace", os.Getenv("BRW_WORKSPACE"), "workspace binding name for default/restricted profiles")
	flag.StringVar(&profilePolicyPath, "profile-policy", os.Getenv("BRW_PROFILE_POLICY"), "profile policy JSON path; defaults to standard brw config discovery")
	flag.BoolVar(&unsafeAllowDefaultProfileCDP, "unsafe-allow-default-profile-cdp", false, "diagnostic override for profiles marked direct_cdp_allowed=false")
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

	if profileName != "" || workspaceName != "" {
		policy, err := profilepolicy.Load(profilePolicyPath)
		if err != nil {
			log.Fatalf("load profile policy: %v", err)
		}
		profile, err := policy.ResolveProfile(workspaceName, profileName)
		if err != nil {
			log.Fatalf("profile policy: %v", err)
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
		log.Printf("using workspace profile %q (%s)", profile.Name, profile.Kind)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var controller browser.Controller
	var bridge *extensionbridge.Bridge
	var manager *browser.Manager

	if upstreamHTTP != "" {
		upstream, err := httpclient.New(upstreamHTTP, timeout)
		if err != nil {
			log.Fatalf("upstream HTTP controller: %v", err)
		}
		controller = upstream
		log.Printf("using upstream HTTP controller %s", upstreamHTTP)
	} else if bridgeMode {
		bridge = extensionbridge.New(bridgeAddr, timeout, bridgeExtensionID)
		controller = bridge
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

	var api *httpapi.Server
	if httpAddr != "" && httpAddr != "off" {
		api = httpapi.New(httpAddr, controller)
		if !isLoopback(httpAddr) {
			log.Printf("WARNING: HTTP API bound to non-loopback address %s; no authentication is enforced — ensure caller auth is in place (SSH/Tailscale)", httpAddr)
		}
		go func() {
			log.Printf("HTTP API listening on %s", httpAddr)
			if err := api.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				// Fail loudly instead of limping on with a dead control plane.
				// In MCP mode the stdin Serve loop would otherwise keep running
				// with no HTTP listener (a zombie a port clash silently created).
				log.Fatalf("HTTP API failed on %s: %v", httpAddr, err)
			}
		}()
	}

	if mcpMode {
		log.Printf("MCP stdio server ready (tool profile: %s)", mcpToolProfile)
		server := mcp.NewWithToolProfile(controller, mcpToolProfile)
		if policy := navpolicy.Parse(allowedDomains, blockedDomains); !policy.Empty() {
			server.SetNavigationPolicy(policy)
			log.Printf("navigation guardrail active (allow=%d, block=%d domains)", len(policy.Allowed), len(policy.Blocked))
		}
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

	if api != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = api.Shutdown(shutdownCtx)
	}
	if bridge != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = bridge.Shutdown(shutdownCtx)
	}
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

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
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
