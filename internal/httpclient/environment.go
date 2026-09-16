package httpclient

import (
	"context"

	"github.com/Don-Works/brw/internal/browser"
)

// The proxy forwards page-environment overrides to the daemon that owns the
// browser. It implements the capability unconditionally: whether the override is
// actually possible is the upstream transport's answer, and the upstream returns
// browser.ErrEnvironmentUnsupported verbatim when it is not.

var _ browser.EnvironmentController = (*Controller)(nil)
var _ browser.InitScriptController = (*Controller)(nil)
var _ browser.TouchController = (*Controller)(nil)
var _ browser.ProfilerController = (*Controller)(nil)
var _ browser.ReactController = (*Controller)(nil)
var _ browser.ScrollToController = (*Controller)(nil)

func (c *Controller) SetGeolocation(ctx context.Context, opts browser.GeolocationOptions) (browser.EnvironmentResult, error) {
	return c.postEnvironment(ctx, "/api/page/geolocation", opts)
}

func (c *Controller) SetNetworkConditions(ctx context.Context, opts browser.NetworkConditionsOptions) (browser.EnvironmentResult, error) {
	return c.postEnvironment(ctx, "/api/page/network_conditions", opts)
}

func (c *Controller) EmulateMedia(ctx context.Context, opts browser.MediaEmulationOptions) (browser.EnvironmentResult, error) {
	return c.postEnvironment(ctx, "/api/page/emulate_media", opts)
}

func (c *Controller) SetLocale(ctx context.Context, opts browser.LocaleOptions) (browser.EnvironmentResult, error) {
	return c.postEnvironment(ctx, "/api/page/locale", opts)
}

func (c *Controller) InitScript(ctx context.Context, opts browser.InitScriptOptions) (browser.InitScriptResult, error) {
	var out browser.InitScriptResult
	err := c.post(ctx, "/api/page/init_script", opts, &out)
	return out, err
}

func (c *Controller) Touch(ctx context.Context, opts browser.TouchOptions) (browser.ActionResult, error) {
	var out browser.ActionResult
	err := c.post(ctx, "/api/page/touch", opts, &out)
	return out, err
}

func (c *Controller) Profile(ctx context.Context, opts browser.ProfileOptions) (browser.ProfileResult, error) {
	var out browser.ProfileResult
	err := c.post(ctx, "/api/page/profile", opts, &out)
	return out, err
}

func (c *Controller) React(ctx context.Context, opts browser.ReactOptions) (browser.ReactResult, error) {
	var out browser.ReactResult
	err := c.post(ctx, "/api/page/react", opts, &out)
	return out, err
}

func (c *Controller) ScrollTo(ctx context.Context, target string) (browser.ActionResult, error) {
	var out browser.ActionResult
	err := c.post(ctx, "/api/page/scroll", map[string]any{"target": target}, &out)
	return out, err
}

func (c *Controller) SetExtraHeaders(ctx context.Context, opts browser.ExtraHeadersOptions) (browser.EnvironmentResult, error) {
	return c.postEnvironment(ctx, "/api/page/extra_headers", opts)
}

func (c *Controller) SetUserAgent(ctx context.Context, opts browser.UserAgentOptions) (browser.EnvironmentResult, error) {
	return c.postEnvironment(ctx, "/api/page/user_agent", opts)
}

func (c *Controller) Authenticate(ctx context.Context, opts browser.CredentialsOptions) (browser.EnvironmentResult, error) {
	return c.postEnvironment(ctx, "/api/page/authenticate", opts)
}

func (c *Controller) SetDownloadPath(ctx context.Context, opts browser.DownloadPathOptions) (browser.EnvironmentResult, error) {
	return c.postEnvironment(ctx, "/api/browser/download_path", opts)
}

func (c *Controller) postEnvironment(ctx context.Context, path string, body any) (browser.EnvironmentResult, error) {
	var out browser.EnvironmentResult
	err := c.post(ctx, path, body, &out)
	return out, err
}
