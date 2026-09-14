package siteconsent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AdminConfig is the operator-supplied half of the consent model: the decisions
// a managed machine makes centrally so nobody has to click through a UI on every
// desk.
//
// AllowedOrigins is a standing yes and AllowedOrigins entries are never written
// into the grant store: a managed allowlist that turned into local records would
// survive being removed from the config, which is the opposite of managed.
type AdminConfig struct {
	// AllowedOrigins are hosts (or bare domains, subdomains included) this
	// machine may drive with no prompt and no stored grant.
	AllowedOrigins []string `json:"allowed_origins,omitempty"`
	// BlockedOrigins are hosts this machine may never drive. No prompt, no
	// grant and no category override reaches past them.
	BlockedOrigins []string `json:"blocked_origins,omitempty"`
	// CategoryDomains extends (or adds to) the shipped blocklist categories.
	CategoryDomains map[string][]string `json:"category_domains,omitempty"`
	// ConfirmActions turns on the high-risk action confirmation gate.
	ConfirmActions bool `json:"confirm_actions,omitempty"`
	// DefaultGrantTTL bounds how long a recorded grant authorises for. Empty or
	// "0" means a grant does not expire on its own.
	DefaultGrantTTL string `json:"default_grant_ttl,omitempty"`

	ttl time.Duration
}

// LoadAdminConfig reads a consent admin config. A missing file is not an error:
// an unmanaged machine has no admin config and must behave as if the fields were
// all empty.
func LoadAdminConfig(path string) (AdminConfig, error) {
	if strings.TrimSpace(path) == "" {
		return AdminConfig{}, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return AdminConfig{}, nil
	}
	if err != nil {
		return AdminConfig{}, err
	}
	var config AdminConfig
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	// Unknown fields are refused rather than ignored. A managed config with a
	// typo'd key that silently does nothing is an operator who believes a
	// restriction is in force when it is not.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return AdminConfig{}, fmt.Errorf("read consent admin config %s: %w", path, err)
	}
	if err := config.normalize(); err != nil {
		return AdminConfig{}, fmt.Errorf("read consent admin config %s: %w", path, err)
	}
	return config, nil
}

// The consent files live beside the profile policy, not beside the session or
// the cache: a grant is a property of the profile, and moving the profile policy
// to another machine without its grants would silently widen what an agent may
// do there.
const (
	storeFileName = "site-grants.json"
	keyFileName   = "site-consent.key"
	adminFileName = "site-consent.json"
)

// StorePath is the grant file inside a brw config directory.
func StorePath(dir string) string { return filepath.Join(dir, storeFileName) }

// KeyPath is the MAC key file inside a brw config directory.
func KeyPath(dir string) string { return filepath.Join(dir, keyFileName) }

// DefaultDir is the brw config directory the consent files live in when no
// profile policy path is known.
func DefaultDir() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "brw"), nil
}

// DirForPolicy returns the directory the consent files belong in for a given
// profile policy path. An empty policy path falls back to DefaultDir.
func DirForPolicy(policyPath string) (string, error) {
	if strings.TrimSpace(policyPath) == "" {
		return DefaultDir()
	}
	return filepath.Dir(policyPath), nil
}

// DiscoverAdminConfigPath returns the standard admin config path beside a
// profile policy file.
func DiscoverAdminConfigPath(policyPath string) string {
	if strings.TrimSpace(policyPath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(policyPath), adminFileName)
}

func (c *AdminConfig) normalize() error {
	c.AllowedOrigins = normalizeDomains(c.AllowedOrigins)
	c.BlockedOrigins = normalizeDomains(c.BlockedOrigins)
	for name, domains := range c.CategoryDomains {
		c.CategoryDomains[name] = normalizeDomains(domains)
	}
	raw := strings.TrimSpace(c.DefaultGrantTTL)
	if raw == "" || raw == "0" {
		c.ttl = 0
		return nil
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("default_grant_ttl %q: %w", c.DefaultGrantTTL, err)
	}
	if ttl < 0 {
		return fmt.Errorf("default_grant_ttl %q is negative", c.DefaultGrantTTL)
	}
	c.ttl = ttl
	return nil
}

// Normalize prepares a hand-built config (one not read from a file) for use.
func (c *AdminConfig) Normalize() error { return c.normalize() }

func normalizeDomains(entries []string) []string {
	out := make([]string, 0, len(entries))
	seen := map[string]bool{}
	for _, entry := range entries {
		host := HostOfOrigin(entry)
		if host == "" {
			host = asciiHost(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(entry), "*."), "."))
		}
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		out = append(out, host)
	}
	return out
}

func (c AdminConfig) blocks(host string) (string, bool) {
	for _, entry := range c.BlockedOrigins {
		if hostMatches(host, entry) {
			return entry, true
		}
	}
	return "", false
}

func (c AdminConfig) allows(host string) bool {
	for _, entry := range c.AllowedOrigins {
		if hostMatches(host, entry) {
			return true
		}
	}
	return false
}

func (c AdminConfig) expiryFrom(now time.Time) time.Time {
	if c.ttl <= 0 {
		return time.Time{}
	}
	return now.Add(c.ttl)
}
