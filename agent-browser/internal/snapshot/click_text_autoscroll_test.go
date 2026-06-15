package snapshot_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/revitt/agent-browser/internal/snapshot"
)

// belowFoldLinkPage places a target link far below the fold (large spacer) plus
// a same-text-prefix distractor in the viewport, so the test proves auto-scroll
// reaches an off-screen match and that opt-out keeps the click in the viewport.
const belowFoldLinkPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>fold test</title></head>
<body style="margin:0;padding:0">
  <a id="top" href="#" onclick="window.__clicked='top';return false;">Top Link</a>
  <div style="height:3000px"></div>
  <a id="deep" href="#" onclick="window.__clicked='deep';return false;">Alternative medicine</a>
</body></html>`

// runClickText evaluates ClickTextScript against the current page and returns
// the parsed result plus the page's window.__clicked marker after the click.
func runClickText(t *testing.T, ctx context.Context, opts snapshot.ClickTextOptions) (map[string]any, string) {
	t.Helper()
	optsJSON, err := json.Marshal(opts)
	if err != nil {
		t.Fatal(err)
	}
	expr := fmt.Sprintf("JSON.stringify(%s(%s))", snapshot.ClickTextScript, optsJSON)
	var raw string
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		obj, exception, evalErr := runtime.Evaluate(expr).WithReturnByValue(true).Do(ctx)
		if evalErr != nil {
			return evalErr
		}
		if exception != nil {
			return fmt.Errorf("exception: %s", exception.Text)
		}
		return json.Unmarshal(obj.Value, &raw)
	})); err != nil {
		t.Fatalf("evaluate ClickTextScript: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("unmarshal click result %q: %v", raw, err)
	}
	var clicked string
	_ = chromedp.Run(ctx, chromedp.Evaluate("window.__clicked || ''", &clicked))
	return result, clicked
}

func TestClickTextAutoScrollsToBelowFoldMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(belowFoldLinkPage))
	}))
	defer srv.Close()

	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()

	t.Run("auto_scroll default reaches below-fold link", func(t *testing.T) {
		ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
		defer ctxCancel()
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srv.URL),
			chromedp.WaitVisible("#top", chromedp.ByID),
			chromedp.Evaluate("window.scrollTo(0,0); window.__clicked='';", nil),
		); err != nil {
			t.Fatalf("navigate: %v", err)
		}
		result, clicked := runClickText(t, ctx, snapshot.ClickTextOptions{Text: "Alternative medicine"})
		if ok, _ := result["ok"].(bool); !ok {
			t.Fatalf("click_text failed for below-fold link (default auto_scroll): %#v", result)
		}
		if clicked != "deep" {
			t.Fatalf("clicked marker = %q, want deep (the below-fold link)", clicked)
		}
	})

	t.Run("auto_scroll=false refuses off-screen match", func(t *testing.T) {
		ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
		defer ctxCancel()
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srv.URL),
			chromedp.WaitVisible("#top", chromedp.ByID),
			chromedp.Evaluate("window.scrollTo(0,0); window.__clicked='';", nil),
		); err != nil {
			t.Fatalf("navigate: %v", err)
		}
		falsey := false
		result, clicked := runClickText(t, ctx, snapshot.ClickTextOptions{Text: "Alternative medicine", AutoScroll: &falsey})
		if ok, _ := result["ok"].(bool); ok {
			t.Fatalf("click_text should not click an off-screen match when auto_scroll=false: %#v", result)
		}
		if clicked == "deep" {
			t.Fatal("auto_scroll=false must not click the below-fold link")
		}
	})

	t.Run("in-viewport match still clicks without scroll dependence", func(t *testing.T) {
		ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
		defer ctxCancel()
		if err := chromedp.Run(ctx,
			chromedp.Navigate(srv.URL),
			chromedp.WaitVisible("#top", chromedp.ByID),
			chromedp.Evaluate("window.scrollTo(0,0); window.__clicked='';", nil),
		); err != nil {
			t.Fatalf("navigate: %v", err)
		}
		result, clicked := runClickText(t, ctx, snapshot.ClickTextOptions{Text: "Top Link"})
		if ok, _ := result["ok"].(bool); !ok {
			t.Fatalf("click_text failed for in-viewport link: %#v", result)
		}
		if clicked != "top" {
			t.Fatalf("clicked marker = %q, want top", clicked)
		}
	})
}
