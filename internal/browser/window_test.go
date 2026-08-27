package browser

import (
	"context"
	"strings"
	"testing"
	"time"
)

func intPtr(v int) *int { return &v }

func TestValidateWindowResizeRejectsNoOp(t *testing.T) {
	// A request that changes nothing must be an error, not a success that did
	// nothing — otherwise an agent reads ok:true and moves on.
	if err := ValidateWindowResize(WindowResizeOptions{}); err == nil {
		t.Fatal("empty resize accepted")
	}
	if err := ValidateWindowResize(WindowResizeOptions{Width: -1}); err == nil {
		t.Fatal("negative width accepted")
	}
	for _, opts := range []WindowResizeOptions{
		{Width: 800},
		{Height: 600},
		{Left: intPtr(0)},
		{Top: intPtr(0)},
		{State: "maximized"},
	} {
		if err := ValidateWindowResize(opts); err != nil {
			t.Errorf("ValidateWindowResize(%+v) = %v, want accepted", opts, err)
		}
	}
}

func TestNormalizeWindowState(t *testing.T) {
	for _, in := range []string{"normal", "MINIMIZED", " maximized ", "fullscreen"} {
		got, err := NormalizeWindowState(in)
		if err != nil {
			t.Errorf("NormalizeWindowState(%q) = %v", in, err)
		}
		if got != strings.ToLower(strings.TrimSpace(in)) {
			t.Errorf("NormalizeWindowState(%q) = %q", in, got)
		}
	}
	if got, err := NormalizeWindowState(""); got != "" || err != nil {
		t.Fatalf("empty state = (%q, %v), want (\"\", nil)", got, err)
	}
	_, err := NormalizeWindowState("kiosk")
	if err == nil {
		t.Fatal("unknown window state accepted")
	}
	if !strings.Contains(err.Error(), "kiosk") {
		t.Fatalf("error does not name the bad state: %v", err)
	}
}

// resizeWasClamped must not treat an intentional maximize as a clamp, or every
// maximize would come back with a misleading warning.
func TestResizeWasClamped(t *testing.T) {
	applied := WindowResizeResult{Width: 1200, Height: 800}
	if resizeWasClamped(WindowResizeOptions{Width: 1200, Height: 800}, applied) {
		t.Error("exact match reported as clamped")
	}
	if !resizeWasClamped(WindowResizeOptions{Width: 99999}, applied) {
		t.Error("oversized request not reported as clamped")
	}
	if resizeWasClamped(WindowResizeOptions{Width: 99999, State: "maximized"}, applied) {
		t.Error("maximize reported as clamped")
	}
}

func TestManagerResizeWindowAppliesAndReadsBack(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	opened, err := m.Open(ctx, "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)

	got, err := m.ResizeWindow(tabCtx, WindowResizeOptions{Width: 900, Height: 700})
	if err != nil {
		t.Fatal(err)
	}
	if !got.OK {
		t.Fatalf("resize reported not ok: %+v", got)
	}
	// The result must be Chrome's applied geometry, not an echo of the request,
	// so a caller can trust it without a second read.
	if got.Width == 0 || got.Height == 0 {
		t.Fatalf("resize returned no geometry: %+v", got)
	}
	if !got.Clamped && (got.Width != 900 || got.Height != 700) {
		t.Fatalf("resize applied %dx%d without reporting a clamp", got.Width, got.Height)
	}

	// A second resize to a different size must move the reported geometry with
	// it. Checked through CDP rather than in-page window.outerWidth, which is 0
	// under headless Chrome and would test the harness instead of the feature.
	smaller, err := m.ResizeWindow(tabCtx, WindowResizeOptions{Width: 640, Height: 480})
	if err != nil {
		t.Fatal(err)
	}
	if smaller.Width == got.Width && smaller.Height == got.Height {
		t.Fatalf("second resize reported the same geometry as the first (%dx%d); the result is echoing the request, not reading back",
			smaller.Width, smaller.Height)
	}
	if !smaller.Clamped && (smaller.Width != 640 || smaller.Height != 480) {
		t.Fatalf("second resize applied %dx%d without reporting a clamp", smaller.Width, smaller.Height)
	}
}

func TestManagerResizeWindowRejectsBadInput(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := m.Open(ctx, "about:blank"); err != nil {
		t.Fatal(err)
	}

	if _, err := m.ResizeWindow(ctx, WindowResizeOptions{}); err == nil {
		t.Error("empty resize accepted")
	}
	if _, err := m.ResizeWindow(ctx, WindowResizeOptions{State: "kiosk"}); err == nil {
		t.Error("unknown window state accepted")
	}
}
