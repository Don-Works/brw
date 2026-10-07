package siteconsent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AdminConfig is the operator-supplied half of the consent model: the decisions a managed machine makes centrally so nobody has to click through a UI on every desk.
type AdminConfig struct {
	// AllowedOrigins are hosts (or bare domains, subdomains included) this machine may drive with no prompt and no stored grant.
	AllowedOrigins []string `json:"allowed_origins,omitempty"`
	// BlockedOrigins are hosts this machine may never drive.
	BlockedOrigins []string `json:"blocked_origins,omitempty"`
	// CategoryDomains extends (or adds to) the shipped blocklist categories.
	CategoryDomains map[string][]string `json:"category_domains,omitempty"`
	// ConfirmActions turns on the high-risk action confirmation gate.
	ConfirmActions bool `json:"confirm_actions,omitempty"`
	// DefaultGrantTTL bounds how long a recorded grant authorises for.
	DefaultGrantTTL string `json:"default_grant_ttl,omitempty"`

	ttl time.Duration
}

// LoadAdminConfig reads a consent admin config.
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
	var config *AdminConfig
	decoder := json.NewDecoder(bytes.NewReader(data))

	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return AdminConfig{}, fmt.Errorf("read consent admin config %s: %w", path, err)
	}
	if config == nil {
		return AdminConfig{}, fmt.Errorf("read consent admin config %s: expected a JSON object", path)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return AdminConfig{}, fmt.Errorf("read consent admin config %s: trailing JSON data", path)
	}
	if err := config.normalize(); err != nil {
		return AdminConfig{}, fmt.Errorf("read consent admin config %s: %w", path, err)
	}
	return *config, nil
}

const (
	storeFileName = "site-grants.json"
	keyFileName   = "site-consent.key"
	adminFileName = "site-consent.json"
)

// StorePath is the grant file inside a brw config directory.
func StorePath(dir string) string { return filepath.Join(dir, storeFileName) }

// KeyPath is the MAC key file inside a brw config directory.
func KeyPath(dir string) string { return filepath.Join(dir, keyFileName) }

// DefaultDir is the brw config directory the consent files live in when no profile policy path is known.
func DefaultDir() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "brw"), nil
}

// DirForPolicy returns the directory the consent files belong in for a given profile policy path.
func DirForPolicy(policyPath string) (string, error) {
	if strings.TrimSpace(policyPath) == "" {
		return DefaultDir()
	}
	return filepath.Dir(policyPath), nil
}

// DiscoverAdminConfigPath returns the standard admin config path beside a profile policy file.
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
