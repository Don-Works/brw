package browser

import (
	"bytes"
	"context"
	"errors"
	"image"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/devtools"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

func TestRuntimeDestinationGateProtectsOutputsWithoutNavigationPolicy(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>Private fixture</title></head><body><h1>Private fixture</h1><button>Private action</button><p>Private fixture body.</p></body></html>`))
	}))
	defer site.Close()
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	opened, err := m.Open(ctx, site.URL)
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("fixture runtime destination refused")
	ctx = WithFrameReadCheck(WithTabID(ctx, opened.Tab.ID), func(raw string) error {
		if !strings.HasPrefix(raw, site.URL) {
			t.Fatalf("checked URL %q, want fixture origin", raw)
		}
		return denied
	})
	t.Setenv("BRW_SCREENSHOT_ALLOW_OUTSIDE_HOME", "1")
	savePath := filepath.Join(t.TempDir(), "refused.png")
	for name, call := range map[string]func(context.Context) (any, error){
		"snapshot":        func(ctx context.Context) (any, error) { return m.Snapshot(ctx, snapshot.SnapshotOptions{}) },
		"find":            func(ctx context.Context) (any, error) { return m.Find(ctx, snapshot.FindOptions{Text: "Private"}) },
		"read":            func(ctx context.Context) (any, error) { return m.Read(ctx) },
		"structured read": func(ctx context.Context) (any, error) { return m.ReadData(ctx) },
		"observe":         func(ctx context.Context) (any, error) { return m.Observe(ctx) },
		"evaluate":        func(ctx context.Context) (any, error) { return m.Evaluate(ctx, "document.title") },
		"screenshot":      func(ctx context.Context) (any, error) { return m.Screenshot(ctx) },
		"annotated screenshot": func(ctx context.Context) (any, error) {
			return m.ScreenshotAnnotated(ctx, AnnotatedScreenshotOptions{})
		},
		"PDF": func(ctx context.Context) (any, error) { return m.CapturePDF(ctx) },
		"PDF stream": func(ctx context.Context) (any, error) {
			r, err := m.CapturePDFStream(ctx)
			if r != nil {
				defer r.Close()
			}
			return r, err
		},
		"saved screenshot": func(ctx context.Context) (any, error) {
			return m.SaveScreenshot(ctx, ScreenshotSaveOptions{SavePath: savePath, Preview: "none"})
		},
		"vitals": func(ctx context.Context) (any, error) { return m.Vitals(ctx, devtools.VitalsOptions{}) },
		"highlight": func(ctx context.Context) (any, error) {
			return m.Highlight(ctx, devtools.HighlightOptions{Clear: true})
		},
		"network requests": func(ctx context.Context) (any, error) { return m.NetworkRequests(ctx, "") },
		"network capture":  func(ctx context.Context) (any, error) { return m.NetworkCapture(ctx, "") },
		"request replay": func(ctx context.Context) (any, error) {
			return m.ReplayRequest(ctx, ReplayRequestParams{URL: site.URL})
		},
		"React":   func(ctx context.Context) (any, error) { return m.React(ctx, ReactOptions{Action: "tree"}) },
		"cookies": func(ctx context.Context) (any, error) { return m.Cookies(ctx, CookieParams{Action: CookieActionList}) },
		"console": func(ctx context.Context) (any, error) { return m.ConsoleMessages(ctx) },
		"dialog":  func(ctx context.Context) (any, error) { return m.Dialog(ctx, DialogOptions{Action: "status"}) },
		"route":   func(ctx context.Context) (any, error) { return m.Route(ctx, RouteOptions{Action: "list"}) },
		"profile": func(ctx context.Context) (any, error) {
			return m.Profile(ctx, ProfileOptions{Action: "start", Kind: ProfileKindCPU})
		},
		"screencast": func(ctx context.Context) (any, error) {
			frames, stop, err := m.ScreencastFrames(ctx, ScreencastOptions{})
			if stop != nil {
				defer stop()
			}
			return frames, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := call(ctx)
			if !errors.Is(err, denied) {
				t.Fatalf("operation = %v, want runtime destination refusal", err)
			}
			if out != nil && !reflect.ValueOf(out).IsZero() {
				t.Fatalf("refused operation exposed a %T result", out)
			}
		})
	}
	if _, err := os.Stat(savePath); !os.IsNotExist(err) {
		t.Fatalf("refused screenshot was persisted: %v", err)
	}
	tabID, tabCtx, cancelTab, err := m.activeContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelTab()
	result := m.observeActionWithBefore(tabID, tabCtx, "Private fixture description", nil)
	if result.OK || strings.Contains(result.Message, "Private fixture") || result.URL != "" || result.Title != "" || len(result.Elements) != 0 || result.Snapshot != nil {
		t.Fatalf("refused action retained page-derived result fields: %+v", result)
	}
}

func TestScreencastRuntimeGatePausesUntilNewDocumentIsAuthorized(t *testing.T) {
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body style="margin:0;background:#00c"></body></html>`))
	}))
	defer public.Close()
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body style="margin:0;background:#c00"></body></html>`))
	}))
	defer private.Close()
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	opened, err := m.Open(ctx, public.URL)
	if err != nil {
		t.Fatal(err)
	}
	tabCtx, err := m.tabContext(opened.Tab.ID)
	if err != nil {
		t.Fatal(err)
	}
	checked := make(chan struct{}, 1)
	release := make(chan struct{})
	releaseCheck := sync.OnceFunc(func() { close(release) })
	defer releaseCheck()
	ctx = WithFrameReadCheck(WithTabID(ctx, opened.Tab.ID), func(raw string) error {
		if strings.HasPrefix(raw, private.URL) {
			select {
			case checked <- struct{}{}:
			default:
			}
			<-release
			return errors.New("fixture private video refused")
		}
		return nil
	})
	frames, stop, err := m.ScreencastFrames(ctx, ScreencastOptions{MaxWidth: 320, MaxHeight: 240})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	select {
	case <-frames:
	case <-time.After(3 * time.Second):
		t.Fatal("public screencast produced no frame")
	}
	if err := chromedp.Run(tabCtx, chromedp.Navigate(private.URL)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-checked:
	case <-time.After(3 * time.Second):
		t.Fatal("new document reached video without an origin check")
	}
	privatePixels := func(data []byte) bool {
		t.Helper()
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		r, g, b, _ := img.At(img.Bounds().Dx()/2, img.Bounds().Dy()/2).RGBA()
		return r > g*3 && r > b*3
	}
	var control []byte
	if err := chromedp.Run(tabCtx, chromedp.CaptureScreenshot(&control)); err != nil {
		t.Fatal(err)
	}
	if !privatePixels(control) {
		t.Fatal("private page did not paint the positive control")
	}
	deadline := time.NewTimer(200 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatal("stream closed before the runtime decision")
			}
			if privatePixels(frame.Data) {
				t.Fatal("video exposed private document pixels before authorization")
			}
		case <-deadline.C:
			releaseCheck()
			for {
				select {
				case frame, ok := <-frames:
					if !ok {
						return
					}
					if privatePixels(frame.Data) {
						t.Fatal("video exposed private document pixels after refusal")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("runtime refusal did not stop the video stream")
				}
			}
		}
	}
}

func TestReadRuntimeDestinationGateChecksDocumentRollover(t *testing.T) {
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><title>Private document</title><body><article><h1>Private document</h1><p>Private document content must not reach a read response after the preliminary snapshot.</p></article></body></html>`))
	}))
	defer private.Close()
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><title>Public document</title><body><p>Public document.</p></body></html>`))
	}))
	defer public.Close()
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	opened, err := m.Open(ctx, public.URL)
	if err != nil {
		t.Fatal(err)
	}
	tabCtx, err := m.tabContext(opened.Tab.ID)
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("fixture private document refused")
	rolled := false
	ctx = WithFrameReadCheck(WithTabID(ctx, opened.Tab.ID), func(raw string) error {
		if strings.HasPrefix(raw, private.URL) {
			return denied
		}
		if !rolled {
			rolled = true
			if err := chromedp.Run(tabCtx, chromedp.Navigate(private.URL)); err != nil {
				return err
			}
		}
		return nil
	})
	out, err := m.Read(ctx)
	if !rolled || !errors.Is(err, denied) || !reflect.ValueOf(out).IsZero() {
		t.Fatalf("read after rollover returned a %T result, err=%v, rolled=%t", out, err, rolled)
	}
}

func TestOpenRuntimeDestinationGateChecksRedirectWithoutNavigationPolicy(t *testing.T) {
	denied := errors.New("fixture private destination refused")
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><title>Private fixture</title><body>Private fixture</body></html>`))
	}))
	defer private.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, private.URL, http.StatusFound) }))
	defer redirect.Close()
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	ctx = WithFrameReadCheck(ctx, func(raw string) error {
		if strings.HasPrefix(raw, private.URL) {
			return denied
		}
		return nil
	})
	for name, open := range map[string]func(context.Context, string) (OpenResult, error){"ordinary": m.Open, "incognito": m.OpenIncognito} {
		t.Run(name, func(t *testing.T) {
			if out, err := open(ctx, redirect.URL); !errors.Is(err, denied) || out.Tab.ID != "" {
				t.Fatalf("redirect open = %+v, %v; want refusal without a tab result", out, err)
			}
		})
	}
	tabs, err := m.ListTabs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tab := range tabs {
		if strings.HasPrefix(tab.URL, private.URL) {
			t.Fatalf("refused redirected tab remains open: %+v", tab)
		}
	}
}
