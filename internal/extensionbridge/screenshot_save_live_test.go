package extensionbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/browsertest"
	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/snapshot"
)

func TestScreenshotSaveExtensionCompositor(t *testing.T) {
	browsers := installedBrowsers()
	if len(browsers) == 0 {
		t.Skip("Chrome/Chromium unavailable")
	}
	if strings.Contains(strings.ToLower(filepath.Base(browsers[0])), "google chrome") || strings.HasPrefix(strings.ToLower(filepath.Base(browsers[0])), "google-chrome") {
		t.Skip("unbranded Chromium required for unpacked extensions")
	}
	t.Setenv("BRW_SCREENSHOT_ALLOW_OUTSIDE_HOME", "1")
	b := New("", 30*time.Second, "")
	token, err := NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	b.SetAuthToken(token)
	bridgeServer := httptest.NewServer(b.server.Handler)
	defer bridgeServer.Close()
	fixture := httptest.NewServer(http.FileServer(http.Dir("../../tests/fixtures")))
	defer fixture.Close()
	extension := t.TempDir()
	if err := os.CopyFS(extension, os.DirFS("../../extension")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(extension, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["background"] = map[string]any{"service_worker": "test_bootstrap.js", "type": "module"}
	raw, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extension, "manifest.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	bootstrap := fmt.Sprintf(`import './service_worker.js'; chrome.storage.local.set({brwBridgeConfig:{bridgeUrl:%q},brwBrowserControlConsent:{granted:true,version:1,grantedAt:new Date().toISOString()}});`, "ws"+strings.TrimPrefix(bridgeServer.URL, "http")+"/extension")
	if err := os.WriteFile(filepath.Join(extension, "test_bootstrap.js"), []byte(bootstrap), 0600); err != nil {
		t.Fatal(err)
	}
	profile := browsertest.NewProfile(t)
	launcher, err := cdp.Launch(context.Background(), cdp.LaunchConfig{ChromePath: browsers[0], UserDataDir: profile.Dir(), Extensions: []string{extension}, Headless: true, Args: append(quietLaunchArgs(), "--use-angle=swiftshader", "--enable-unsafe-swiftshader", "--force-device-scale-factor=2")})
	if err != nil {
		t.Fatal(err)
	}
	profile.StopWith(func() { _ = launcher.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	readyCtx, readyCancel := context.WithTimeout(ctx, 20*time.Second)
	defer readyCancel()
	if _, err := b.getConn(readyCtx); err != nil {
		t.Fatalf("unpacked extension did not connect: %v", err)
	}
	opened, err := b.Open(ctx, fixture.URL+"/presentation.html")
	if err != nil {
		t.Fatal(err)
	}
	ctx = browser.WithTabID(ctx, opened.Tab.ID)
	if err := b.WaitFor(ctx, "fn:window.fixtureReady === true", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	snap, err := b.Snapshot(ctx, snapshot.SnapshotOptions{Mode: "all"})
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
		t.Fatal("media ref not found")
	}
	other, err := b.Open(ctx, fixture.URL+"/content.html")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, scale := range []int{1, 2, 3} {
		for _, element := range []bool{false, true} {
			opts := browser.ScreenshotSaveOptions{SavePath: filepath.Join(dir, fmt.Sprintf("capture-%d-%t.png", scale, element)), Scale: scale, Hide: []string{"#overlay"}}
			if element {
				opts.Ref = ref
			} else {
				opts.Region = &browser.ScreenshotRegion{X: 20, Y: 40, Width: 320, Height: 100}
			}
			result, err := b.SaveScreenshot(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			if result.Width != 320*scale || result.Height != 100*scale {
				t.Fatalf("dimensions: %dx%d", result.Width, result.Height)
			}
			data, err := os.ReadFile(result.Path)
			if err != nil {
				t.Fatal(err)
			}
			img, _, err := image.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			r, g, blue, _ := img.At(80*scale, 50*scale).RGBA()
			if r < 60000 || g > 3000 || blue > 3000 {
				t.Fatalf("WebGL: %d %d %d", r, g, blue)
			}
			r, g, blue, _ = img.At(240*scale, 50*scale).RGBA()
			if g < 60000 || r > 3000 || blue > 3000 {
				t.Fatalf("video: %d %d %d", r, g, blue)
			}
		}
	}
	for _, format := range []string{"jpeg", "webp"} {
		result, err := b.SaveScreenshot(ctx, browser.ScreenshotSaveOptions{SavePath: filepath.Join(dir, "capture."+format), Format: format, Scale: 2, Ref: ref, Hide: []string{"#overlay"}})
		if err != nil {
			t.Fatal(err)
		}
		if result.Width != 640 || result.Height != 200 || result.MIMEType != "image/"+format {
			t.Fatalf("format %s: %+v", format, result)
		}
	}
	for _, format := range []string{"png", "jpeg", "webp"} {
		result, err := b.SaveScreenshot(ctx, browser.ScreenshotSaveOptions{SavePath: filepath.Join(dir, "fractional."+format), Format: format, Scale: 3, Region: &browser.ScreenshotRegion{X: 20.5, Y: 40.5, Width: 100.5, Height: 50.5}, Hide: []string{"#overlay"}})
		if err != nil {
			t.Fatal(err)
		}
		if result.Width != 302 || result.Height != 152 {
			t.Fatalf("fractional %s dimensions: %dx%d", format, result.Width, result.Height)
		}
		data, err := os.ReadFile(result.Path)
		if err != nil {
			t.Fatal(err)
		}
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		red, green, blue, _ := img.At(50, 50).RGBA()
		if red < 55000 || green > 8000 || blue > 8000 {
			t.Fatalf("fractional %s lost WebGL pixels: %d %d %d", format, red, green, blue)
		}
	}

	full, err := b.SaveScreenshot(ctx, browser.ScreenshotSaveOptions{SavePath: filepath.Join(dir, "full.png"), FullPage: true, OmitBackground: true, Hide: []string{"#overlay"}, Preview: "none"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(full.Path)
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, alpha := img.At(1, 1).RGBA()
	if alpha != 0 || full.Height < 1600 || full.Preview != nil {
		t.Fatalf("full page: alpha=%d height=%d", alpha, full.Height)
	}
	red, green, blue, _ := img.At(50, 1525).RGBA()
	if red < 60000 || green > 3000 || blue < 60000 {
		t.Fatal("full-page capture lost content below the viewport")
	}
	red, green, blue, _ = img.At(100, 80).RGBA()
	if red < 60000 || green > 3000 || blue > 3000 {
		t.Fatal("full-page capture lost WebGL content")
	}

	tabs, err := b.ListTabs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restored := false
	for _, tab := range tabs {
		if tab.ID == other.Tab.ID && tab.Active {
			restored = true
		}
	}
	if !restored {
		t.Fatal("original active tab was not restored")
	}

	var hidden string
	if err := b.evaluate(ctx, `getComputedStyle(document.querySelector('#overlay')).visibility`, "", &hidden); err != nil {
		t.Fatal(err)
	}
	if hidden != "visible" {
		t.Fatal("hidden selector was not restored")
	}
}
