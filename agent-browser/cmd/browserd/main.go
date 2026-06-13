package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/revitt/agent-browser/internal/browser"
	cdplaunch "github.com/revitt/agent-browser/internal/cdp"
	"github.com/revitt/agent-browser/internal/extensionbridge"
	httpapi "github.com/revitt/agent-browser/internal/http"
	"github.com/revitt/agent-browser/internal/httpclient"
	"github.com/revitt/agent-browser/internal/mcp"
	"github.com/revitt/agent-browser/internal/profilepolicy"
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

	flag.StringVar(&httpAddr, "http", envDefault("AGENT_BROWSER_HTTP_ADDR", "127.0.0.1:17310"), "HTTP listen address, or off. Defaults to loopback; bind a non-loopback address only behind SSH/Tailscale with caller auth.")
	flag.BoolVar(&mcpMode, "mcp", false, "run MCP stdio server")
	flag.BoolVar(&bridgeMode, "bridge", false, "use installed Chrome extension bridge instead of direct CDP")
	flag.StringVar(&bridgeAddr, "bridge-addr", envDefault("AGENT_BROWSER_BRIDGE_ADDR", "127.0.0.1:17311"), "extension bridge WebSocket listen address")
	flag.StringVar(&upstreamHTTP, "upstream-http", os.Getenv("AGENT_BROWSER_UPSTREAM_HTTP"), "proxy MCP/HTTP control to an existing local agent-browser HTTP daemon")
	flag.StringVar(&cfg.RemoteURL, "remote", os.Getenv("AGENT_BROWSER_REMOTE_URL"), "attach to existing CDP endpoint, for example http://127.0.0.1:9222")
	flag.StringVar(&profileName, "profile", os.Getenv("AGENT_BROWSER_PROFILE"), "workspace-allowed browser profile name")
	flag.StringVar(&workspaceName, "workspace", os.Getenv("AGENT_BROWSER_WORKSPACE"), "workspace binding name for default/restricted profiles")
	flag.StringVar(&profilePolicyPath, "profile-policy", os.Getenv("AGENT_BROWSER_PROFILE_POLICY"), "profile policy JSON path; defaults to .mcplexer/config/browser-profiles.json discovery")
	flag.BoolVar(&unsafeAllowDefaultProfileCDP, "unsafe-allow-default-profile-cdp", false, "diagnostic override for profiles marked direct_cdp_allowed=false")
	flag.StringVar(&cfg.ChromePath, "chrome-path", os.Getenv("AGENT_BROWSER_CHROME_PATH"), "Chrome/Chromium executable path")
	flag.StringVar(&cfg.UserDataDir, "user-data-dir", envDefault("AGENT_BROWSER_USER_DATA_DIR", cdplaunch.DefaultProfileDir("")), "persistent Chrome user data directory")
	flag.StringVar(&cfg.ProfileDirectory, "profile-directory", os.Getenv("AGENT_BROWSER_PROFILE_DIRECTORY"), "Chrome profile directory within user data dir, for example 'Profile 1'")
	flag.IntVar(&cfg.Port, "remote-debugging-port", 0, "remote debugging port for launched Chrome; 0 chooses a free local port")
	flag.Var(&extensions, "extension", "extension directory to load; repeatable")
	flag.Var(&chromeArgs, "chrome-arg", "extra Chrome argument; repeatable")
	flag.DurationVar(&timeout, "timeout", 20*time.Second, "default browser operation timeout")
	flag.Parse()

	cfg.Extensions = extensions
	cfg.ChromeArgs = chromeArgs
	cfg.Timeout = timeout

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
			log.Fatalf("profile %q is allowed only through extension bridge, not direct CDP launch; use --profile agent-revitt, --remote, or --unsafe-allow-default-profile-cdp for diagnostics", profile.Name)
		}
		cfg.UserDataDir = profile.UserDataDir
		cfg.ProfileDirectory = profile.ProfileDirectory
		log.Printf("using workspace profile %q (%s)", profile.Name, profile.Kind)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var controller httpapi.Controller
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
		log.Printf("MCP stdio server ready")
		if err := mcp.New(controller).Serve(ctx, os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
			log.Fatalf("mcp server: %v", err)
		}
		return
	}

	fmt.Fprintf(os.Stderr, "agent-browserd ready")
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
