package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
)

// responsiveFixture declares a viewport meta tag, so a correctly emulated phone
// must lay it out at the device width.
const responsiveFixture = `<html><head><meta name="viewport" content="width=device-width, initial-scale=1">` +
	`<title>responsive</title></head><body><h1>responsive</h1></body></html>`

// tallFixture has no viewport meta AND scrolls, so it catches the scrollbar
// trap: documentElement.clientWidth reads ~15px under the emulated width here
// even when the override is correct.
const tallFixture = `<html><head><title>tall</title></head><body><h1>tall</h1>` +
	`<div style="height:3000px"></div></body></html>`

func serveEmulationFixture(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func emulationTab(t *testing.T, m *Manager, ctx context.Context, url string) {
	t.Helper()
	var id target.ID
	if err := m.runBrowser(ctx, func(rc context.Context) error {
		var e error
		id, e = target.CreateTarget("about:blank").Do(rc)
		return e
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	m.refs.SetActive(string(id))
	if _, err := m.tabContext(string(id)); err != nil {
		t.Fatalf("tab context: %v", err)
	}
	if _, err := m.NavigateTo(ctx, url); err != nil {
		t.Fatalf("navigate: %v", err)
	}
}

// Chrome ignores a page's viewport meta tag when the device metrics override
// carries mobile:true, laying the page out at a fixed 980px instead of the
// emulated width. screen.*, DPR, user agent and touch points still emulate, so
// the override looks applied while every width-based media query evaluates
// against 980 — which silently defeats responsive testing, the thing device
// emulation is for.
func TestEmulateDeviceGivesAMobileLayoutViewport(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
	}{
		{"page with a viewport meta tag", responsiveFixture},
		// Regression guard for the scrollbar trap: measuring the layout viewport
		// with clientWidth reads 360 here against a 375 request, which made the
		// fallback look like it had failed and left the page at 980.
		{"scrolling page with no viewport meta tag", tallFixture},
	}

	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			emulationTab(t, m, ctx, serveEmulationFixture(t, tt.fixture))

			result, err := m.EmulateDevice(ctx, DeviceEmulationOptions{Device: "iphone_se"})
			if err != nil {
				t.Fatalf("EmulateDevice: %v", err)
			}
			if result.LayoutViewportWidth != 375 {
				t.Errorf("layout viewport = %d, want 375 (the emulated device width)", result.LayoutViewportWidth)
			}

			// What the page itself sees is the part that matters: a media query
			// evaluating against 980 means the caller tested a desktop layout.
			matched, err := m.Evaluate(ctx, `window.matchMedia('(max-width: 400px)').matches`)
			if err != nil {
				t.Fatalf("evaluate media query: %v", err)
			}
			if got, _ := matched.(bool); !got {
				t.Error("a phone-width media query must match under phone emulation")
			}

			// Dropping the mobile flag must not cost the rest of the emulation.
			for _, probe := range []struct {
				name string
				expr string
				want any
			}{
				{"screen width", `screen.width`, float64(375)},
				{"device pixel ratio", `devicePixelRatio`, float64(2)},
				{"touch points", `navigator.maxTouchPoints`, float64(5)},
				{"mobile user agent", `/iPhone/.test(navigator.userAgent)`, true},
			} {
				value, err := m.Evaluate(ctx, probe.expr)
				if err != nil {
					t.Errorf("evaluate %s: %v", probe.name, err)
					continue
				}
				if value != probe.want {
					t.Errorf("%s = %v, want %v", probe.name, value, probe.want)
				}
			}

			if _, err := m.EmulateDevice(ctx, DeviceEmulationOptions{Clear: true}); err != nil {
				t.Fatalf("clear: %v", err)
			}
			after, err := m.Evaluate(ctx, `window.innerWidth`)
			if err != nil {
				t.Fatalf("evaluate after clear: %v", err)
			}
			if got, _ := after.(float64); got == 375 {
				t.Error("clearing emulation should restore the real viewport width")
			}
		})
	}
}

// The reported width has to be the measured one, so a caller can tell what it
// actually tested rather than trusting the request it made.
func TestEmulateDeviceReportsTheMeasuredViewport(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	emulationTab(t, m, ctx, serveEmulationFixture(t, responsiveFixture))

	// A non-mobile override needs no fallback and must not claim one.
	result, err := m.EmulateDevice(ctx, DeviceEmulationOptions{
		Width: 1280, Height: 800,
	})
	if err != nil {
		t.Fatalf("EmulateDevice: %v", err)
	}
	if result.MobileLayoutFallback {
		t.Error("a desktop-width override needs no mobile fallback")
	}
	if result.LayoutViewportWidth != 1280 {
		t.Errorf("layout viewport = %d, want 1280", result.LayoutViewportWidth)
	}
}

func TestLayoutWidthMatches(t *testing.T) {
	tests := []struct {
		measured, requested int64
		want                bool
	}{
		{375, 375, true},
		// Chrome reports innerWidth a pixel over the layout width in some modes.
		{376, 375, true},
		{374, 375, true},
		// The failure this exists to catch.
		{980, 375, false},
		{981, 375, false},
		{1280, 375, false},
	}
	for _, tt := range tests {
		if got := layoutWidthMatches(tt.measured, tt.requested); got != tt.want {
			t.Errorf("layoutWidthMatches(%d, %d) = %v, want %v", tt.measured, tt.requested, got, tt.want)
		}
	}
}
