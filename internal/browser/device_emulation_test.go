package browser

import "testing"

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
