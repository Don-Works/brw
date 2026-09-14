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

func (c *Controller) SetGeolocation(ctx context.Context, opts browser.GeolocationOptions) (browser.EnvironmentResult, error) {
	return c.postEnvironment(ctx, "/api/page/geolocation", opts)
}

func (c *Controller) SetNetworkConditions(ctx context.Context, opts browser.NetworkConditionsOptions) (browser.EnvironmentResult, error) {
	return c.postEnvironment(ctx, "/api/page/network_conditions", opts)
}

func (c *Controller) EmulateMedia(ctx context.Context, opts browser.MediaEmulationOptions) (browser.EnvironmentResult, error) {
	return c.postEnvironment(ctx, "/api/page/emulate_media", opts)
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
