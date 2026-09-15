package profileroster

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/profilepolicy"
)

// RegistrableDomain collapses a cookie host_key to a chip key.
func RegistrableDomain(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimPrefix(host, ".")
	if strings.HasPrefix(host, "www.") {
		host = strings.TrimPrefix(host, "www.")
	}
	return host
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
		return RegistrableDomain(origin)
	}
	return RegistrableDomain(u.Hostname())
}

// GoogleAccount reads Chrome Preferences for the signed-in Gaia email.
func GoogleAccount(profile profilepolicy.Profile) string {
	dir := profileDirectory(profile)
	if dir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, "Preferences"))
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
		if strings.TrimSpace(a.Email) != "" {
			return strings.TrimSpace(a.Email)
		}
	}
	return ""
}

func profileDirectory(profile profilepolicy.Profile) string {
	udd := strings.TrimSpace(profile.UserDataDir)
	if udd == "" {
		return ""
	}
	inner := profile.ProfileDirectory
	if inner == "" {
		inner = "Default"
	}
	return filepath.Join(udd, inner)
}

func chipsFromCookies(cookies []browser.Cookie, pins []profilepolicy.Pin, account string) []Chip {
	byDomain := map[string]*Chip{}
	now := float64(time.Now().Unix())
	for _, c := range cookies {
		d := RegistrableDomain(c.Domain)
		if d == "" {
			continue
		}
		chip := byDomain[d]
		if chip == nil {
			chip = &Chip{Domain: d, Health: HealthUnknown}
			byDomain[d] = chip
		}
		if c.Name != "" {
			chip.Names = appendUnique(chip.Names, c.Name)
		}
		expired := !c.Session && c.Expires > 0 && c.Expires < now
		if expired && chip.Health != HealthSignedIn {
			chip.Health = HealthExpired
		} else if !expired {
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
			chip = &Chip{Domain: d, Health: HealthMissing, Account: pin.Account}
			byDomain[d] = chip
		}
		chip.Pinned = true
		if chip.Account == "" {
			chip.Account = pin.Account
		}
	}
	out := make([]Chip, 0, len(byDomain))
	for _, chip := range byDomain {
		if chip.Account == "" {
			chip.Account = account
		}
		out = append(out, *chip)
	}
	return out
}

func appendUnique(list []string, v string) []string {
	for _, e := range list {
		if e == v {
			return list
		}
	}
	return append(list, v)
}
