package extensionbridge

import (
	"context"

	"github.com/Don-Works/brw/internal/browser"
)

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

func (b *Bridge) SetLocale(context.Context, browser.LocaleOptions) (browser.EnvironmentResult, error) {
	return browser.EnvironmentResult{}, browser.ErrEnvironmentUnsupported
}

func (b *Bridge) InitScript(context.Context, browser.InitScriptOptions) (browser.InitScriptResult, error) {
	return browser.InitScriptResult{}, browser.ErrInitScriptUnsupported
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
