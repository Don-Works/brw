package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/chromedp/cdproto"
	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	cdpe "github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// The MCP and HTTP layers reach these through a type assertion, so a signature
// that drifts would not fail the build — it would quietly turn every override
// into "this transport does not support it" on the transport that does.
var _ EnvironmentController = (*Manager)(nil)

// environmentState holds the per-tab page-environment overrides that CDP itself
// cannot report back: the scoped header table the request interceptor consults,
// the credential armed for one in-flight navigation, and the user-agent baseline
// needed to undo an override (Emulation has no clearUserAgentOverride).
//
// Zero value is usable; maps are created on first use.
type environmentState struct {
	mu          sync.Mutex
	headers     map[string][]OriginHeaders
	credentials map[string]*armedCredential
	uaBaselines map[string]string
	authCalls   map[string]*sync.Mutex
	// geoPermissions remembers, per ORIGIN, the geolocation permission that was
	// in force before brw granted its own. Chrome has no getPermission, and the
	// permission is browser-wide rather than per tab, so clearing an override
	// would otherwise revoke a grant the human made themselves.
	geoPermissions map[string]string
	// initScripts is the per-tab registry of Page.addScriptToEvaluateOnNewDocument
	// identifiers, which is the only handle CDP gives back for removing a script.
	initScripts map[string][]InitScript
}

// armedCredential is alive only between Authenticate arming it and the deferred
// drop at the end of that same call. It is never written to disk, a log, the
// trace, or a result.
type armedCredential struct {
	origin     string
	username   string
	password   string
	challenged bool
	answered   int
}

func (e *environmentState) initLocked() {
	if e.headers == nil {
		e.headers = map[string][]OriginHeaders{}
	}
	if e.credentials == nil {
		e.credentials = map[string]*armedCredential{}
	}
	if e.uaBaselines == nil {
		e.uaBaselines = map[string]string{}
	}
	if e.authCalls == nil {
		e.authCalls = map[string]*sync.Mutex{}
	}
	if e.geoPermissions == nil {
		e.geoPermissions = map[string]string{}
	}
	if e.initScripts == nil {
		e.initScripts = map[string][]InitScript{}
	}
}

func (e *environmentState) addInitScript(tabID string, script InitScript) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	e.initScripts[tabID] = append(e.initScripts[tabID], script)
}

func (e *environmentState) removeInitScript(tabID, id string) (InitScript, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	scripts := e.initScripts[tabID]
	for i, script := range scripts {
		if script.ID == id {
			e.initScripts[tabID] = append(scripts[:i], scripts[i+1:]...)
			if len(e.initScripts[tabID]) == 0 {
				delete(e.initScripts, tabID)
			}
			return script, true
		}
	}
	return InitScript{}, false
}

func (e *environmentState) listInitScripts(tabID string) []InitScript {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	return append([]InitScript(nil), e.initScripts[tabID]...)
}

func (e *environmentState) clearInitScripts(tabID string) []InitScript {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	scripts := append([]InitScript(nil), e.initScripts[tabID]...)
	delete(e.initScripts, tabID)
	return scripts
}

// authLock serialises Authenticate calls on one tab.
func (e *environmentState) authLock(tabID string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	lock, ok := e.authCalls[tabID]
	if !ok {
		lock = &sync.Mutex{}
		e.authCalls[tabID] = lock
	}
	return lock
}

// rememberGeoPermission records the permission state an origin had before brw
// granted its own, once. A second override on the same origin must not overwrite
// the first recording with the granted state brw itself installed.
func (e *environmentState) rememberGeoPermission(origin, state string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	if _, ok := e.geoPermissions[origin]; !ok {
		e.geoPermissions[origin] = state
	}
}

// takeGeoPermission returns the state to restore for an origin, and whether brw
// is the one that changed it. Not ok means brw granted nothing here and must
// leave the permission alone.
func (e *environmentState) takeGeoPermission(origin string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	state, ok := e.geoPermissions[origin]
	delete(e.geoPermissions, origin)
	return state, ok
}

func (e *environmentState) setHeaders(tabID string, entries []OriginHeaders) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	if len(entries) == 0 {
		delete(e.headers, tabID)
		return
	}
	e.headers[tabID] = entries
}

func (e *environmentState) listHeaders(tabID string) []OriginHeaders {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	return append([]OriginHeaders(nil), e.headers[tabID]...)
}

// headersFor returns the header set declared for the origin of requestURL, or
// nil when this request is not on a declared origin. The nil case is the whole
// point: Network.setExtraHTTPHeaders is a session-wide override that attaches
// its headers to EVERY request the page makes, so an Authorization header set
// that way reaches every analytics beacon, font CDN and tracking pixel the page
// embeds. Scoping has to happen per request, which is why this is consulted from
// the Fetch.requestPaused handler rather than sent to Chrome once.
func (e *environmentState) headersFor(tabID, requestURL string) map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	entries := e.headers[tabID]
	if len(entries) == 0 {
		return nil
	}
	origin, err := CanonicalOrigin(requestURL)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.Origin == origin {
			return entry.Headers
		}
	}
	return nil
}

func (e *environmentState) armCredential(tabID, origin, username, password string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	e.credentials[tabID] = &armedCredential{origin: origin, username: username, password: password}
}

// dropCredential removes the armed credential and overwrites its fields, so a
// heap dump taken after the call cannot recover a password from the freed
// struct. It returns what the credential did while it was armed.
func (e *environmentState) dropCredential(tabID string) (challenged bool, answered int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	armed, ok := e.credentials[tabID]
	if !ok {
		return false, 0
	}
	challenged, answered = armed.challenged, armed.answered
	armed.origin = ""
	armed.username = ""
	armed.password = ""
	delete(e.credentials, tabID)
	return challenged, answered
}

// authArmed reports whether this tab currently needs Fetch.enable to issue
// authRequired events.
func (e *environmentState) authArmed(tabID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	_, ok := e.credentials[tabID]
	return ok
}

// authResponse answers one authRequired challenge. A challenge from an origin
// the caller did not name falls through to Chrome's default handling rather than
// offering the credential to whoever asked.
func (e *environmentState) authResponse(tabID, challengeURL string) *fetch.AuthChallengeResponse {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	armed, ok := e.credentials[tabID]
	if !ok {
		return &fetch.AuthChallengeResponse{Response: fetch.AuthChallengeResponseResponseDefault}
	}
	armed.challenged = true
	origin, err := CanonicalOrigin(challengeURL)
	if err != nil || origin != armed.origin {
		return &fetch.AuthChallengeResponse{Response: fetch.AuthChallengeResponseResponseDefault}
	}
	armed.answered++
	return &fetch.AuthChallengeResponse{
		Response: fetch.AuthChallengeResponseResponseProvideCredentials,
		Username: armed.username,
		Password: armed.password,
	}
}

func (e *environmentState) rememberUABaseline(tabID, userAgent string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	if _, ok := e.uaBaselines[tabID]; !ok {
		e.uaBaselines[tabID] = userAgent
	}
}

func (e *environmentState) takeUABaseline(tabID string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	baseline, ok := e.uaBaselines[tabID]
	delete(e.uaBaselines, tabID)
	return baseline, ok
}

// forget drops every environment override recorded for a closed tab.
func (e *environmentState) forget(tabID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	delete(e.headers, tabID)
	delete(e.uaBaselines, tabID)
	delete(e.authCalls, tabID)
	if armed, ok := e.credentials[tabID]; ok {
		armed.origin = ""
		armed.username = ""
		armed.password = ""
		delete(e.credentials, tabID)
	}
}

// geolocationOverrideParams mirrors Emulation.setGeolocationOverride's payload
// WITHOUT cdproto's omitzero tags. Those tags drop a zero latitude or longitude
// from the wire, and CDP reads an omitted coordinate as "position unavailable" —
// so the generated struct silently turns Greenwich (longitude 0) into a
// geolocation error instead of a position.
type geolocationOverrideParams struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Accuracy  float64 `json:"accuracy"`
}

// SetGeolocation overrides the coordinates navigator.geolocation reports.
func (m *Manager) SetGeolocation(ctx context.Context, opts GeolocationOptions) (EnvironmentResult, error) {
	cfg, clear, err := NormalizeGeolocationOptions(opts)
	if err != nil {
		return EnvironmentResult{}, err
	}
	tabID, tabCtx, cancel, err := m.contextForTab(ctx, strings.TrimSpace(opts.TabID))
	if err != nil {
		return EnvironmentResult{}, err
	}
	defer cancel()

	origin, originErr := m.pageOrigin(tabCtx)
	if clear {
		if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
			return cdpe.ClearGeolocationOverride().Do(runCtx)
		})); err != nil {
			return EnvironmentResult{}, err
		}
		message := "cleared the geolocation override; the page is back on the browser's own location service"
		if originErr == nil {
			// Only what brw changed is changed back. On a persistent profile the
			// site may have been granted geolocation by the human long before this
			// session, and resetting to Prompt regardless would revoke it for them.
			if previous, ours := m.env.takeGeoPermission(origin); ours {
				_ = m.setGeolocationPermission(ctx, origin, permissionSettingFor(previous))
				message += "; the page's geolocation permission is back at " + previous
			} else {
				message += "; the page's geolocation permission is untouched because brw did not grant it"
			}
		}
		return EnvironmentResult{
			OK:      true,
			TabID:   tabID,
			Cleared: true,
			Message: message,
		}, nil
	}

	// Without the grant the override is invisible: a page that has never been
	// allowed geolocation gets PERMISSION_DENIED from getCurrentPosition and
	// never reaches the overridden position at all.
	granted := false
	if originErr == nil {
		previous, readErr := m.geolocationPermissionState(tabCtx)
		if restore, remember := geoPermissionToRestore(previous, readErr); remember {
			m.env.rememberGeoPermission(origin, restore)
		}
		granted = m.setGeolocationPermission(ctx, origin, cdpbrowser.PermissionSettingGranted) == nil
	}
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
		// A conversion, not a rebuild: the JSON tags are the only difference
		// between the two structs, and they are the reason this one exists.
		return cdp.Execute(runCtx, cdpe.CommandSetGeolocationOverride, geolocationOverrideParams(cfg), nil)
	})); err != nil {
		return EnvironmentResult{}, err
	}

	message := "geolocation override applied to this tab; the page's geolocation permission was granted so the override is actually reachable"
	if !granted {
		message = "geolocation override applied to this tab, but the page's geolocation permission could not be granted — a page that has not been allowed geolocation will still see PERMISSION_DENIED"
	}
	return EnvironmentResult{OK: true, TabID: tabID, Geolocation: &cfg, Message: message}, nil
}

// SetNetworkConditions throttles or disconnects the tab's network.
func (m *Manager) SetNetworkConditions(ctx context.Context, opts NetworkConditionsOptions) (EnvironmentResult, error) {
	cfg, clear, err := NormalizeNetworkConditions(opts)
	if err != nil {
		return EnvironmentResult{}, err
	}
	tabID, tabCtx, cancel, err := m.contextForTab(ctx, strings.TrimSpace(opts.TabID))
	if err != nil {
		return EnvironmentResult{}, err
	}
	defer cancel()

	var degraded bool
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
		if err := network.Enable().Do(runCtx); err != nil {
			return err
		}
		var applyErr error
		degraded, applyErr = applyNetworkConditions(runCtx, cfg)
		return applyErr
	})); err != nil {
		return EnvironmentResult{}, err
	}

	message := "network conditions applied to this tab"
	switch {
	case clear:
		message = "restored the tab's normal network: online, no added latency, no throughput cap"
	case cfg.Offline:
		message = "the tab is now offline: navigator.onLine is false and every request fails as if the network were gone. Requests already in flight are not cancelled"
	}
	if degraded {
		message += ". This browser no longer has Network.emulateNetworkConditions, so only navigator.onLine and navigator.connection were overridden — requests still reach the network"
	}
	return EnvironmentResult{OK: true, TabID: tabID, Cleared: clear, Network: &cfg, Message: message}, nil
}

// networkConditionsParams is the payload both the current and the previous
// spelling of this command take. The fields are identical; only the method name
// changed.
type networkConditionsParams struct {
	Offline            bool    `json:"offline"`
	Latency            float64 `json:"latency"`
	DownloadThroughput float64 `json:"downloadThroughput"`
	UploadThroughput   float64 `json:"uploadThroughput"`
}

// applyNetworkConditions sends the throttling command.
//
// Network.emulateNetworkConditions and not Network.overrideNetworkState, which
// is what the protocol definitions brw builds against now carry. They are not
// interchangeable: measured against Chrome, overrideNetworkState sets
// navigator.onLine to false and leaves every request working, while
// emulateNetworkConditions is the one that actually fails them. An offline
// emulation the page can detect but that still serves it data is worse than no
// emulation, because the caller believes they tested the offline path.
//
// The rename means a future Chrome may drop the old name, so the new one is the
// fallback rather than a missing capability. When the fallback is what runs, say
// so: the caller is getting a reported-offline browser that is still online.
//
// The fallback is chosen on the JSON-RPC error CODE, not on Chrome's wording for
// it: matching the message would turn a graceful degrade into a hard failure for
// every caller the day that sentence is reworded.
func applyNetworkConditions(ctx context.Context, cfg NetworkConditionsConfig) (degraded bool, err error) {
	params := networkConditionsParams{
		Offline:            cfg.Offline,
		Latency:            cfg.LatencyMS,
		DownloadThroughput: cfg.DownloadThroughput,
		UploadThroughput:   cfg.UploadThroughput,
	}
	err = cdp.Execute(ctx, "Network.emulateNetworkConditions", params, nil)
	if err == nil || !isMethodNotFound(err) {
		return false, err
	}
	return true, cdp.Execute(ctx, network.CommandOverrideNetworkState, params, nil)
}

// cdpMethodNotFound is JSON-RPC's method-not-found code, which is what Chrome
// answers a command it no longer implements.
const cdpMethodNotFound = -32601

func isMethodNotFound(err error) bool {
	var protocolErr *cdproto.Error
	return errors.As(err, &protocolErr) && protocolErr.Code == cdpMethodNotFound
}

// EmulateMedia forces the CSS media type and user-preference media features.
func (m *Manager) EmulateMedia(ctx context.Context, opts MediaEmulationOptions) (EnvironmentResult, error) {
	cfg, clear, err := NormalizeMediaEmulation(opts)
	if err != nil {
		return EnvironmentResult{}, err
	}
	tabID, tabCtx, cancel, err := m.contextForTab(ctx, strings.TrimSpace(opts.TabID))
	if err != nil {
		return EnvironmentResult{}, err
	}
	defer cancel()

	features := make([]*cdpe.MediaFeature, 0, 2)
	if cfg.ColorScheme != "" {
		features = append(features, &cdpe.MediaFeature{Name: "prefers-color-scheme", Value: cfg.ColorScheme})
	}
	if cfg.ReducedMotion != "" {
		features = append(features, &cdpe.MediaFeature{Name: "prefers-reduced-motion", Value: cfg.ReducedMotion})
	}
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
		// An empty media string plus no features is CDP's documented way to drop
		// the whole override, so clear needs no separate command.
		return cdpe.SetEmulatedMedia().WithMedia(cfg.Media).WithFeatures(features).Do(runCtx)
	})); err != nil {
		return EnvironmentResult{}, err
	}
	m.invalidateState(tabID)

	message := "emulated media applied to this tab; media queries re-evaluate immediately, but a page that reads the preference once at startup needs a reload"
	if clear {
		message = "cleared emulated media; the tab is back on the browser's own media type and user preferences"
	}
	return EnvironmentResult{OK: true, TabID: tabID, Cleared: clear, Media: &cfg, Message: message}, nil
}

// SetLocale overrides the language and time zone the tab reports. Both are
// independent Emulation commands: an empty value restores the host's own for
// that one field, so a request that names only a timezone leaves the locale
// untouched. clear:true restores both.
func (m *Manager) SetLocale(ctx context.Context, opts LocaleOptions) (EnvironmentResult, error) {
	cfg, clear, err := NormalizeLocale(opts)
	if err != nil {
		return EnvironmentResult{}, err
	}
	tabID, tabCtx, cancel, err := m.contextForTab(ctx, strings.TrimSpace(opts.TabID))
	if err != nil {
		return EnvironmentResult{}, err
	}
	defer cancel()

	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
		// Emulation.setLocaleOverride takes an ICU locale ("en_GB"), while the
		// caller and navigator.language use BCP 47 ("en-GB"). Convert on the way
		// in so either spelling works.
		if cfg.Locale != "" || clear {
			if err := cdpe.SetLocaleOverride().WithLocale(icuLocale(cfg.Locale)).Do(runCtx); err != nil {
				return err
			}
		}
		if cfg.Timezone != "" || clear {
			if err := cdpe.SetTimezoneOverride(cfg.Timezone).Do(runCtx); err != nil {
				return fmt.Errorf("set timezone override: %w", err)
			}
		}
		return nil
	})); err != nil {
		return EnvironmentResult{}, err
	}
	m.invalidateState(tabID)

	message := "locale override applied to this tab; Intl formatting, Date and the Accept-Language header use it immediately. navigator.language follows only on the browser builds that derive it from the locale override — where it does not, use brw_set_user_agent with accept_language, which always does"
	if clear {
		message = "cleared the locale and timezone overrides; the tab is back on the host's own locale and zone"
	}
	return EnvironmentResult{OK: true, TabID: tabID, Cleared: clear, Locale: &cfg, Message: message}, nil
}

// icuLocale converts a BCP 47 tag ("en-GB") to the ICU form CDP documents
// ("en_GB"). Chrome accepts the hyphenated form too, but the documented one is
// what a version bump is least likely to change.
func icuLocale(tag string) string {
	return strings.ReplaceAll(tag, "-", "_")
}

// SetExtraHeaders declares extra request headers and the origins allowed to
// receive them.
func (m *Manager) SetExtraHeaders(ctx context.Context, opts ExtraHeadersOptions) (EnvironmentResult, error) {
	entries, clear, err := NormalizeExtraHeaders(opts)
	if err != nil {
		return EnvironmentResult{}, err
	}
	// The bounded context is taken only to resolve and validate the tab; every
	// command below runs on the long-lived one.
	tabID, _, cancel, err := m.contextForTab(ctx, strings.TrimSpace(opts.TabID))
	if err != nil {
		return EnvironmentResult{}, err
	}
	cancel()

	if clear {
		removed := HeaderNames(m.env.listHeaders(tabID))
		m.env.setHeaders(tabID, nil)
		message := "cleared the per-origin extra headers listed here, and turned request interception back off: nothing else on this tab needs it"
		liveCtx, err := m.tabContext(tabID)
		if err == nil {
			if syncErr := m.syncFetchInterception(liveCtx, tabID); syncErr != nil {
				message = "cleared the per-origin extra headers listed here; request interception could not be turned back off, so requests on this tab still pause at the daemon: " + syncErr.Error()
			} else if m.navPolicy.Confines() || m.routes.count(tabID) > 0 {
				message = "cleared the per-origin extra headers listed here; request interception stays armed because routes or the navigation policy still need it"
			}
		}
		return EnvironmentResult{
			OK:           true,
			TabID:        tabID,
			Cleared:      true,
			ExtraHeaders: removed,
			Message:      message,
		}, nil
	}

	// The interception listener must outlive this call, so it is armed on the
	// tab's own long-lived context rather than the deadline-bounded one the
	// commands run under. A listener bound to the bounded context would be
	// dropped the moment this function returned, and the header table would be
	// consulted by nothing.
	liveCtx, err := m.tabContext(tabID)
	if err != nil {
		return EnvironmentResult{}, err
	}
	previous := m.env.listHeaders(tabID)
	m.env.setHeaders(tabID, entries)
	// Headers are attached from the interception handler, so interception has to
	// be armed even when no route and no navigation policy asked for it.
	m.armInterception(tabID, liveCtx)
	if err := m.syncFetchInterception(liveCtx, tabID); err != nil {
		m.env.setHeaders(tabID, previous)
		return EnvironmentResult{}, fmt.Errorf("arm request interception for per-origin headers: %w", err)
	}
	return EnvironmentResult{
		OK:           true,
		TabID:        tabID,
		ExtraHeaders: HeaderNames(entries),
		Message:      "extra headers will be attached only to requests whose origin is declared here; every other request the page makes is left untouched. Header values are not echoed back",
	}, nil
}

// SetUserAgent overrides the user agent, Accept-Language and navigator.platform.
func (m *Manager) SetUserAgent(ctx context.Context, opts UserAgentOptions) (EnvironmentResult, error) {
	cfg, clear, err := NormalizeUserAgent(opts)
	if err != nil {
		return EnvironmentResult{}, err
	}
	tabID, tabCtx, cancel, err := m.contextForTab(ctx, strings.TrimSpace(opts.TabID))
	if err != nil {
		return EnvironmentResult{}, err
	}
	defer cancel()

	if clear {
		baseline, ok := m.env.takeUABaseline(tabID)
		if !ok {
			return EnvironmentResult{}, errors.New("no user-agent override was applied through brw on this tab, and CDP has no command to clear one; reload or reopen the tab to get the browser's own user agent back")
		}
		if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
			return cdpe.SetUserAgentOverride(baseline).Do(runCtx)
		})); err != nil {
			// Put the baseline back: a failed restore that also forgot the
			// baseline would leave the tab unrecoverable.
			m.env.rememberUABaseline(tabID, baseline)
			return EnvironmentResult{}, err
		}
		m.invalidateState(tabID)
		return EnvironmentResult{OK: true, TabID: tabID, Cleared: true, Message: "restored the user agent brw captured before the first override"}, nil
	}

	// CDP has no clearUserAgentOverride, so the only way back is to re-apply the
	// original string — which has to be read before the first override lands.
	var baseline string
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(`navigator.userAgent`, &baseline)); err != nil {
		return EnvironmentResult{}, fmt.Errorf("capture the original user agent before overriding it: %w", err)
	}
	m.env.rememberUABaseline(tabID, baseline)

	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
		override := cdpe.SetUserAgentOverride(cfg.UserAgent)
		if cfg.AcceptLanguage != "" {
			override = override.WithAcceptLanguage(cfg.AcceptLanguage)
		}
		if cfg.Platform != "" {
			override = override.WithPlatform(cfg.Platform)
		}
		return override.Do(runCtx)
	})); err != nil {
		return EnvironmentResult{}, err
	}
	m.invalidateState(tabID)
	return EnvironmentResult{
		OK:        true,
		TabID:     tabID,
		UserAgent: &cfg,
		Message:   "user agent override applied to this tab and to the User-Agent header it sends; Sec-CH-UA client hints still report the real browser unless the site ignores them",
	}, nil
}

// Authenticate loads one URL with HTTP credentials armed for a single origin,
// then drops them.
func (m *Manager) Authenticate(ctx context.Context, opts CredentialsOptions) (EnvironmentResult, error) {
	origin, target, err := NormalizeCredentials(opts)
	if err != nil {
		return EnvironmentResult{}, err
	}
	if target, err = m.prepareNavigationURL(target); err != nil {
		return EnvironmentResult{}, err
	}
	// The bounded context is taken only to resolve and validate the tab; the
	// navigation below brings its own deadline.
	tabID, _, cancel, err := m.contextForTab(ctx, strings.TrimSpace(opts.TabID))
	if err != nil {
		return EnvironmentResult{}, err
	}
	cancel()

	liveCtx, err := m.tabContext(tabID)
	if err != nil {
		return EnvironmentResult{}, err
	}

	// The armed credential is keyed by tab, so two concurrent calls on one tab
	// would overwrite each other's entry and the first to finish would drop the
	// second's while its navigation was still in flight. They queue instead.
	call := m.env.authLock(tabID)
	call.Lock()
	defer call.Unlock()

	// Armed before the listener is installed, so the enable that armInterception
	// schedules already sees a credential and asks for authRequired events.
	m.env.armCredential(tabID, origin, opts.Username, opts.Password)
	// The drop runs whatever happens below, including a panic unwinding through
	// here: the credential must not outlive this call on any path.
	defer func() {
		m.env.dropCredential(tabID)
		// Back to whatever this tab still needs: interception without auth
		// handling when routes, headers or a policy want it, and off when nothing
		// does, so a later 401 falls to Chrome rather than a dead interception.
		_ = m.syncFetchInterception(liveCtx, tabID)
	}()
	m.armInterception(tabID, liveCtx)
	if err := m.syncFetchInterception(liveCtx, tabID); err != nil {
		return EnvironmentResult{}, fmt.Errorf("arm authentication handling: %w", err)
	}

	navResult, navErr := m.NavigateTo(WithTabID(ctx, tabID), target)
	challenged, answered := m.env.dropCredential(tabID)
	if navErr != nil {
		return EnvironmentResult{}, navErr
	}

	outcome := &AuthenticationOutcome{
		Origin:        origin,
		URL:           target,
		Challenged:    challenged,
		Answered:      answered,
		BrowserCached: answered > 0,
	}
	message := fmt.Sprintf("answered %d authentication challenge(s) from %s and dropped brw's copy of the credentials before returning. The BROWSER now holds them: Chrome caches an answered credential for that origin for the rest of the browser session and no CDP command clears it, so every later load of %s in this browser is authenticated. Do this in an incognito context and dispose it when you are done if that is not what you want", answered, origin, origin)
	if !challenged {
		message = fmt.Sprintf("loaded %s without the server ever asking for authentication; the credentials were dropped unused and the browser cached nothing", target)
	}
	return EnvironmentResult{
		OK:            navResult.OK,
		TabID:         tabID,
		Authenticated: outcome,
		Message:       message,
	}, nil
}

// SetDownloadPath redirects completed downloads to a caller-named directory.
func (m *Manager) SetDownloadPath(ctx context.Context, opts DownloadPathOptions) (EnvironmentResult, error) {
	// Both checks are before the arguments: each is a property of the browser,
	// so each has to hold for clear:true as well, which would otherwise adopt a
	// brw staging directory for somebody else's browser context.
	//
	// The remote check is first because it is the narrower one. A provider's
	// browser is also a browser brw did not start, so both fire on it, and the
	// reason a caller needs is that the path names a directory on another
	// machine — not that they should launch their own Chrome, which is advice
	// they cannot act on for a cloud session.
	if err := m.refuseOnRemote("local_downloads"); err != nil {
		return EnvironmentResult{}, err
	}
	// The sibling verb to Downloads: refusing the reader and not the writer
	// would leave setDownloadBehavior retargeting a whole browser context that
	// holds somebody else's windows.
	if !m.stagesDownloads() {
		return EnvironmentResult{}, ErrDownloadRoutingAttachedBrowser
	}
	if !opts.Clear {
		if strings.TrimSpace(opts.Path) == "" {
			return EnvironmentResult{}, errors.New("download path is required, or clear:true to go back to brw's managed staging directory")
		}
		if !filepath.IsAbs(opts.Path) {
			return EnvironmentResult{}, fmt.Errorf("download path %q must be absolute", opts.Path)
		}
	}
	// Arm tracking first: it installs the Browser download listeners brw_downloads
	// reads, and without them a download into the caller's directory would land
	// on disk but never appear in brw_downloads.
	if err := m.ensureDownloadTracking(ctx); err != nil {
		return EnvironmentResult{}, err
	}

	if opts.Clear {
		dir, err := m.restoreManagedDownloadDir()
		if err != nil {
			return EnvironmentResult{}, err
		}
		if err := m.applyDownloadBehavior(ctx, dir); err != nil {
			return EnvironmentResult{}, err
		}
		return EnvironmentResult{
			OK:           true,
			Cleared:      true,
			DownloadPath: dir,
			Message:      "downloads go back to brw's private staging directory, which Manager.Close removes on shutdown",
		}, nil
	}

	dir, err := m.adoptDownloadDir(opts.Path)
	if err != nil {
		return EnvironmentResult{}, err
	}
	if err := m.applyDownloadBehavior(ctx, dir); err != nil {
		return EnvironmentResult{}, err
	}
	return EnvironmentResult{
		OK:           true,
		DownloadPath: dir,
		Message:      "downloads now land in this directory, named by their brw download id rather than the server's suggested filename. That is what keeps the path brw_downloads reports exact: a suggested filename is attacker-controlled and Chrome silently renames collisions. brw will not delete this directory. The setting is browser-wide, so every tab downloads here, and files that completed before this call stay where they were at the paths brw_downloads already reported",
	}, nil
}

// adoptDownloadDir switches tracking to a caller-owned directory.
//
// The managed staging directory it replaces is RETIRED rather than removed:
// downloads that already completed recorded paths inside it, and deleting it
// here would take those files away while brw_downloads went on reporting where
// they used to be. Manager.Close removes every retired directory.
func (m *Manager) adoptDownloadDir(path string) (string, error) {
	dir := filepath.Clean(path)
	if info, err := os.Lstat(dir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("download path must not be a symlink")
		}
		if !info.IsDir() {
			return "", fmt.Errorf("download path %q is not a directory", dir)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// A symlinked PARENT passes the Lstat above, so resolve the whole path and
	// check what the caller's directory actually lands on. The caller's own
	// spelling is what is recorded and echoed back; both name the same directory,
	// and echoing the resolved one would answer a question they did not ask.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("download path %q resolves to %q, which is not a directory", dir, resolved)
	}
	m.retireDownloadStaging()
	m.downloadsMu.Lock()
	m.downloadDir = dir
	// Never owned: brw removes only directories it created, so a caller pointing
	// at their own Downloads folder can never have it deleted on shutdown.
	m.downloadDirOwned = false
	m.downloadsMu.Unlock()
	return dir, nil
}

// restoreManagedDownloadDir goes back to a fresh private staging directory. The
// previous one is retired rather than removed, for the same reason adoption
// retires it: files already reported as living there.
func (m *Manager) restoreManagedDownloadDir() (string, error) {
	m.retireDownloadStaging()
	m.downloadsMu.Lock()
	defer m.downloadsMu.Unlock()
	dir, err := m.resolveDownloadDir()
	if err != nil {
		return "", err
	}
	m.downloadDir = dir
	return dir, nil
}

// syncFetchInterception brings Chrome's request interception on this tab in line
// with what the tab needs right now, and is the only place Fetch.enable or
// Fetch.disable is sent.
//
// handleAuthRequests is a property of the enable call, so arming a credential
// after interception was already enabled has to re-enable it and dropping one has
// to re-enable it without auth. Turning it OFF again matters as much: every
// intercepted request pauses, crosses to the daemon and is continued from a
// goroutine, so a tab left armed after its one brw_authenticate or its cleared
// header table pays that cost on every request for the life of the tab.
//
// The decision and the command are taken under one per-tab lock. Without it the
// late enable armInterception schedules could read "no credential", be overtaken
// by Authenticate's own enable, and land last — leaving interception armed with
// auth handling off while a credential was armed.
func (m *Manager) syncFetchInterception(tabCtx context.Context, tabID string) error {
	lock := m.containment.enableLock(tabID)
	lock.Lock()
	defer lock.Unlock()

	enable, handleAuth, patterns := m.fetchInterceptionCommand(tabID)
	runCtx, cancel := context.WithTimeout(tabCtx, m.timeout)
	defer cancel()
	if !enable {
		return chromedp.Run(runCtx, fetch.Disable())
	}
	return chromedp.Run(runCtx, fetch.Enable().
		WithPatterns(patterns).
		WithHandleAuthRequests(handleAuth))
}

// fetchInterceptionCommand answers what Chrome should be told about this tab:
// whether to intercept at all, whether to deliver authRequired events while it
// does, and WHICH requests to pause. Separate from the command that carries it
// so the decision can be exercised without a browser.
//
// The pattern matters as much as the enable. Every intercepted request pauses,
// crosses to the daemon and is continued from a goroutine, so pausing "*" on a
// tab whose only reason to intercept is the content boundary makes every image,
// font, script and XHR on the page pay for a rule that only ever decides
// documents.
func (m *Manager) fetchInterceptionCommand(tabID string) (enable, handleAuth bool, patterns []*fetch.RequestPattern) {
	handleAuth = m.env.authArmed(tabID)
	everything := handleAuth ||
		m.navPolicy.Confines() ||
		m.routes.count(tabID) > 0 ||
		len(m.env.listHeaders(tabID)) > 0
	// The content boundary decides document requests, which it can only do while
	// Chrome is pausing them.
	inlineDocument := m.containment.inlineDocumentArmed(tabID)
	enable = everything || m.contentNavGuard || inlineDocument
	if !enable {
		return false, false, nil
	}
	switch {
	case everything:
		patterns = []*fetch.RequestPattern{{URLPattern: "*"}}
	case m.contentNavGuard:
		patterns = []*fetch.RequestPattern{{URLPattern: "*", ResourceType: network.ResourceTypeDocument}}
	}
	if inlineDocument {
		patterns = append(patterns, &fetch.RequestPattern{
			URLPattern:   "*",
			ResourceType: network.ResourceTypeDocument,
			RequestStage: fetch.RequestStageResponse,
		})
	}
	return true, handleAuth, patterns
}

// continueWithEnvironmentHeaders answers one paused request, attaching the
// headers declared for its origin.
//
// CDP's continueRequest REPLACES the header set rather than adding to it, so the
// request's own headers have to be carried through or the request goes out
// without its Referer, Accept, Cookie or Origin and the server sees a different
// request from the one the page made.
func (m *Manager) continueWithEnvironmentHeaders(runCtx context.Context, tabID string, paused *fetch.EventRequestPaused) error {
	extra := m.env.headersFor(tabID, paused.Request.URL)
	if len(extra) == 0 {
		return fetch.ContinueRequest(paused.RequestID).Do(runCtx)
	}
	overridden := make(map[string]bool, len(extra))
	for name := range extra {
		overridden[strings.ToLower(name)] = true
	}
	merged := make([]*fetch.HeaderEntry, 0, len(paused.Request.Headers)+len(extra))
	for name, value := range paused.Request.Headers {
		// Header names are case-insensitive, so an exact match would let
		// "authorization" from the page survive alongside our "Authorization" and
		// send the header twice.
		if overridden[strings.ToLower(name)] {
			continue
		}
		merged = append(merged, &fetch.HeaderEntry{Name: name, Value: fmt.Sprint(value)})
	}
	for name, value := range extra {
		merged = append(merged, &fetch.HeaderEntry{Name: name, Value: value})
	}
	return fetch.ContinueRequest(paused.RequestID).WithHeaders(merged).Do(runCtx)
}

// pageOrigin reads the tab's current security origin, which is what a permission
// grant has to name.
func (m *Manager) pageOrigin(tabCtx context.Context) (string, error) {
	var origin string
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(`location.origin`, &origin)); err != nil {
		return "", err
	}
	origin = strings.TrimSpace(origin)
	if origin == "" || origin == "null" {
		return "", fmt.Errorf("tab has no security origin to grant permissions to")
	}
	return origin, nil
}

// Permission states as the Permissions API spells them.
const (
	permissionStateGranted = "granted"
	permissionStateDenied  = "denied"
	permissionStatePrompt  = "prompt"
)

// geoPermissionToRestore decides what clear has to put back for an origin whose
// geolocation brw is about to grant, given the state the page reported first.
//
// An unreadable state resolves to prompt rather than to "record nothing".
// navigator.permissions.query runs in the page and fails on one that is
// navigating or already torn down, and recording nothing makes the clear path
// take its "brw did not grant this" branch, which leaves brw's own grant
// standing for the life of a persistent profile. Prompt is where a page starts,
// so the cost of guessing wrong is that a human's earlier grant gets asked for
// again; the cost of the other guess is a permanent grant nobody asked for.
//
// An already-granted state is recorded as nothing at all: that grant is not
// brw's, so clear must not take it away.
func geoPermissionToRestore(previous string, readErr error) (state string, remember bool) {
	if readErr != nil {
		return permissionStatePrompt, true
	}
	if previous == permissionStateGranted {
		return "", false
	}
	return previous, true
}

// permissionSettingFor maps a Permissions API state onto the CDP setting that
// reproduces it. Anything unrecognized falls to prompt, which is the state a
// page starts in and the safe one to leave behind.
func permissionSettingFor(state string) cdpbrowser.PermissionSetting {
	switch state {
	case permissionStateGranted:
		return cdpbrowser.PermissionSettingGranted
	case permissionStateDenied:
		return cdpbrowser.PermissionSettingDenied
	default:
		return cdpbrowser.PermissionSettingPrompt
	}
}

// geolocationPermissionState reads the page's current geolocation permission.
// Browser.getPermission does not exist, so the page's own Permissions API is the
// only way to learn what the state was before brw overwrote it.
func (m *Manager) geolocationPermissionState(tabCtx context.Context) (string, error) {
	var state string
	err := chromedp.Run(tabCtx, chromedp.Evaluate(
		`navigator.permissions.query({name:"geolocation"}).then(status => status.state)`,
		&state,
		func(params *runtime.EvaluateParams) *runtime.EvaluateParams {
			return params.WithAwaitPromise(true)
		},
	))
	if err != nil {
		return "", err
	}
	return state, nil
}

func (m *Manager) setGeolocationPermission(ctx context.Context, origin string, setting cdpbrowser.PermissionSetting) error {
	return m.runBrowser(ctx, func(runCtx context.Context) error {
		return cdpbrowser.SetPermission(&cdpbrowser.PermissionDescriptor{Name: "geolocation"}, setting).
			WithOrigin(origin).
			Do(runCtx)
	})
}
