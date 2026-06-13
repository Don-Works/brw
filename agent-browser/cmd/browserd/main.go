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
	httpapi "github.com/revitt/agent-browser/internal/http"
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
	var timeout time.Duration
	var profileName string
	var profilePolicyPath string
	var unsafeAllowDefaultProfileCDP bool

	flag.StringVar(&httpAddr, "http", ":17310", "HTTP listen address, or off")
	flag.BoolVar(&mcpMode, "mcp", false, "run MCP stdio server")
	flag.StringVar(&cfg.RemoteURL, "remote", "", "attach to existing CDP endpoint, for example http://127.0.0.1:9222")
	flag.StringVar(&profileName, "profile", "", "workspace-allowed browser profile name")
	flag.StringVar(&profilePolicyPath, "profile-policy", "", "profile policy JSON path; defaults to .mcplexer/config/browser-profiles.json discovery")
	flag.BoolVar(&unsafeAllowDefaultProfileCDP, "unsafe-allow-default-profile-cdp", false, "diagnostic override for profiles marked direct_cdp_allowed=false")
	flag.StringVar(&cfg.ChromePath, "chrome-path", "", "Chrome/Chromium executable path")
	flag.StringVar(&cfg.UserDataDir, "user-data-dir", cdplaunch.DefaultProfileDir(""), "persistent Chrome user data directory")
	flag.StringVar(&cfg.ProfileDirectory, "profile-directory", "", "Chrome profile directory within user data dir, for example 'Profile 1'")
	flag.IntVar(&cfg.Port, "remote-debugging-port", 0, "remote debugging port for launched Chrome; 0 chooses a free local port")
	flag.Var(&extensions, "extension", "extension directory to load; repeatable")
	flag.Var(&chromeArgs, "chrome-arg", "extra Chrome argument; repeatable")
	flag.DurationVar(&timeout, "timeout", 20*time.Second, "default browser operation timeout")
	flag.Parse()

	cfg.Extensions = extensions
	cfg.ChromeArgs = chromeArgs
	cfg.Timeout = timeout

	if profileName != "" {
		policy, err := profilepolicy.Load(profilePolicyPath)
		if err != nil {
			log.Fatalf("load profile policy: %v", err)
		}
		profile, err := policy.Find(profileName)
		if err != nil {
			log.Fatalf("profile policy: %v", err)
		}
		if !profile.DirectCDPAllowed && cfg.RemoteURL == "" && !unsafeAllowDefaultProfileCDP {
			log.Fatalf("profile %q is allowed only through extension bridge, not direct CDP launch; use --profile agent-revitt, --remote, or --unsafe-allow-default-profile-cdp for diagnostics", profileName)
		}
		cfg.UserDataDir = profile.UserDataDir
		cfg.ProfileDirectory = profile.ProfileDirectory
		log.Printf("using workspace profile %q (%s)", profile.Name, profile.Kind)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	manager, err := browser.New(ctx, cfg)
	if err != nil {
		log.Fatalf("start browser: %v", err)
	}
	defer func() {
		if err := manager.Close(); err != nil {
			log.Printf("close browser: %v", err)
		}
	}()

	var api *httpapi.Server
	if httpAddr != "" && httpAddr != "off" {
		api = httpapi.New(httpAddr, manager)
		go func() {
			log.Printf("HTTP API listening on %s", httpAddr)
			if err := api.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("HTTP API stopped: %v", err)
				stop()
			}
		}()
	}

	if mcpMode {
		log.Printf("MCP stdio server ready")
		if err := mcp.New(manager).Serve(ctx, os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
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
