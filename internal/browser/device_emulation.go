package browser

import (
	"fmt"
	"strings"
	"unicode"
)

// DeviceEmulationOptions describes a DevTools Protocol device emulation target.
// Width/height are CSS viewport pixels, not OS window bounds. Mobile=true uses
// Chrome's real mobile emulation mode, including viewport meta tag handling,
// overlay scrollbars, and text autosizing.
type DeviceEmulationOptions struct {
	Clear             bool    `json:"clear,omitempty"`
	Device            string  `json:"device,omitempty"`
	Width             int64   `json:"width,omitempty"`
	Height            int64   `json:"height,omitempty"`
	DeviceScaleFactor float64 `json:"device_scale_factor,omitempty"`
	Mobile            *bool   `json:"mobile,omitempty"`
	Touch             *bool   `json:"touch,omitempty"`
	UserAgent         string  `json:"user_agent,omitempty"`
	Platform          string  `json:"platform,omitempty"`
	MaxTouchPoints    int64   `json:"max_touch_points,omitempty"`
	Orientation       string  `json:"orientation,omitempty"`
}

// DeviceEmulationConfig is the fully resolved target that transports apply via
// CDP Emulation.* commands.
type DeviceEmulationConfig struct {
	Device            string  `json:"device"`
	Width             int64   `json:"width"`
	Height            int64   `json:"height"`
	DeviceScaleFactor float64 `json:"device_scale_factor"`
	Mobile            bool    `json:"mobile"`
	Touch             bool    `json:"touch"`
	UserAgent         string  `json:"user_agent,omitempty"`
	Platform          string  `json:"platform,omitempty"`
	MaxTouchPoints    int64   `json:"max_touch_points,omitempty"`
	Orientation       string  `json:"orientation"`
}

type DeviceEmulationResult struct {
	OK        bool                   `json:"ok"`
	Cleared   bool                   `json:"cleared,omitempty"`
	Emulation *DeviceEmulationConfig `json:"emulation,omitempty"`
	Message   string                 `json:"message,omitempty"`
}

type devicePreset struct {
	Device            string
	Width             int64
	Height            int64
	DeviceScaleFactor float64
	UserAgent         string
	Platform          string
	MaxTouchPoints    int64
}

const (
	iphoneMobileUA  = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
	ipadMobileUA    = "Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
	androidChromeUA = "Mozilla/5.0 (Linux; Android 14; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Mobile Safari/537.36"
)

var deviceEmulationPresets = map[string]devicePreset{
	"iphonese": {
		Device: "iphone_se", Width: 375, Height: 667, DeviceScaleFactor: 2,
		UserAgent: iphoneMobileUA, Platform: "iPhone", MaxTouchPoints: 5,
	},
	"iphone12": {
		Device: "iphone_12", Width: 390, Height: 844, DeviceScaleFactor: 3,
		UserAgent: iphoneMobileUA, Platform: "iPhone", MaxTouchPoints: 5,
	},
	"iphone13": {
		Device: "iphone_13", Width: 390, Height: 844, DeviceScaleFactor: 3,
		UserAgent: iphoneMobileUA, Platform: "iPhone", MaxTouchPoints: 5,
	},
	"iphone14": {
		Device: "iphone_14", Width: 390, Height: 844, DeviceScaleFactor: 3,
		UserAgent: iphoneMobileUA, Platform: "iPhone", MaxTouchPoints: 5,
	},
	"iphone14promax": {
		Device: "iphone_14_pro_max", Width: 430, Height: 932, DeviceScaleFactor: 3,
		UserAgent: iphoneMobileUA, Platform: "iPhone", MaxTouchPoints: 5,
	},
	"pixel5": {
		Device: "pixel_5", Width: 393, Height: 851, DeviceScaleFactor: 2.75,
		UserAgent: androidChromeUA, Platform: "Linux armv8l", MaxTouchPoints: 5,
	},
	"pixel7": {
		Device: "pixel_7", Width: 412, Height: 915, DeviceScaleFactor: 2.625,
		UserAgent: androidChromeUA, Platform: "Linux armv8l", MaxTouchPoints: 5,
	},
	"galaxys20": {
		Device: "galaxy_s20", Width: 360, Height: 800, DeviceScaleFactor: 3,
		UserAgent: androidChromeUA, Platform: "Linux armv8l", MaxTouchPoints: 5,
	},
	"ipadmini": {
		Device: "ipad_mini", Width: 768, Height: 1024, DeviceScaleFactor: 2,
		UserAgent: ipadMobileUA, Platform: "iPad", MaxTouchPoints: 5,
	},
	"ipad": {
		Device: "ipad", Width: 820, Height: 1180, DeviceScaleFactor: 2,
		UserAgent: ipadMobileUA, Platform: "iPad", MaxTouchPoints: 5,
	},
}

// NormalizeDeviceEmulationOptions resolves a caller request into a CDP-ready
// config. The returned clear flag is true when the caller requested reset/off.
func NormalizeDeviceEmulationOptions(opts DeviceEmulationOptions) (DeviceEmulationConfig, bool, error) {
	deviceKey := normalizedDeviceKey(opts.Device)
	if opts.Clear || deviceKey == "clear" || deviceKey == "reset" || deviceKey == "off" || deviceKey == "none" || deviceKey == "desktop" {
		return DeviceEmulationConfig{}, true, nil
	}

	var cfg DeviceEmulationConfig
	if deviceKey == "" && opts.Width == 0 && opts.Height == 0 {
		deviceKey = "iphonese"
	}

	switch {
	case deviceKey == "custom" || deviceKey == "responsive":
		cfg.Device = deviceKey
	case deviceKey != "":
		preset, ok := deviceEmulationPresets[deviceKey]
		if !ok {
			return DeviceEmulationConfig{}, false, fmt.Errorf("unknown device preset %q; use iphone_se, iphone_14, iphone_14_pro_max, pixel_7, galaxy_s20, ipad_mini, responsive, or explicit width/height", opts.Device)
		}
		cfg = DeviceEmulationConfig{
			Device:            preset.Device,
			Width:             preset.Width,
			Height:            preset.Height,
			DeviceScaleFactor: preset.DeviceScaleFactor,
			Mobile:            true,
			Touch:             true,
			UserAgent:         preset.UserAgent,
			Platform:          preset.Platform,
			MaxTouchPoints:    preset.MaxTouchPoints,
		}
	default:
		cfg.Device = "custom"
	}

	if opts.Width > 0 {
		cfg.Width = opts.Width
	}
	if opts.Height > 0 {
		cfg.Height = opts.Height
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return DeviceEmulationConfig{}, false, fmt.Errorf("width and height are required for %s emulation", emptyAs(cfg.Device, "custom"))
	}
	if cfg.Width > 10000 || cfg.Height > 10000 {
		return DeviceEmulationConfig{}, false, fmt.Errorf("width and height must be <= 10000 CSS pixels")
	}

	if opts.DeviceScaleFactor > 0 {
		cfg.DeviceScaleFactor = opts.DeviceScaleFactor
	}
	if cfg.DeviceScaleFactor <= 0 {
		cfg.DeviceScaleFactor = 2
	}
	if cfg.DeviceScaleFactor > 10 {
		return DeviceEmulationConfig{}, false, fmt.Errorf("device_scale_factor must be <= 10")
	}

	if opts.Mobile != nil {
		cfg.Mobile = *opts.Mobile
	} else if cfg.Device == "custom" || cfg.Device == "responsive" {
		cfg.Mobile = true
	}
	if opts.Touch != nil {
		cfg.Touch = *opts.Touch
	} else if (cfg.Device == "custom" || cfg.Device == "responsive") && cfg.Mobile {
		cfg.Touch = true
	}

	if opts.MaxTouchPoints > 0 {
		cfg.MaxTouchPoints = opts.MaxTouchPoints
	}
	if cfg.Touch && cfg.MaxTouchPoints == 0 {
		cfg.MaxTouchPoints = 5
	}
	if !cfg.Touch {
		cfg.MaxTouchPoints = 0
	}
	if cfg.MaxTouchPoints > 10 {
		return DeviceEmulationConfig{}, false, fmt.Errorf("max_touch_points must be <= 10")
	}

	if opts.UserAgent != "" {
		cfg.UserAgent = opts.UserAgent
	} else if (cfg.Device == "custom" || cfg.Device == "responsive") && cfg.Mobile {
		cfg.UserAgent = androidChromeUA
	}
	if opts.Platform != "" {
		cfg.Platform = opts.Platform
	} else if (cfg.Device == "custom" || cfg.Device == "responsive") && cfg.Mobile {
		cfg.Platform = "Linux armv8l"
	}

	orientation, err := normalizeDeviceOrientation(opts.Orientation, cfg.Width, cfg.Height)
	if err != nil {
		return DeviceEmulationConfig{}, false, err
	}
	cfg.Orientation = orientation
	if orientation == "landscape" && cfg.Width < cfg.Height {
		cfg.Width, cfg.Height = cfg.Height, cfg.Width
	}
	if orientation == "portrait" && cfg.Width > cfg.Height {
		cfg.Width, cfg.Height = cfg.Height, cfg.Width
	}

	return cfg, false, nil
}

func normalizeDeviceOrientation(raw string, width, height int64) (string, error) {
	switch normalizedDeviceKey(raw) {
	case "", "auto":
		if width > height {
			return "landscape", nil
		}
		return "portrait", nil
	case "portrait", "portraitprimary", "portraitsecondary":
		return "portrait", nil
	case "landscape", "landscapeprimary", "landscapesecondary":
		return "landscape", nil
	default:
		return "", fmt.Errorf("orientation must be portrait, landscape, or omitted")
	}
}

func normalizedDeviceKey(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func emptyAs(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
