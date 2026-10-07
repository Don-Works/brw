package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

var _ EnvironmentController = (*Manager)(nil)

type environmentState struct {
	mu          sync.Mutex
	headers     map[string][]OriginHeaders
	credentials map[string]*armedCredential
	uaBaselines map[string]string
	authCalls   map[string]*sync.Mutex

	geoPermissions map[string]string

	initScripts map[string][]InitScript
}

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
			e.initScripts[tabID] = slices.Delete(scripts, i, i+1)
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

func (e *environmentState) rememberGeoPermission(origin, state string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	if _, ok := e.geoPermissions[origin]; !ok {
		e.geoPermissions[origin] = state
	}
}

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

func (e *environmentState) authArmed(tabID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	_, ok := e.credentials[tabID]
	return ok
}

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

func (e *environmentState) forget(tabID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLocked()
	delete(e.headers, tabID)
	delete(e.uaBaselines, tabID)
	delete(e.authCalls, tabID)
	delete(e.initScripts, tabID)
	if armed, ok := e.credentials[tabID]; ok {
		armed.origin = ""
		armed.username = ""
		armed.password = ""
		delete(e.credentials, tabID)
	}
}

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

	granted := false
	if originErr == nil {
		previous, readErr := m.geolocationPermissionState(tabCtx)
		if restore, remember := geoPermissionToRestore(previous, readErr); remember {
			m.env.rememberGeoPermission(origin, restore)
		}
		granted = m.setGeolocationPermission(ctx, origin, cdpbrowser.PermissionSettingGranted) == nil
	}
	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(runCtx context.Context) error {

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

type networkConditionsParams struct {
	Offline            bool    `json:"offline"`
	Latency            float64 `json:"latency"`
	DownloadThroughput float64 `json:"downloadThroughput"`
	UploadThroughput   float64 `json:"uploadThroughput"`
}

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

// SetLocale overrides the language and time zone the tab reports.
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

func icuLocale(tag string) string {
	return strings.ReplaceAll(tag, "-", "_")
}

// SetExtraHeaders declares extra request headers and the origins allowed to receive them.
func (m *Manager) SetExtraHeaders(ctx context.Context, opts ExtraHeadersOptions) (EnvironmentResult, error) {
	entries, clear, err := NormalizeExtraHeaders(opts)
	if err != nil {
		return EnvironmentResult{}, err
	}

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

	liveCtx, err := m.tabContext(tabID)
	if err != nil {
		return EnvironmentResult{}, err
	}
	previous := m.env.listHeaders(tabID)
	m.env.setHeaders(tabID, entries)

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

			m.env.rememberUABaseline(tabID, baseline)
			return EnvironmentResult{}, err
		}
		m.invalidateState(tabID)
		return EnvironmentResult{OK: true, TabID: tabID, Cleared: true, Message: "restored the user agent brw captured before the first override"}, nil
	}

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

// Authenticate loads one URL with HTTP credentials armed for a single origin, then drops them.
func (m *Manager) Authenticate(ctx context.Context, opts CredentialsOptions) (EnvironmentResult, error) {
	origin, target, err := NormalizeCredentials(opts)
	if err != nil {
		return EnvironmentResult{}, err
	}
	if target, err = m.prepareNavigationURL(target); err != nil {
		return EnvironmentResult{}, err
	}

	tabID, _, cancel, err := m.contextForTab(ctx, strings.TrimSpace(opts.TabID))
	if err != nil {
		return EnvironmentResult{}, err
	}
	cancel()

	liveCtx, err := m.tabContext(tabID)
	if err != nil {
		return EnvironmentResult{}, err
	}

	call := m.env.authLock(tabID)
	call.Lock()
	defer call.Unlock()

	m.env.armCredential(tabID, origin, opts.Username, opts.Password)

	defer func() {
		m.env.dropCredential(tabID)

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

	if err := m.refuseOnRemote("local_downloads"); err != nil {
		return EnvironmentResult{}, err
	}

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

	m.downloadDirOwned = false
	m.downloadsMu.Unlock()
	return dir, nil
}

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

func (m *Manager) fetchInterceptionCommand(tabID string) (enable, handleAuth bool, patterns []*fetch.RequestPattern) {
	handleAuth = m.env.authArmed(tabID)
	everything := handleAuth ||
		m.navPolicy.Confines() ||
		m.routes.count(tabID) > 0 ||
		len(m.env.listHeaders(tabID)) > 0

	inlineDocument := m.containment.inlineDocumentPatterns(tabID)
	enable = everything || m.contentNavGuard || len(inlineDocument) > 0
	if !enable {
		return false, false, nil
	}
	switch {
	case everything:
		patterns = []*fetch.RequestPattern{{URLPattern: "*"}}
	case m.contentNavGuard:
		patterns = []*fetch.RequestPattern{{URLPattern: "*", ResourceType: network.ResourceTypeDocument}}
	}
	for _, pattern := range inlineDocument {
		patterns = append(patterns, &fetch.RequestPattern{
			URLPattern:   pattern,
			ResourceType: network.ResourceTypeDocument,
			RequestStage: fetch.RequestStageResponse,
		})
	}
	return true, handleAuth, patterns
}

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

const (
	permissionStateGranted = "granted"
	permissionStateDenied  = "denied"
	permissionStatePrompt  = "prompt"
)

func geoPermissionToRestore(previous string, readErr error) (state string, remember bool) {
	if readErr != nil {
		return permissionStatePrompt, true
	}
	if previous == permissionStateGranted {
		return "", false
	}
	return previous, true
}

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
