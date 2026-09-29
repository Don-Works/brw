package profileroster

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/discovery"
	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/profilepolicy"
)

// ErrRefused marks a copy the roster will not perform, as opposed to one that
// failed.
var ErrRefused = errors.New("refused")

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrRefused}, args...)...)
}

// isRealProfile reports whether a user-data-dir belongs to a browser a human
// uses day to day.
func isRealProfile(userDataDir string) bool {
	dir := strings.TrimSpace(userDataDir)
	if dir == "" {
		return false
	}
	return cdp.IsInsideRealBrowserProfile(profilepolicy.ExpandPath(dir))
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// rosterOwner is the tab-lease owner every roster call uses, so it never runs
// inside, or releases, the lease of the agent session that launched it.
const rosterOwner = "brw-profile-roster"

func rosterClient(base string, timeout time.Duration) (*httpclient.Controller, error) {
	ctrl, err := httpclient.New(base, timeout)
	if err != nil {
		return nil, err
	}
	ctrl.UseOwner(rosterOwner)
	return ctrl, nil
}

// release hands back the tab the roster's calls leased, and closes any tab the
// daemon opened for them.
func (l cookieLane) release() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = l.ctrl.ReleaseSession(ctx, true)
}

// cookieLane is a brw-owned direct-CDP profile whose daemon answered on this
// machine as that profile.
type cookieLane struct {
	profile profilepolicy.Profile
	ctrl    *httpclient.Controller
}

// openCookieLane is the whole gate a copy passes before any cookie is read or
// written. The same rules as brw_state apply, and stricter: both ends must be a
// browser brw launched itself on this host, so the extension bridge, the Chrome
// opt-in lane, a --remote attach and an off-host browser are all refused, and
// neither end may sit in a human's daily browser profile directory, whether the
// policy or the live daemon names it.
func openCookieLane(ctx context.Context, policy profilepolicy.Policy, name, role string) (cookieLane, error) {
	profile, err := policy.Find(name)
	if err != nil {
		return cookieLane{}, err
	}
	if !profile.DirectCDPAllowed {
		return cookieLane{}, refuse("%s profile %q is not a brw-owned direct-CDP profile", role, name)
	}
	if isRealProfile(profile.UserDataDir) {
		return cookieLane{}, refuse("%s profile %q points at a daily browser profile directory (%s)", role, name, profile.UserDataDir)
	}
	base := discovery.HTTPURL(profile)
	if !isLoopbackURL(base) {
		return cookieLane{}, refuse("%s profile %q has a daemon address off this machine (%s)", role, name, base)
	}
	ctrl, err := rosterClient(base, 20*time.Second)
	if err != nil {
		return cookieLane{}, err
	}
	health, err := ctrl.Health(ctx)
	if err != nil {
		return cookieLane{}, fmt.Errorf("%s profile %q: daemon at %s is not reachable: %w", role, name, base, err)
	}
	id := health.Identity
	if id.Profile != name {
		return cookieLane{}, refuse("%s daemon at %s answers as profile %q, not %q", role, base, id.Profile, name)
	}
	if id.Transport != brwidentity.TransportDirectCDP {
		return cookieLane{}, refuse("%s profile %q runs on the %q transport; copying sessions needs a browser brw launched itself on this machine (%s)", role, name, id.Transport, brwidentity.TransportDirectCDP)
	}
	if isRealProfile(id.UserDataDir) {
		return cookieLane{}, refuse("%s daemon for %q drives a daily browser profile directory (%s)", role, name, id.UserDataDir)
	}
	return cookieLane{profile: profile, ctrl: ctrl}, nil
}
