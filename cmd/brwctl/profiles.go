package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/profileroster"
	"github.com/Don-Works/brw/internal/setup"
)

func profilesCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", profilesUsage)
	}
	switch args[0] {
	case "list":
		return profilesList(args[1:])
	case "create":
		return profilesCreate(args[1:])
	case "sessions":
		return profilesSessions(args[1:])
	case "pin":
		return profilesPin(args[1:])
	case "copy", "move":
		return profilesCopy(args[0], args[1:])
	case "gui":
		return profilesGUI(args[1:])
	default:
		return fmt.Errorf("unknown profiles command %q\n%s", args[0], profilesUsage)
	}
}

const profilesUsage = `usage: brwctl profiles <command>

commands:
  list                 print the profile roster as JSON
  create --name SLUG   add an isolated direct-CDP Chromium jar
  sessions NAME        cookie-domain chips for one profile
  pin NAME --origin U  declare an expected login
  copy --from A --to B --domain D
  gui                  print the loopback URL for the visual editor
`

func profilesPolicyPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if p := strings.TrimSpace(os.Getenv("BRW_PROFILE_POLICY")); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return setup.DefaultPolicyPath(home)
}

func profilesList(args []string) error {
	fs := flag.NewFlagSet("profiles list", flag.ContinueOnError)
	policyPath := fs.String("policy", "", "browser-profiles.json path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := profilesPolicyPath(*policyPath)
	if err != nil {
		return err
	}
	board, err := profileroster.LoadBoard(context.Background(), path)
	if err != nil {
		return err
	}
	return printJSON(board)
}

func profilesCreate(args []string) error {
	fs := flag.NewFlagSet("profiles create", flag.ContinueOnError)
	name := fs.String("name", "", "profile slug")
	account := fs.String("account", "", "intended Google account (becomes the first pin)")
	browser := fs.String("browser", "chromium", "chromium or chrome")
	policyPath := fs.String("policy", "", "browser-profiles.json path")
	noService := fs.Bool("no-service", false, "write the policy only; do not install a LaunchAgent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*name) == "" {
		return fmt.Errorf("--name is required")
	}
	path, err := profilesPolicyPath(*policyPath)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	brwd := "brwd"
	if exe, err := os.Executable(); err == nil {
		candidate := strings.TrimSuffix(exe, "brwctl") + "brwd"
		if _, err := os.Stat(candidate); err == nil {
			brwd = candidate
		}
	}
	result, err := profileroster.Create(profileroster.CreateRequest{
		Name:           *name,
		Account:        *account,
		Browser:        *browser,
		PolicyPath:     path,
		Home:           home,
		GOOS:           runtime.GOOS,
		BRWDPath:       brwd,
		InstallService: !*noService,
	})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func profilesSessions(args []string) error {
	fs := flag.NewFlagSet("profiles sessions", flag.ContinueOnError)
	policyPath := fs.String("policy", "", "browser-profiles.json path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: brwctl profiles sessions NAME")
	}
	path, err := profilesPolicyPath(*policyPath)
	if err != nil {
		return err
	}
	board, err := profileroster.LoadBoard(context.Background(), path)
	if err != nil {
		return err
	}
	for _, well := range board.Wells {
		if well.Name == fs.Arg(0) {
			return printJSON(well)
		}
	}
	return fmt.Errorf("profile %q is not in the roster", fs.Arg(0))
}

func profilesPin(args []string) error {
	fs := flag.NewFlagSet("profiles pin", flag.ContinueOnError)
	origin := fs.String("origin", "", "https origin")
	account := fs.String("account", "", "expected account")
	label := fs.String("label", "", "short label")
	policyPath := fs.String("policy", "", "browser-profiles.json path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *origin == "" {
		return fmt.Errorf("usage: brwctl profiles pin NAME --origin https://…")
	}
	path, err := profilesPolicyPath(*policyPath)
	if err != nil {
		return err
	}
	return profileroster.AddPin(path, fs.Arg(0), *origin, *account, *label)
}

func profilesCopy(mode string, args []string) error {
	fs := flag.NewFlagSet("profiles "+mode, flag.ContinueOnError)
	from := fs.String("from", "", "source profile")
	to := fs.String("to", "", "destination profile")
	domain := fs.String("domain", "", "registrable domain")
	policyPath := fs.String("policy", "", "browser-profiles.json path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" || *to == "" || *domain == "" {
		return fmt.Errorf("usage: brwctl profiles %s --from A --to B --domain example.com", mode)
	}
	path, err := profilesPolicyPath(*policyPath)
	if err != nil {
		return err
	}
	policy, err := profilepolicy.Load(path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	result, err := profileroster.CopyDomain(ctx, policy, *from, *to, *domain, mode)
	if err != nil {
		return err
	}
	return printJSON(result)
}

func profilesGUI(args []string) error {
	fs := flag.NewFlagSet("profiles gui", flag.ContinueOnError)
	policyPath := fs.String("policy", "", "browser-profiles.json path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := profilesPolicyPath(*policyPath)
	if err != nil {
		return err
	}
	policy, err := profilepolicy.Load(path)
	if err != nil {
		return err
	}
	for _, p := range policy.Profiles {
		if p.BridgeHTTPAddr != "" {
			addr := p.BridgeHTTPAddr
			if !strings.Contains(addr, "://") {
				addr = "http://" + addr
			}
			fmt.Println(strings.TrimRight(addr, "/") + "/profiles")
			fmt.Fprintln(os.Stderr, "hidden until enabled in the brw extension Options → Advanced → Profile manager")
			return nil
		}
	}
	return fmt.Errorf("no daemon HTTP address in the policy; run brwctl setup or brwctl profiles create")
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
