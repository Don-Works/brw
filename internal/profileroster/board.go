package profileroster

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/discovery"
	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/setup"
)

// LoadBoard builds the visual editor payload from the policy and live daemons.
func LoadBoard(ctx context.Context, policyPath string) (Board, error) {
	policy, err := profilepolicy.Load(policyPath)
	if err != nil {
		return Board{}, err
	}
	records, err := discovery.List(policyPath, 2*time.Second)
	if err != nil {
		return Board{}, err
	}
	byName := map[string]discovery.Record{}
	for _, rec := range records {
		byName[rec.Profile] = rec
	}

	var wells []Well
	for _, profile := range policy.Profiles {
		if !profile.DirectCDPAllowed && !profile.ExtensionBridgeAllowed {
			continue
		}
		rec := byName[profile.Name]
		account := GoogleAccount(profile)
		for _, pin := range profile.Pins {
			if strings.Contains(strings.ToLower(pin.Origin), "google") && pin.Account != "" {
				account = pin.Account
				break
			}
		}
		well := Well{
			Name:        profile.Name,
			DisplayName: profile.Name,
			Account:     account,
			Namespace:   Namespace(profile.Name),
			Workspace:   Workspace(profile.Name),
			Transport:   rec.Transport,
			Reachable:   rec.Reachable,
			DaemonError: rec.Error,
			AcceptsDrop: profile.DirectCDPAllowed && !isDailyChromeDir(profile.UserDataDir),
			SourceOnly:  profile.ExtensionBridgeAllowed && !profile.DirectCDPAllowed,
			UserDataDir: profile.UserDataDir,
			HTTPAddr:    discovery.HTTPURL(profile),
			Pins:        profile.Pins,
		}
		if well.Transport == "" {
			well.Transport = setup.ResolvedTransport(profile)
		}
		if rec.Reachable && profile.DirectCDPAllowed {
			if chips, err := liveChips(ctx, profile, account); err == nil {
				well.Chips = chips
			}
		}
		if len(well.Chips) == 0 {
			well.Chips = chipsFromCookies(nil, profile.Pins, account)
		}
		wells = append(wells, well)
	}
	return Board{Wells: wells}, nil
}

func liveChips(ctx context.Context, profile profilepolicy.Profile, account string) ([]Chip, error) {
	ctrl, err := httpclient.New(discovery.HTTPURL(profile), 8*time.Second)
	if err != nil {
		return nil, err
	}
	var cookies []browser.Cookie
	seen := map[string]bool{}
	origins := []string{"https://accounts.google.com"}
	for _, pin := range profile.Pins {
		if pin.Origin != "" {
			origins = append(origins, pin.Origin)
		}
	}
	for _, origin := range origins {
		listed, err := ctrl.Cookies(ctx, browser.CookieParams{Action: browser.CookieActionList, URL: origin})
		if err != nil {
			continue
		}
		for _, c := range listed.Cookies {
			key := c.Domain + "|" + c.Name + "|" + c.Path
			if seen[key] {
				continue
			}
			seen[key] = true
			cookies = append(cookies, c)
		}
	}
	return chipsFromCookies(cookies, profile.Pins, account), nil
}

// AddPin records an expected session on a profile.
func AddPin(policyPath, name, origin, account, label string) error {
	existing, found, err := setup.LoadPolicyFile(policyPath)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no policy at %s", policyPath)
	}
	idx := -1
	for i := range existing.Profiles {
		if existing.Profiles[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("profile %q is not in the policy", name)
	}
	d := pinDomain(origin)
	for _, p := range existing.Profiles[idx].Pins {
		if pinDomain(p.Origin) == d && p.Account == account {
			return nil
		}
	}
	if label == "" {
		label = d
	}
	existing.Profiles[idx].Pins = append(existing.Profiles[idx].Pins, profilepolicy.Pin{
		Label:   label,
		Origin:  origin,
		Account: account,
	})
	_, err = setup.WritePolicy(policyPath, existing, time.Now())
	return err
}
