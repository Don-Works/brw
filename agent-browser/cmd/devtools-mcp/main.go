package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/revitt/agent-browser/internal/profilepolicy"
)

func main() {
	log.SetOutput(os.Stderr)

	var profileName string
	var policyPath string
	var endpoint string
	var mode string
	var command string
	flag.StringVar(&profileName, "profile", "", "workspace-allowed browser profile name")
	flag.StringVar(&policyPath, "profile-policy", "", "profile policy JSON path")
	flag.StringVar(&endpoint, "cdp-endpoint", "", "CDP browser endpoint for direct-CDP sessions")
	flag.StringVar(&mode, "mode", "", "devtools MCP correlation mode; defaults from profile policy")
	flag.StringVar(&command, "command", "npx -y chrome-devtools-mcp@latest", "command used to start Chrome DevTools MCP")
	flag.Parse()

	if profileName == "" {
		log.Fatal("--profile is required")
	}

	policy, err := profilepolicy.Load(policyPath)
	if err != nil {
		log.Fatalf("load profile policy: %v", err)
	}
	profile, err := policy.Find(profileName)
	if err != nil {
		log.Fatalf("profile policy: %v", err)
	}
	if !profile.DevToolsMCPAllowed {
		log.Fatalf("profile %q is not allowed to use Chrome DevTools MCP by workspace policy", profileName)
	}
	if mode == "" {
		mode = profile.DevToolsMCPMode
	}

	args := flag.Args()
	switch mode {
	case "cdp-endpoint":
		if endpoint == "" {
			log.Fatalf("profile %q requires --cdp-endpoint for DevTools MCP correlation", profileName)
		}
		args = append([]string{"--browserUrl", endpoint}, args...)
	case "profile-correlated-wrapper":
		log.Fatalf("profile %q uses installed Chrome auth; DevTools MCP must use Chrome's approved profile auto-connect flow or a managed extension install before this wrapper can launch it", profileName)
	default:
		if mode == "" {
			log.Fatalf("profile %q does not declare devtools_mcp_mode", profileName)
		}
		log.Fatalf("unsupported devtools_mcp_mode %q for profile %q", mode, profileName)
	}

	parts := strings.Fields(command)
	if len(parts) == 0 {
		log.Fatal("--command is empty")
	}
	cmd := exec.Command(parts[0], append(parts[1:], args...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Chrome DevTools MCP exited: %v\n", err)
		os.Exit(1)
	}
}
