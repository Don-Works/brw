package browser

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

func TestScreenshotSaveCompositor(t *testing.T) {
	t.Setenv("BRW_SCREENSHOT_ALLOW_OUTSIDE_HOME", "1")
	m := newHeadlessManagerWith(t, chromedp.Flag("disable-gpu", false), chromedp.Flag("use-angle", "swiftshader"), chromedp.Flag("enable-unsafe-swiftshader", true), chromedp.Flag("force-device-scale-factor", "2"))
	fixtures := http.FileServer(http.Dir("../../tests/fixtures"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow.png" {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
			pixel := image.NewRGBA(image.Rect(0, 0, 1, 1))
			pixel.SetRGBA(0, 0, color.RGBA{B: 255, A: 255})
			w.Header().Set("Content-Type", "image/png")
			_ = png.Encode(w, pixel)
			return
		}
		fixtures.ServeHTTP(w, r)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	opened, err := m.Open(ctx, srv.URL+"/presentation.html")
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithTabID(ctx, opened.Tab.ID)
	if err := m.WaitFor(ctx, "fn:window.fixtureReady === true", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot(ctx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatal(err)
	}
	ref := ""
	for _, e := range snap.Elements {
		if e.Name == "Rendered media" {
			ref = e.Ref
		}
	}
	if ref == "" {
		t.Fatal("missing media ref")
	}
	dir := t.TempDir()
	capture := func(name string, o ScreenshotSaveOptions) (SavedScreenshot, image.Image) {
		t.Helper()
		o.SavePath = filepath.Join(dir, name+"."+o.Format)
		result, err := m.SaveScreenshot(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(result.Path)
		if err != nil {
			t.Fatal(err)
		}
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		return result, img
	}
	assertColor := func(img image.Image, x, y int, want string) {
		t.Helper()
		r, g, b, _ := img.At(x, y).RGBA()
		switch want {
		case "red":
			if r < 55000 || g > 8000 || b > 8000 {
				t.Fatalf("WebGL pixel: %d %d %d", r, g, b)
			}
		case "green":
			if r > 8000 || g < 55000 || b > 8000 {
				t.Fatalf("video pixel: %d %d %d", r, g, b)
			}
		case "blue":
			if r > 8000 || g > 8000 || b < 55000 {
				t.Fatalf("overlay not restored: %d %d %d", r, g, b)
			}
		}
	}
	for _, scale := range []int{1, 2, 3} {
		for _, element := range []bool{false, true} {
			o := ScreenshotSaveOptions{Scale: scale, Format: "png", Hide: []string{"#overlay"}}
			if element {
				o.Ref = ref
			} else {
				o.Region = &ScreenshotRegion{X: 20, Y: 40, Width: 320, Height: 100}
			}
			result, img := capture(fmt.Sprintf("clip-%d-%t", scale, element), o)
			if result.Width != 320*scale || result.Height != 100*scale {
				t.Fatalf("scale %d: dimensions %dx%d", scale, result.Width, result.Height)
			}
			assertColor(img, 80*scale, 50*scale, "red")
			assertColor(img, 240*scale, 50*scale, "green")
		}
	}
	fractional, fractionalImage := capture("fractional", ScreenshotSaveOptions{Format: "png", Scale: 3, Region: &ScreenshotRegion{X: 20.5, Y: 40.5, Width: 100.5, Height: 50.5}, Hide: []string{"#overlay"}})
	if fractional.Width != 302 || fractional.Height != 152 {
		t.Fatalf("fractional dimensions: %dx%d", fractional.Width, fractional.Height)
	}
	assertColor(fractionalImage, 50, 50, "red")

	for _, format := range []string{"jpeg", "webp"} {
		result, img := capture(format, ScreenshotSaveOptions{Format: format, Scale: 2, Region: &ScreenshotRegion{X: 20, Y: 40, Width: 320, Height: 100}, Hide: []string{"#overlay"}})
		if result.Width != 640 || result.Height != 200 {
			t.Fatalf("%s dimensions: %+v", format, result)
		}
		assertColor(img, 160, 100, "red")
		assertColor(img, 480, 100, "green")
	}
	result, img := capture("full", ScreenshotSaveOptions{Format: "png", FullPage: true, OmitBackground: true, Preview: "none"})
	if result.Height < 1600 || result.Preview != nil {
		t.Fatalf("full page: %+v", result)
	}
	_, _, _, a := img.At(1, 1).RGBA()
	if a != 0 {
		t.Fatalf("background alpha=%d", a)
	}
	assertColor(img, 100, 80, "blue")
	fullRed, fullGreen, fullBlue, _ := img.At(50, 1525).RGBA()
	if fullRed < 60000 || fullGreen > 3000 || fullBlue < 60000 {
		t.Fatal("full-page capture lost content below the viewport")
	}
	_, err = m.SaveScreenshot(ctx, ScreenshotSaveOptions{SavePath: filepath.Join(dir, "failure.png"), Hide: []string{"["}})
	if err == nil {
		t.Fatal("invalid selector accepted")
	}
	_, img = capture("restored", ScreenshotSaveOptions{Format: "png", Region: &ScreenshotRegion{X: 20, Y: 40, Width: 320, Height: 100}})
	assertColor(img, 80, 50, "blue")
	_, tabCtx, tabCancel, err := m.activeContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tabCancel()
	var ignored any
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(`scrollTo(0,1450)`, &ignored)); err != nil {
		t.Fatal(err)
	}
	var scrollY float64
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(`window.scrollY`, &scrollY)); err != nil {
		t.Fatal(err)
	}
	_, scrolled := capture("scrolled", ScreenshotSaveOptions{Format: "png", Region: &ScreenshotRegion{X: 20, Y: 1500 - scrollY, Width: 100, Height: 50}})
	red, green, blue, _ := scrolled.At(50, 25).RGBA()
	if red < 60000 || green > 3000 || blue < 60000 {
		t.Fatalf("scrolled crop: %d %d %d", red, green, blue)
	}
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(`scrollTo(0,0)`, &ignored)); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(`(()=>{const img=document.createElement('img');img.id='late';img.style.cssText='position:absolute;left:20px;top:40px;width:10px;height:10px;z-index:3';img.src='/slow.png';document.body.append(img);})()`, &ignored)); err != nil {
		t.Fatal(err)
	}
	_, loaded := capture("loaded-image", ScreenshotSaveOptions{Format: "png", Region: &ScreenshotRegion{X: 20, Y: 40, Width: 10, Height: 10}, Hide: []string{"#overlay"}})
	assertColor(loaded, 5, 5, "blue")
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(`document.querySelector('#late').remove()`, &ignored)); err != nil {
		t.Fatal(err)
	}

	cancelCtx, cancelCapture := context.WithCancel(ctx)
	time.AfterFunc(100*time.Millisecond, cancelCapture)
	cancelledPath := filepath.Join(dir, "cancelled.png")
	_, err = m.SaveScreenshot(cancelCtx, ScreenshotSaveOptions{SavePath: cancelledPath, Hide: []string{"#overlay"}, SettleMS: 1000})
	if err == nil {
		t.Fatal("cancelled capture succeeded")
	}
	if _, err := os.Stat(cancelledPath); !os.IsNotExist(err) {
		t.Fatalf("cancelled capture wrote a file: %v", err)
	}
	var clean bool
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(`getComputedStyle(document.querySelector('#overlay')).visibility==='visible' && !Object.keys(window).some(k=>k.startsWith('__brwCapture_'))`, &clean)); err != nil {
		t.Fatal(err)
	}
	if !clean {
		t.Fatal("cancellation left temporary page state")
	}

}
