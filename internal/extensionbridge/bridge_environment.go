package extensionbridge

import (
	"context"

	"github.com/Don-Works/brw/internal/browser"
)

// The extension bridge implements browser.EnvironmentController only to refuse
// it by name. Returning browser.ErrEnvironmentUnsupported from a real method is
// what makes the refusal legible: the MCP and HTTP layers type-assert the
// capability, so a bridge that simply did not implement it would answer with a
// generic "this transport does not support …" that says nothing about why, and
// nothing an agent could act on.
//
// Each override here is a DevTools Protocol session override held by the
// debugger session that installed it. The extension attaches and detaches
// chrome.debugger around operations, and a detach drops every override the
// session held — so an applied-then-evaporated override would be worse than an
// error: the caller would believe the page was offline, or in Tokyo, or sending
// a bearer token, when it was not.

// The refusal is only reachable if the bridge still satisfies the interface the
// MCP and HTTP layers assert on.
var _ browser.EnvironmentController = (*Bridge)(nil)

func (b *Bridge) SetGeolocation(context.Context, browser.GeolocationOptions) (browser.EnvironmentResult, error) {
	return browser.EnvironmentResult{}, browser.ErrEnvironmentUnsupported
}

func (b *Bridge) SetNetworkConditions(context.Context, browser.NetworkConditionsOptions) (browser.EnvironmentResult, error) {
	return browser.EnvironmentResult{}, browser.ErrEnvironmentUnsupported
}

func (b *Bridge) EmulateMedia(context.Context, browser.MediaEmulationOptions) (browser.EnvironmentResult, error) {
	return browser.EnvironmentResult{}, browser.ErrEnvironmentUnsupported
}

func (b *Bridge) SetExtraHeaders(context.Context, browser.ExtraHeadersOptions) (browser.EnvironmentResult, error) {
	return browser.EnvironmentResult{}, browser.ErrEnvironmentUnsupported
}

func (b *Bridge) SetUserAgent(context.Context, browser.UserAgentOptions) (browser.EnvironmentResult, error) {
	return browser.EnvironmentResult{}, browser.ErrEnvironmentUnsupported
}

func (b *Bridge) Authenticate(context.Context, browser.CredentialsOptions) (browser.EnvironmentResult, error) {
	return browser.EnvironmentResult{}, browser.ErrEnvironmentUnsupported
}

func (b *Bridge) SetDownloadPath(context.Context, browser.DownloadPathOptions) (browser.EnvironmentResult, error) {
	return browser.EnvironmentResult{}, browser.ErrEnvironmentUnsupported
}
