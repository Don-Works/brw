package browser

import (
	"context"
	"fmt"

	cdpe "github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

type deviceEmulationIdentity struct {
	UserAgent string `json:"userAgent"`
	Platform  string `json:"platform"`
}

type deviceEmulationState struct {
	Baseline    deviceEmulationIdentity
	HasBaseline bool
	Config      DeviceEmulationConfig
}

func (m *Manager) EmulateDevice(ctx context.Context, opts DeviceEmulationOptions) (DeviceEmulationResult, error) {
	cfg, clear, err := NormalizeDeviceEmulationOptions(opts)
	if err != nil {
		return DeviceEmulationResult{}, err
	}

	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return DeviceEmulationResult{}, err
	}
	defer cancel()

	if clear {
		return m.clearDeviceEmulation(tabID, tabCtx)
	}

	identity, hasIdentity, err := m.deviceEmulationBaseline(tabID, tabCtx, cfg.UserAgent != "" || cfg.Platform != "")
	if err != nil {
		return DeviceEmulationResult{}, err
	}

	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return applyDeviceEmulation(ctx, cfg)
	})); err != nil {
		return DeviceEmulationResult{}, err
	}

	layoutWidth, fellBack := m.ensureLayoutViewport(tabCtx, cfg)

	m.emulationMu.Lock()
	m.emulationStates[tabID] = deviceEmulationState{
		Baseline:    identity,
		HasBaseline: hasIdentity,
		Config:      cfg,
	}
	m.emulationMu.Unlock()
	m.invalidateState(tabID)

	message := "applied DevTools device emulation to the active tab; reload if the app only detects device class during initial page load"
	if fellBack {
		message = fmt.Sprintf(
			"applied DevTools device emulation to the active tab, with the mobile flag dropped so the page lays out at %dpx. "+
				"Chrome ignores a page's viewport meta tag under mobile emulation and would otherwise lay it out at a fixed 980px, "+
				"leaving every width-based media query evaluating against 980. Screen size, pixel ratio, user agent and touch points "+
				"are still emulated. Reload if the app only detects device class during initial page load", cfg.Width)
	}
	return DeviceEmulationResult{
		OK:                   true,
		Emulation:            &cfg,
		LayoutViewportWidth:  layoutWidth,
		MobileLayoutFallback: fellBack,
		Message:              message,
	}, nil
}

// ensureLayoutViewport makes the page actually lay out at the emulated width,
// and reports the width it ended up with.
//
// Chrome does not process a page's viewport meta tag when the device metrics
// override carries mobile:true - off Android it lays the page out at a fixed
// 980px desktop fallback instead of the emulated width. screen.*,
// devicePixelRatio, the user agent and touch points all emulate correctly, so
// the override LOOKS applied while every width-based media query still
// evaluates against 980, which silently defeats the one thing device emulation
// is for.
//
// This is Chrome's behaviour rather than ours: a bare
// Emulation.setDeviceMetricsOverride(w, h, dsf, mobile=true) reproduces it, the
// --enable-viewport and --enable-viewport-meta switches do not change it, and
// the identical call with mobile:false lays out at the requested width.
//
// So measure what the page got, and when the mobile flag cost us the layout
// viewport, re-apply without it. Everything that made the context mobile -
// screen size, DPR, user agent, touch points - is applied by separate CDP calls
// and survives; only the renderer's internal mobile flag is given up, which is
// worth a correct layout viewport to a caller testing responsive behaviour. The
// caller is told both the measured width and whether the fallback was used, so
// nothing about this is silent.
func (m *Manager) ensureLayoutViewport(tabCtx context.Context, cfg DeviceEmulationConfig) (width int64, fellBack bool) {
	measured, err := measureLayoutViewportWidth(tabCtx)
	if err != nil {
		return 0, false
	}
	if !cfg.Mobile || measured <= 0 || layoutWidthMatches(measured, cfg.Width) {
		return measured, false
	}

	desktopLayout := cfg
	desktopLayout.Mobile = false
	if applyErr := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return applyDeviceEmulation(ctx, desktopLayout)
	})); applyErr != nil {
		return measured, false
	}
	retried, retryErr := measureLayoutViewportWidth(tabCtx)
	if retryErr == nil && layoutWidthMatches(retried, cfg.Width) {
		return retried, true
	}
	// The fallback did not help, so restore exactly what the caller asked for
	// rather than leaving the tab in a third state nobody chose.
	_ = chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return applyDeviceEmulation(ctx, cfg)
	}))
	return measured, false
}

// measureLayoutViewportWidth reports the width the page lays out at, which is
// what width-based media queries evaluate against.
//
// window.innerWidth rather than documentElement.clientWidth: clientWidth
// EXCLUDES a classic scrollbar, so a tall page under a correct 375px override
// measures 360 and an exact comparison would wrongly conclude the override had
// not taken. innerWidth tracks the emulated width whether or not the page
// scrolls.
func measureLayoutViewportWidth(tabCtx context.Context) (int64, error) {
	var width int64
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(`window.innerWidth`, &width)); err != nil {
		return 0, err
	}
	return width, nil
}

// layoutWidthMatches allows a pixel or two of slack. Chrome reports innerWidth
// one pixel over the layout width in some emulation modes, and the question
// being asked is "did the override take", not "is this exact to the pixel" —
// the failure it has to catch is a 980px desktop fallback against a 375px
// request, which no rounding tolerance could hide.
func layoutWidthMatches(measured, requested int64) bool {
	delta := measured - requested
	if delta < 0 {
		delta = -delta
	}
	return delta <= 2
}

func (m *Manager) clearDeviceEmulation(tabID string, tabCtx context.Context) (DeviceEmulationResult, error) {
	m.emulationMu.Lock()
	state, hadState := m.emulationStates[tabID]
	m.emulationMu.Unlock()

	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		if err := cdpe.ClearDeviceMetricsOverride().Do(ctx); err != nil {
			return err
		}
		if err := cdpe.SetTouchEmulationEnabled(false).Do(ctx); err != nil {
			return err
		}
		if err := cdpe.SetEmitTouchEventsForMouse(false).
			WithConfiguration(cdpe.SetEmitTouchEventsForMouseConfigurationDesktop).Do(ctx); err != nil {
			return err
		}
		if state.HasBaseline && state.Baseline.UserAgent != "" {
			ua := cdpe.SetUserAgentOverride(state.Baseline.UserAgent)
			if state.Baseline.Platform != "" {
				ua = ua.WithPlatform(state.Baseline.Platform)
			}
			if err := ua.Do(ctx); err != nil {
				return err
			}
		}
		return nil
	})); err != nil {
		return DeviceEmulationResult{}, err
	}

	m.emulationMu.Lock()
	delete(m.emulationStates, tabID)
	m.emulationMu.Unlock()
	m.invalidateState(tabID)

	result := DeviceEmulationResult{
		OK:      true,
		Cleared: true,
		Message: "cleared DevTools device metrics and touch emulation for the active tab",
	}
	if !hadState || !state.HasBaseline {
		result.Message += "; no stored user-agent baseline was available to restore"
	}
	return result, nil
}

func (m *Manager) deviceEmulationBaseline(tabID string, tabCtx context.Context, required bool) (deviceEmulationIdentity, bool, error) {
	m.emulationMu.Lock()
	state, ok := m.emulationStates[tabID]
	m.emulationMu.Unlock()
	if ok && state.HasBaseline {
		return state.Baseline, true, nil
	}
	if !required {
		return deviceEmulationIdentity{}, false, nil
	}

	var identity deviceEmulationIdentity
	expr := `({userAgent: navigator.userAgent || "", platform: navigator.platform || ""})`
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(expr, &identity)); err != nil {
		return deviceEmulationIdentity{}, false, fmt.Errorf("capture original user agent before emulation: %w", err)
	}
	return identity, true, nil
}

func applyDeviceEmulation(ctx context.Context, cfg DeviceEmulationConfig) error {
	metrics := cdpe.SetDeviceMetricsOverride(cfg.Width, cfg.Height, cfg.DeviceScaleFactor, cfg.Mobile).
		WithScreenWidth(cfg.Width).
		WithScreenHeight(cfg.Height).
		WithScreenOrientation(deviceScreenOrientation(cfg.Orientation)).
		WithScreenOrientationLockEmulation(true)
	if err := metrics.Do(ctx); err != nil {
		return err
	}

	touch := cdpe.SetTouchEmulationEnabled(cfg.Touch)
	if cfg.Touch && cfg.MaxTouchPoints > 0 {
		touch = touch.WithMaxTouchPoints(cfg.MaxTouchPoints)
	}
	if err := touch.Do(ctx); err != nil {
		return err
	}

	touchConfig := cdpe.SetEmitTouchEventsForMouseConfigurationDesktop
	if cfg.Touch {
		touchConfig = cdpe.SetEmitTouchEventsForMouseConfigurationMobile
	}
	if err := cdpe.SetEmitTouchEventsForMouse(cfg.Touch).WithConfiguration(touchConfig).Do(ctx); err != nil {
		return err
	}

	if cfg.UserAgent != "" {
		ua := cdpe.SetUserAgentOverride(cfg.UserAgent)
		if cfg.Platform != "" {
			ua = ua.WithPlatform(cfg.Platform)
		}
		if err := ua.Do(ctx); err != nil {
			return err
		}
	}
	return nil
}

func deviceScreenOrientation(orientation string) *cdpe.ScreenOrientation {
	if orientation == "landscape" {
		return &cdpe.ScreenOrientation{Type: cdpe.OrientationTypeLandscapePrimary, Angle: 90}
	}
	return &cdpe.ScreenOrientation{Type: cdpe.OrientationTypePortraitPrimary, Angle: 0}
}
