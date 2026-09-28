package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

type screenshotSaveController struct {
	fakeController
	opts browser.ScreenshotSaveOptions
	tab  string
}

func (c *screenshotSaveController) SaveScreenshot(ctx context.Context, opts browser.ScreenshotSaveOptions) (browser.SavedScreenshot, error) {
	c.opts = opts
	c.tab = browser.TabIDFromContext(ctx)
	result := browser.SavedScreenshot{Path: opts.SavePath, Width: 2400, Height: 1200, Bytes: 9000000, SHA256: strings.Repeat("a", 64), MIMEType: "image/png"}
	if opts.Preview != "none" {
		result.Preview = &browser.Screenshot{MIMEType: "image/jpeg", Base64: "c21hbGw="}
	}
	return result, nil
}

func TestScreenshotSaveMCPReturnsOnlyMetadataAndPreview(t *testing.T) {
	for _, preview := range []string{"small", "none"} {
		c := &screenshotSaveController{}
		args := json.RawMessage(`{"save_path":"/fixture/docs/hero.png","scale":3,"hide":[".overlay"],"region":{"x":1,"y":2,"width":800,"height":400},"preview":"` + preview + `"}`)
		result, rpcErr := New(c).callTool(browser.WithTabID(context.Background(), "42"), "brw_screenshot_save", args)
		if rpcErr != nil {
			t.Fatal(rpcErr)
		}
		raw, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Content  []toolContent           `json:"content"`
			Metadata browser.SavedScreenshot `json:"structuredContent"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if c.opts.Scale != 3 || c.opts.Region.Width != 800 || c.tab != "42" {
			t.Fatalf("options lost: %+v tab=%s", c.opts, c.tab)
		}
		if decoded.Metadata.Width != 2400 || decoded.Metadata.Preview != nil || decoded.Content[0].Type != "text" {
			t.Fatalf("unexpected result: %s", raw)
		}
		if preview == "none" {
			if len(decoded.Content) != 1 {
				t.Fatal("none returned an image")
			}
		} else if len(decoded.Content) != 2 || decoded.Content[1].Data != "c21hbGw=" || decoded.Content[1].MIMEType != "image/jpeg" {
			t.Fatalf("unexpected preview: %s", raw)
		}
	}
}
