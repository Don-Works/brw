package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/httpclient"
)

type screenshotSaveController struct {
	fakeController
	opts browser.ScreenshotSaveOptions
	tab  string
}

func (c *screenshotSaveController) SaveScreenshot(ctx context.Context, opts browser.ScreenshotSaveOptions) (browser.SavedScreenshot, error) {
	c.opts = opts
	c.tab = browser.TabIDFromContext(ctx)
	return browser.SavedScreenshot{Path: opts.SavePath, Width: 600, Height: 300, Bytes: 100000, Preview: &browser.Screenshot{MIMEType: "image/jpeg", Base64: "cHJldmlldw=="}}, nil
}

func TestScreenshotSaveHTTPForwardsToBrowserHost(t *testing.T) {
	controller := &screenshotSaveController{}
	host := httptest.NewServer(New("127.0.0.1:0", controller).server.Handler)
	defer host.Close()
	client, err := httpclient.New(host.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	opts := browser.ScreenshotSaveOptions{SavePath: "/capture-host/capture.png", Scale: 3, Hide: []string{".overlay"}, Region: &browser.ScreenshotRegion{X: 1, Y: 2, Width: 200, Height: 100}, SettleMS: 200, Preview: "small"}
	result, err := client.SaveScreenshot(browser.WithTabID(context.Background(), "42"), opts)
	if err != nil {
		t.Fatal(err)
	}
	if controller.opts.SavePath != opts.SavePath || controller.opts.Scale != 3 || controller.opts.Region.Width != 200 || controller.opts.Hide[0] != ".overlay" || controller.tab != "42" {
		t.Fatalf("lost forwarding: %+v tab=%s", controller.opts, controller.tab)
	}
	if result.Path != opts.SavePath || result.Preview == nil || result.Preview.Base64 != "cHJldmlldw==" {
		t.Fatalf("invalid metadata/preview: %+v", result)
	}
}

func TestScreenshotSaveHTTPUsesTheConsentCheckedQueryTab(t *testing.T) {
	controller := &screenshotSaveController{}
	server := New("127.0.0.1:0", controller)
	request := httptest.NewRequest(http.MethodPost, "/api/visual/screenshot_save?tab_id=42", strings.NewReader(`{"save_path":"/fixture/capture.png","tab_id":"99"}`))
	request.Host = "127.0.0.1"
	recorder := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || controller.tab != "42" {
		t.Fatalf("capture bypassed query target: status=%d tab=%s body=%s", recorder.Code, controller.tab, recorder.Body.String())
	}
}
