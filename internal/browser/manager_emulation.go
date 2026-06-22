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

	m.emulationMu.Lock()
	m.emulationStates[tabID] = deviceEmulationState{
		Baseline:    identity,
		HasBaseline: hasIdentity,
		Config:      cfg,
	}
	m.emulationMu.Unlock()
	m.invalidateState(tabID)

	return DeviceEmulationResult{
		OK:        true,
		Emulation: &cfg,
		Message:   "applied DevTools device emulation to the active tab; reload if the app only detects device class during initial page load",
	}, nil
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
