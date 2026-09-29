package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/discovery"
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/profileroster"
)

const profilesUsage = `usage: brwctl profiles <command> [--profile-policy PATH]

commands:
  list                          the roster as JSON: every profile, its daemon, and the sites it holds
  sessions NAME                 one profile from the roster
  create NAME [--account A]     add a brw-owned direct-CDP profile with its own directory and port
  pin NAME --origin URL         record a login the profile is expected to hold
  copy --from A --to B --domain D
  move --from A --to B --domain D
                                copy (or move) one site's cookies between two brw-owned profiles
  gui                           print the loopback URL of the roster page`

func profilesCommand(args []string) error {
	return runProfiles(args, os.Stdout)
}

func runProfiles(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(profilesUsage)
	}
	fs := flag.NewFlagSet("profiles "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	policyPath := fs.String("profile-policy", os.Getenv("BRW_PROFILE_POLICY"), "profile policy JSON path")
	account := fs.String("account", "", "the Google account the new profile should use")
	browserName := fs.String("browser", "chromium", "browser for a new profile")
	origin := fs.String("origin", "", "origin of the expected login")
	label := fs.String("label", "", "short label for the pin")
	from := fs.String("from", "", "source profile")
	to := fs.String("to", "", "destination profile")
	domain := fs.String("domain", "", "site whose cookies to copy, for example example.com")
	if err := fs.Parse(interleaved(args[1:], fs)); err != nil {
		return fmt.Errorf("%w\n%s", err, profilesUsage)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	switch args[0] {
	case "list", "sessions":
		path, err := resolvedPolicyPath(*policyPath)
		if err != nil {
			return err
		}
		board, err := profileroster.LoadBoard(ctx, path)
		if err != nil {
			return err
		}
		if args[0] == "list" {
			writeJSON(out, board)
			return nil
		}
		if fs.NArg() != 1 {
			return errors.New("usage: brwctl profiles sessions NAME")
		}
		for _, well := range board.Wells {
			if well.Name == fs.Arg(0) {
				writeJSON(out, well)
				return nil
			}
		}
		return fmt.Errorf("profile %q is not in the roster", fs.Arg(0))
	case "create":
		if fs.NArg() != 1 {
			return errors.New("usage: brwctl profiles create NAME [--account EMAIL]")
		}
		exe, _ := os.Executable()
		result, err := profileroster.Create(profileroster.CreateRequest{
			Name:       fs.Arg(0),
			Account:    *account,
			Browser:    *browserName,
			PolicyPath: *policyPath,
			BRWDPath:   brwdPath(defaultAppDir(), exe, runtime.GOOS, execRunner{}.look),
		})
		if err != nil {
			return err
		}
		writeJSON(out, result)
		return nil
	case "pin":
		if fs.NArg() != 1 || *origin == "" {
			return errors.New("usage: brwctl profiles pin NAME --origin https://example.com [--account A] [--label L]")
		}
		path, err := resolvedPolicyPath(*policyPath)
		if err != nil {
			return err
		}
		return profileroster.AddPin(path, fs.Arg(0), *origin, *account, *label)
	case "copy", "move":
		if *from == "" || *to == "" || *domain == "" {
			return fmt.Errorf("usage: brwctl profiles %s --from A --to B --domain example.com", args[0])
		}
		policy, err := profilepolicy.Load(*policyPath)
		if err != nil {
			return err
		}
		result, err := profileroster.CopyDomain(ctx, policy, *from, *to, *domain, args[0])
		if err != nil {
			return err
		}
		writeJSON(out, result)
		return nil
	case "gui":
		policy, err := profilepolicy.Load(*policyPath)
		if err != nil {
			return err
		}
		for _, p := range policy.Profiles {
			if !p.DirectCDPAllowed && !p.ExtensionBridgeAllowed {
				continue
			}
			if rec := discovery.Probe(p, 2*time.Second); rec.Reachable {
				fmt.Fprintln(out, strings.TrimRight(rec.HTTPAddr, "/")+"/profiles")
				return nil
			}
		}
		return errors.New("no profile daemon in the policy is running; start one, then open http://127.0.0.1:<its port>/profiles")
	default:
		return fmt.Errorf("unknown profiles command %q\n%s", args[0], profilesUsage)
	}
}

func resolvedPolicyPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	return profilepolicy.Discover("")
}

// interleaved moves flags ahead of positionals, so `create bookkeeper
// --account a@b` parses the same as `create --account a@b bookkeeper`.
func interleaved(args []string, fs *flag.FlagSet) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if f := fs.Lookup(name); f != nil && i+1 < len(args) {
			if b, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !b.IsBoolFlag() {
				flags = append(flags, args[i+1])
				i++
			}
		}
	}
	return append(flags, positional...)
}
