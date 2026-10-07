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

		for _, record := range rejected {
			log.Printf("WARNING: consent record for %s (%s) refused: %s", record.Origin, record.Scope, record.Reason)
		}
	}
	return guard, nil
}

func consentGrantor() string {
	if u, err := user.Current(); err == nil && strings.TrimSpace(u.Username) != "" {
		return u.Username
	}
	return "unknown"
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
