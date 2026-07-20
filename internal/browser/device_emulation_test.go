package browser

import (
	"strings"
	"testing"
)

func TestNormalizeDeviceEmulationPresetLandscape(t *testing.T) {
	cfg, clear, err := NormalizeDeviceEmulationOptions(DeviceEmulationOptions{
		Device:      "iPhone SE",
		Orientation: "landscape",
	})
	if err != nil {
		t.Fatal(err)
	}
	if clear {
		t.Fatal("clear = true, want false")
	}
	if cfg.Device != "iphone_se" || cfg.Width != 667 || cfg.Height != 375 {
		t.Fatalf("preset dimensions = %+v", cfg)
	}
	if !cfg.Mobile || !cfg.Touch || cfg.DeviceScaleFactor != 2 || cfg.MaxTouchPoints != 5 {
		t.Fatalf("mobile/touch/DPR = %+v", cfg)
	}
	if cfg.UserAgent == "" || cfg.Platform != "iPhone" {
		t.Fatalf("user agent/platform = %+v", cfg)
	}
}

func TestNormalizeDeviceEmulationCustomDefaultsMobile(t *testing.T) {
	cfg, clear, err := NormalizeDeviceEmulationOptions(DeviceEmulationOptions{
		Width:  412,
		Height: 915,
	})
	if err != nil {
		t.Fatal(err)
	}
	if clear {
		t.Fatal("clear = true, want false")
	}
	if cfg.Device != "custom" || !cfg.Mobile || !cfg.Touch || cfg.DeviceScaleFactor != 2 {
		t.Fatalf("custom config = %+v", cfg)
	}
	if cfg.Orientation != "portrait" {
		t.Fatalf("orientation = %q, want portrait", cfg.Orientation)
	}
}

func TestNormalizeDeviceEmulationClearAlias(t *testing.T) {
	_, clear, err := NormalizeDeviceEmulationOptions(DeviceEmulationOptions{Device: "desktop"})
	if err != nil {
		t.Fatal(err)
	}
	if !clear {
		t.Fatal("desktop alias should clear emulation")
	}
}

// TestUnknownPresetErrorListsEveryPreset guards the drift that made
// brw_emulate_device fail 31.6% of its calls in the usage ledger: the error
// message hard-coded 7 of the 10 registered presets, so an agent that guessed
// a device name was handed an incomplete recovery list and guessed again.
func TestUnknownPresetErrorListsEveryPreset(t *testing.T) {
	_, _, err := NormalizeDeviceEmulationOptions(DeviceEmulationOptions{Device: "iPhone 15"})
	if err == nil {
		t.Fatal("expected an error for an unknown preset")
	}
	for _, name := range SupportedDevicePresets() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("unknown-preset error omits registered preset %q: %s", name, err)
		}
	}
	if len(SupportedDevicePresets()) != len(deviceEmulationPresets) {
		t.Errorf("SupportedDevicePresets returned %d names for %d presets",
			len(SupportedDevicePresets()), len(deviceEmulationPresets))
	}
}

// TestEveryPresetResolves keeps the advertised list honest: every name the
// error message and tool schema offer must actually normalize.
func TestEveryPresetResolves(t *testing.T) {
	for _, name := range SupportedDevicePresets() {
		cfg, clear, err := NormalizeDeviceEmulationOptions(DeviceEmulationOptions{Device: name})
		if err != nil {
			t.Errorf("advertised preset %q does not resolve: %v", name, err)
			continue
		}
		if clear {
			t.Errorf("preset %q unexpectedly resolved to a clear request", name)
		}
		if cfg.Width <= 0 || cfg.Height <= 0 {
			t.Errorf("preset %q resolved to empty metrics: %+v", name, cfg)
		}
	}
}

// TestAndroidPresetsReportTheirOwnModel guards a bug found while verifying the
// preset list live: pixel_5 and galaxy_s20 shared one user-agent constant and
// both announced themselves as a "Pixel 7", so any device sniffing the caller
// was trying to exercise saw the wrong hardware. iOS is deliberately exempt —
// real iPhone/iPad UAs carry no model string.
func TestAndroidPresetsReportTheirOwnModel(t *testing.T) {
	models := map[string]string{
		"pixel_5":    "Pixel 5",
		"pixel_7":    "Pixel 7",
		"galaxy_s20": "SM-G981B",
	}
	seen := map[string]string{}
	for device, model := range models {
		cfg, _, err := NormalizeDeviceEmulationOptions(DeviceEmulationOptions{Device: device})
		if err != nil {
			t.Fatalf("%s: %v", device, err)
		}
		if !strings.Contains(cfg.UserAgent, model) {
			t.Errorf("%s user agent does not report %q: %s", device, model, cfg.UserAgent)
		}
		if prev, dup := seen[cfg.UserAgent]; dup {
			t.Errorf("%s and %s share a user agent: %s", device, prev, cfg.UserAgent)
		}
		seen[cfg.UserAgent] = device
	}
}
