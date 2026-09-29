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
)

// Open asks a profile's own daemon to open rawURL, so that daemon's navigation
// policy and site-consent gate decide it as they would for an agent.
func Open(ctx context.Context, policy profilepolicy.Policy, name, rawURL string) (browser.OpenResult, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return browser.OpenResult{}, fmt.Errorf("url must be an http(s) URL, got %q", rawURL)
	}
	profile, err := policy.Find(name)
	if err != nil {
		return browser.OpenResult{}, err
	}
	base := discovery.HTTPURL(profile)
	if !isLoopbackURL(base) {
		return browser.OpenResult{}, refuse("profile %q has a daemon address off this machine (%s)", name, base)
	}
	ctrl, err := rosterClient(base, 30*time.Second)
	if err != nil {
		return browser.OpenResult{}, err
	}
	health, err := ctrl.Health(ctx)
	if err != nil {
		return browser.OpenResult{}, fmt.Errorf("profile %q: daemon at %s is not reachable: %w", name, base, err)
	}
	if health.Identity.Profile != name {
		return browser.OpenResult{}, refuse("daemon at %s answers as profile %q, not %q", base, health.Identity.Profile, name)
	}
	result, err := ctrl.Open(ctx, u.String())
	if err == nil {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ctrl.ReleaseSession(releaseCtx, false)
	}
	return result, err
}
