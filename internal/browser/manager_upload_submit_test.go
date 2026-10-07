package browser

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func TestManagerInlineUploadSurvivesSubsequentFormSubmit(t *testing.T) {
	received := make(chan string, 1)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			file, header, err := r.FormFile("file")
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer file.Close()
			data, err := io.ReadAll(file)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			value := header.Filename + ":" + string(data)
			received <- value
			fmt.Fprintf(w, `<!doctype html><title>received</title><main>received %s</main>`, value)
			return
		}
		fmt.Fprint(w, `<!doctype html><title>upload</title>
<form method="post" enctype="multipart/form-data">
  <label>File <input name="file" type="file"></label>
  <button type="submit">Upload</button>
</form>`)
	}))
	defer site.Close()

	oldRetention := uploadTempRetention
	uploadTempRetention = 750 * time.Millisecond
	t.Cleanup(func() { uploadTempRetention = oldRetention })

	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opened, err := m.Open(ctx, site.URL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)
	snap, err := m.Snapshot(tabCtx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var inputRef, submitRef string
	for _, el := range snap.Elements {
		if el.Tag == "input" && el.Type == "file" {
			inputRef = el.Ref
		}
		if el.Role == "button" && strings.Contains(el.Name, "Upload") {
			submitRef = el.Ref
		}
	}
	if inputRef == "" || submitRef == "" {
		t.Fatalf("upload controls missing: %+v", snap.Elements)
	}
	if _, err := m.UploadFile(tabCtx, snapshot.UploadOptions{
		Ref:         inputRef,
		BytesBase64: base64.StdEncoding.EncodeToString([]byte("payload survives")),
		Filename:    "proof.txt",
	}); err != nil {
		t.Fatalf("populate file input: %v", err)
	}
	if _, err := m.Click(tabCtx, submitRef); err != nil {
		t.Fatalf("submit form: %v", err)
	}
	select {
	case got := <-received:
		if got != "proof.txt:payload survives" {
			t.Fatalf("server received %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("form submission never received the retained upload bytes")
	}
}

func TestCancelledChooserUploadRestoresManualSelection(t *testing.T) {
	clicked := make(chan struct{}, 1)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/clicked" {
			select {
			case clicked <- struct{}{}:
			default:
			}
			return
		}
		fmt.Fprint(w, `<!doctype html><title>chooser cleanup</title><input id="file" type="file"><button onclick="fetch('/clicked')">No chooser</button>`)
	}))
	defer site.Close()
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opened, err := m.Open(ctx, site.URL)
	if err != nil {
		t.Fatal(err)
	}
	tabCtx, err := m.tabContext(opened.Tab.ID)
	if err != nil {
		t.Fatal(err)
	}
	chooser := make(chan *page.EventFileChooserOpened, 2)
	chromedp.ListenTarget(tabCtx, func(event any) {
		if event, ok := event.(*page.EventFileChooserOpened); ok {
			select {
			case chooser <- event:
			default:
			}
		}
	})
	clickFile := func() {
		t.Helper()
		if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			_, exception, err := runtime.Evaluate("document.getElementById('file').click()").WithUserGesture(true).Do(ctx)
			if exception != nil {
				return fmt.Errorf("file selection: %s", FormatRuntimeException(exception))
			}
			return err
		})); err != nil {
			t.Fatal(err)
		}
	}
	if err := chromedp.Run(tabCtx, page.SetInterceptFileChooserDialog(true)); err != nil {
		t.Fatal(err)
	}
	clickFile()
	select {
	case event := <-chooser:
		if err := chromedp.Run(tabCtx, dom.SetFileInputFiles([]string{}).WithBackendNodeID(event.BackendNodeID)); err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("interception control did not report the native file chooser")
	}
	if err := chromedp.Run(tabCtx, page.SetInterceptFileChooserDialog(false)); err != nil {
		t.Fatal(err)
	}
	callCtx, cancelCall := context.WithCancel(WithTabID(ctx, opened.Tab.ID))
	defer cancelCall()
	done := make(chan error, 1)
	go func() {
		_, err := m.UploadFile(callCtx, snapshot.UploadOptions{ClickText: "No chooser", BytesBase64: base64.StdEncoding.EncodeToString([]byte("fixture")), Filename: "fixture.txt"})
		done <- err
	}()
	select {
	case <-clicked:
	case <-time.After(5 * time.Second):
		t.Fatal("upload trigger was never clicked")
	}
	cancelCall()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled upload = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled upload did not stop")
	}
	clickFile()
	select {
	case <-chooser:
		t.Fatal("cancelled upload left manual file selection intercepted")
	case <-time.After(400 * time.Millisecond):
	}
}
