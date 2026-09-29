package profileroster

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/discovery"
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/setup"
)

const googleAccountsOrigin = "https://accounts.google.com"

// LoadBoard builds the roster from the policy and whichever daemons answer.
func LoadBoard(ctx context.Context, policyPath string) (Board, error) {
	policy, err := profilepolicy.Load(policyPath)
	if err != nil {
		return Board{}, err
	}
	wells := []Well{}
	for _, profile := range policy.Profiles {
		if !profile.DirectCDPAllowed && !profile.ExtensionBridgeAllowed {
			continue
		}
		rec := discovery.Probe(profile, 2*time.Second)
		account := GoogleAccount(profile)
		for _, pin := range profile.Pins {
			if pinDomain(pin.Origin) == "accounts.google.com" && pin.Account != "" {
				account = pin.Account
				break
			}
		}
		owned := profile.DirectCDPAllowed && !isRealProfile(profile.UserDataDir)
		well := Well{
			Name:        profile.Name,
			Account:     account,
			Namespace:   Namespace(profile.Name),
			Workspace:   Workspace(profile.Name),
			Transport:   setup.ResolvedTransport(profile),
			Reachable:   rec.Reachable,
			DaemonError: rec.Error,
			UserDataDir: profile.UserDataDir,
			HTTPAddr:    rec.HTTPAddr,
			Pins:        profile.Pins,
		}
		if rec.Identity != nil && rec.Identity.Transport != "" {
			well.Transport = rec.Identity.Transport
		}
		if owned && rec.Reachable {
			if chips, err := liveChips(ctx, policy, profile, account); err == nil {
				well.Chips = chips
				well.AcceptsDrop = true
				well.OffersDrag = true
			} else {
				well.DaemonError = err.Error()
			}
		}
		if well.Chips == nil {
			well.Chips = chipsFromCookies(nil, profile.Pins, account, time.Now())
		}
		wells = append(wells, well)
	}
	return Board{Wells: wells}, nil
}

func liveChips(ctx context.Context, policy profilepolicy.Policy, profile profilepolicy.Profile, account string) ([]Chip, error) {
	lane, err := openCookieLane(ctx, policy, profile.Name, "board")
	if err != nil {
		return nil, err
	}
	defer lane.release()
	origins := []string{googleAccountsOrigin}
	for _, pin := range profile.Pins {
		if d := pinDomain(pin.Origin); d != "" {
			origins = append(origins, "https://"+d)
		}
	}
	var cookies []browser.Cookie
	seen := map[string]bool{}
	for _, origin := range origins {
		listed, err := lane.ctrl.Cookies(ctx, browser.CookieParams{Action: browser.CookieActionList, URL: origin})
		if err != nil {
			continue
		}
		for _, c := range listed.Cookies {
			key := cookieKey(c)
			if seen[key] {
				continue
			}
			seen[key] = true
			cookies = append(cookies, c)
		}
	}
	return chipsFromCookies(cookies, profile.Pins, account, time.Now()), nil
}

// AddPin records an expected session on a profile in the policy file.
func AddPin(policyPath, name, origin, account, label string) error {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return fmt.Errorf("origin must be an http(s) URL such as https://example.com, got %q", origin)
	}
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
		Origin:  u.Scheme + "://" + u.Host,
		Account: account,
	})
	_, err = setup.WritePolicy(policyPath, existing, time.Now())
	return err
}
