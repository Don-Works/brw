package browser

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
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
