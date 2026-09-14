package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/user"
	"strings"

	"github.com/Don-Works/brw/internal/siteconsent"
)

type siteConsentOptions struct {
	enabled    bool
	configPath string
	policyPath string
	prompt     bool
	mcpMode    bool
	confirm    bool
}

// buildSiteConsent assembles the consent guard from the daemon's flags.
//
// It returns a nil guard when consent is off, which every consumer treats as
// "no gate". Everything that would only matter with consent on is refused
// loudly here rather than accepted and ignored: a daemon started with
// --confirm-actions and no --site-consent would otherwise run with no
// confirmation at all while its operator believed the opposite.
func buildSiteConsent(opts siteConsentOptions) (*siteconsent.Guard, error) {
	if !opts.enabled {
		if opts.confirm {
			return nil, errors.New("--confirm-actions needs --site-consent: the risk gate is part of the consent surface and does nothing on its own")
		}
		if opts.prompt {
			return nil, errors.New("--site-consent-prompt needs --site-consent: there is nothing to ask about without a consent store")
		}
		return nil, nil
	}

	dir, err := siteconsent.DirForPolicy(opts.policyPath)
	if err != nil {
		return nil, fmt.Errorf("resolve the consent directory: %w", err)
	}
	store, err := siteconsent.OpenStore(siteconsent.StorePath(dir), siteconsent.KeyPath(dir))
	if err != nil {
		return nil, err
	}

	configPath := strings.TrimSpace(opts.configPath)
	if configPath == "" {
		configPath = siteconsent.DiscoverAdminConfigPath(siteconsent.StorePath(dir))
	}
	admin, err := siteconsent.LoadAdminConfig(configPath)
	if err != nil {
		return nil, err
	}
	// The flag turns the gate ON; it can never turn an admin config's gate off.
	// An operator who set confirm_actions on a managed machine has made a
	// decision the local command line must not be able to undo.
	admin.ConfirmActions = admin.ConfirmActions || opts.confirm
	if err := admin.Normalize(); err != nil {
		return nil, err
	}

	guard, err := siteconsent.NewGuard(store, admin)
	if err != nil {
		return nil, err
	}
	guard.SetGrantor(consentGrantor())

	if opts.prompt {
		switch {
		case opts.mcpMode:
			return nil, errors.New("--site-consent-prompt cannot be used with --mcp: the MCP transport owns stdin, so there is no terminal to ask on")
		case !stdinIsTerminal():
			return nil, errors.New("--site-consent-prompt needs a terminal on stdin; this daemon has none, so it would have nobody to ask")
		default:
			guard.SetPrompter(siteconsent.NewTerminalPrompter(os.Stdin, os.Stderr))
		}
	}

	mode := "non-interactive (un-granted origins are refused)"
	if guard.Interactive() {
		mode = "interactive (un-granted origins are asked about on this terminal)"
	}
	log.Printf("site consent active: %s, %s, confirm-actions=%v", store.Path(), mode, admin.ConfirmActions)
	if rejected := store.Rejected(); len(rejected) > 0 {
		// Loud, because the only way a record fails its MAC is that something
		// other than brw wrote to the file.
		for _, record := range rejected {
			log.Printf("WARNING: consent record for %s (%s) refused: %s", record.Origin, record.Scope, record.Reason)
		}
	}
	return guard, nil
}

// consentGrantor names who a recorded consent is attributed to: the OS account
// this daemon runs as, which is the only identity brw can actually observe.
func consentGrantor() string {
	if u, err := user.Current(); err == nil && strings.TrimSpace(u.Username) != "" {
		return u.Username
	}
	return "unknown"
}

// stdinIsTerminal reports whether stdin is a character device, which is what
// distinguishes "a person started this in a shell" from "a service manager gave
// it /dev/null".
func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
