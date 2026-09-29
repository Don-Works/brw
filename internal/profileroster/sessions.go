package profileroster

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/profilepolicy"
)

// RegistrableDomain collapses a cookie domain to the host a chip is keyed by:
// lower case, without a leading dot or a leading "www.".
func RegistrableDomain(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimPrefix(host, ".")
	return strings.TrimPrefix(host, "www.")
}

func pinDomain(origin string) string {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return ""
	}
	if !strings.Contains(origin, "://") {
		origin = "https://" + origin
	}
	u, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	return RegistrableDomain(u.Hostname())
}

// GoogleAccount reads the signed-in Google account from a brw-owned profile's
// Preferences. A daily browser profile is never read.
func GoogleAccount(profile profilepolicy.Profile) string {
	udd := strings.TrimSpace(profile.UserDataDir)
	if udd == "" || isRealProfile(udd) {
		return ""
	}
	inner := profile.ProfileDirectory
	if inner == "" {
		inner = "Default"
	}
	data, err := os.ReadFile(filepath.Join(profilepolicy.ExpandPath(udd), inner, "Preferences"))
	if err != nil {
		return ""
	}
	var prefs struct {
		AccountInfo []struct {
			Email string `json:"email"`
		} `json:"account_info"`
	}
	if json.Unmarshal(data, &prefs) != nil {
		return ""
	}
	for _, a := range prefs.AccountInfo {
		if email := strings.TrimSpace(a.Email); email != "" {
			return email
		}
	}
	return ""
}

func chipsFromCookies(cookies []browser.Cookie, pins []profilepolicy.Pin, account string, now time.Time) []Chip {
	byDomain := map[string]*Chip{}
	unix := float64(now.Unix())
	for _, c := range cookies {
		d := RegistrableDomain(c.Domain)
		if d == "" {
			continue
		}
		chip := byDomain[d]
		if chip == nil {
			chip = &Chip{Domain: d, Health: HealthExpired}
			byDomain[d] = chip
		}
		if c.Name != "" && !contains(chip.Names, c.Name) {
			chip.Names = append(chip.Names, c.Name)
		}
		if c.Session || c.Expires <= 0 || c.Expires >= unix {
			chip.Health = HealthSignedIn
		}
	}
	for _, pin := range pins {
		d := pinDomain(pin.Origin)
		if d == "" {
			continue
		}
		chip := byDomain[d]
		if chip == nil {
			chip = &Chip{Domain: d, Health: HealthMissing}
			byDomain[d] = chip
		}
		chip.Pinned = true
		if chip.Account == "" {
			chip.Account = pin.Account
		}
	}
	out := make([]Chip, 0, len(byDomain))
	for _, chip := range byDomain {
		if chip.Account == "" && strings.HasSuffix(chip.Domain, "google.com") {
			chip.Account = account
		}
		sort.Strings(chip.Names)
		out = append(out, *chip)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out
}

func contains(list []string, v string) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
}
