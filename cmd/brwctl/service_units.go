package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Don-Works/brw/internal/setup"
)

// brwdServiceUnits lists every per-user service unit that runs brwdPath,
// whatever the unit is called. `brwctl setup` names its own units, but a
// machine set up by hand carries units under other labels, and an upgrade that
// only restarts the names it knows leaves those daemons on the old build.
func brwdServiceUnits(goos, home, brwdPath string) []setup.ServiceUnit {
	if goos != "darwin" && goos != "linux" {
		return nil
	}
	dir := filepath.Dir(setup.ServiceParams{GOOS: goos, Profile: "brwd", Home: home}.UnitPath())
	units, err := setup.ScanServiceUnits(dir, goos)
	if err != nil {
		return nil
	}
	want := canonicalPath(brwdPath)
	var matched []setup.ServiceUnit
	for _, unit := range units {
		if unit.Label == "" {
			continue
		}
		for _, token := range unit.Tokens {
			if canonicalPath(strings.Trim(token, `"'`)) == want {
				matched = append(matched, unit)
				break
			}
		}
	}
	return matched
}

// unitProfile is the --profile a unit passes to brwd, or "".
func unitProfile(unit setup.ServiceUnit) string {
	for i, token := range unit.Tokens {
		token = strings.Trim(token, `"'`)
		if (token == "--profile" || token == "-profile") && i+1 < len(unit.Tokens) {
			return strings.Trim(unit.Tokens[i+1], `"'`)
		}
		for _, prefix := range []string{"--profile=", "-profile="} {
			if value, ok := strings.CutPrefix(token, prefix); ok {
				return value
			}
		}
	}
	return ""
}

func canonicalPath(path string) string {
	if !filepath.IsAbs(path) {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

func serviceRestartArgsForLabel(goos, label string) []string {
	switch goos {
	case "darwin":
		return []string{"launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), label)}
	case "windows":
		return []string{"schtasks", "/Run", "/TN", label}
	default:
		return []string{"systemctl", "--user", "restart", label + ".service"}
	}
}
