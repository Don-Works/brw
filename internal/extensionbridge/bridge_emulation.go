package extensionbridge

import (
	"context"
	"fmt"

	"github.com/Don-Works/brw/internal/browser"
)

func (b *Bridge) EmulateDevice(ctx context.Context, opts browser.DeviceEmulationOptions) (browser.DeviceEmulationResult, error) {
	cfg, clear, err := browser.NormalizeDeviceEmulationOptions(opts)
	if err != nil {
		return browser.DeviceEmulationResult{}, err
	}

	tabID := b.contextTabID(ctx)
	if tabID == "" {
		return browser.DeviceEmulationResult{}, fmt.Errorf("no active tab available for device emulation")
	}

	if clear {
		return b.clearDeviceEmulation(ctx, tabID)
	}

	identity, hasIdentity, err := b.deviceEmulationBaseline(ctx, tabID, cfg.UserAgent != "" || cfg.Platform != "")
	if err != nil {
		return browser.DeviceEmulationResult{}, err
	}

	if _, err := b.cdp(ctx, tabID, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width":                          cfg.Width,
		"height":                         cfg.Height,
		"deviceScaleFactor":              cfg.DeviceScaleFactor,
		"mobile":                         cfg.Mobile,
		"screenWidth":                    cfg.Width,
		"screenHeight":                   cfg.Height,
		"screenOrientation":              screenOrientationPayload(cfg.Orientation),
		"screenOrientationLockEmulation": true,
	}); err != nil {
		return browser.DeviceEmulationResult{}, err
	}
	if _, err := b.cdp(ctx, tabID, "Emulation.setTouchEmulationEnabled", map[string]any{
		"enabled":        cfg.Touch,
		"maxTouchPoints": cfg.MaxTouchPoints,
	}); err != nil {
		return browser.DeviceEmulationResult{}, err
	}
	touchConfig := "desktop"
	if cfg.Touch {
		touchConfig = "mobile"
	}
	if _, err := b.cdp(ctx, tabID, "Emulation.setEmitTouchEventsForMouse", map[string]any{
		"enabled":       cfg.Touch,
		"configuration": touchConfig,
	}); err != nil {
		return browser.DeviceEmulationResult{}, err
	}
	if cfg.UserAgent != "" {
		params := map[string]any{"userAgent": cfg.UserAgent}
		if cfg.Platform != "" {
			params["platform"] = cfg.Platform
		}
		if _, err := b.cdp(ctx, tabID, "Emulation.setUserAgentOverride", params); err != nil {
			return browser.DeviceEmulationResult{}, err
		}
	}

	b.emulationMu.Lock()
	b.emulationStates[tabID] = bridgeDeviceEmulationState{
		Baseline:    identity,
		HasBaseline: hasIdentity,
		Config:      cfg,
	}
	b.emulationMu.Unlock()
	b.clearSnapshotCache(ctx, tabID)

	return browser.DeviceEmulationResult{
		OK:        true,
		Emulation: &cfg,
		Message:   "applied DevTools device emulation to the active tab; reload if the app only detects device class during initial page load",
	}, nil
}

func (b *Bridge) clearDeviceEmulation(ctx context.Context, tabID string) (browser.DeviceEmulationResult, error) {
	b.emulationMu.Lock()
	state, hadState := b.emulationStates[tabID]
	b.emulationMu.Unlock()

	if _, err := b.cdp(ctx, tabID, "Emulation.clearDeviceMetricsOverride", nil); err != nil {
		return browser.DeviceEmulationResult{}, err
	}
	if _, err := b.cdp(ctx, tabID, "Emulation.setTouchEmulationEnabled", map[string]any{"enabled": false}); err != nil {
		return browser.DeviceEmulationResult{}, err
	}
	if _, err := b.cdp(ctx, tabID, "Emulation.setEmitTouchEventsForMouse", map[string]any{
		"enabled":       false,
		"configuration": "desktop",
	}); err != nil {
		return browser.DeviceEmulationResult{}, err
	}
	if state.HasBaseline && state.Baseline.UserAgent != "" {
		params := map[string]any{"userAgent": state.Baseline.UserAgent}
		if state.Baseline.Platform != "" {
			params["platform"] = state.Baseline.Platform
		}
		if _, err := b.cdp(ctx, tabID, "Emulation.setUserAgentOverride", params); err != nil {
			return browser.DeviceEmulationResult{}, err
		}
	}

	b.emulationMu.Lock()
	delete(b.emulationStates, tabID)
	b.emulationMu.Unlock()
	b.clearSnapshotCache(ctx, tabID)

	result := browser.DeviceEmulationResult{
		OK:      true,
		Cleared: true,
		Message: "cleared DevTools device metrics and touch emulation for the active tab",
	}
	if !hadState || !state.HasBaseline {
		result.Message += "; no stored user-agent baseline was available to restore"
	}
	return result, nil
}

func (b *Bridge) deviceEmulationBaseline(ctx context.Context, tabID string, required bool) (bridgeDeviceIdentity, bool, error) {
	b.emulationMu.Lock()
	state, ok := b.emulationStates[tabID]
	b.emulationMu.Unlock()
	if ok && state.HasBaseline {
		return state.Baseline, true, nil
	}
	if !required {
		return bridgeDeviceIdentity{}, false, nil
	}

	var identity bridgeDeviceIdentity
	expr := `({userAgent: navigator.userAgent || "", platform: navigator.platform || ""})`
	if err := b.evaluate(ctx, expr, tabID, &identity); err != nil {
		return bridgeDeviceIdentity{}, false, fmt.Errorf("capture original user agent before emulation: %w", err)
	}
	return identity, true, nil
}

func (b *Bridge) clearSnapshotCache(ctx context.Context, tabID string) {
	if tabID == "" {
		return
	}
	_, _ = b.call(ctx, "clear_snapshot_cache", map[string]any{"tabId": parseTabID(tabID)})
}

func screenOrientationPayload(orientation string) map[string]any {
	if orientation == "landscape" {
		return map[string]any{"type": "landscapePrimary", "angle": 90}
	}
	return map[string]any{"type": "portraitPrimary", "angle": 0}
}
