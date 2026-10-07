package artifact

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

func TestDownloadCaptureRechecksStoredSourcePermission(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "listed", true: "cached"}[cached], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture.txt")
			payload := []byte("synthetic protected download")
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			entry := browser.DownloadEntry{GUID: "fixture-guid", URL: "https://protected.example.test/file", State: "completed", Path: path, SuggestedFilename: "fixture.txt"}
			fake := &downloadServiceFakeBrowser{result: browser.DownloadsResult{Supported: true, FilePaths: true, Downloads: []browser.DownloadEntry{entry}}}
			service, err := NewService(newTestStore(t, 1<<20, 2<<20), fake)
			if err != nil {
				t.Fatal(err)
			}
			opens := 0
			service.downloadSourceOpener = func(name string) (*os.File, error) { opens++; return os.Open(name) }
			opts := CaptureOptions{Kind: "download", DownloadGUID: entry.GUID}
			capture := func(ctx context.Context) (Meta, error) {
				if cached {
					return service.CaptureCompletedDownload(ctx, entry, opts)
				}
				return service.CaptureArtifact(ctx, opts)
			}
			denied := errors.New("source permission refused")
			ctx := browser.WithFrameReadCheck(context.Background(), func(string) error { return denied })
			if meta, err := capture(ctx); !errors.Is(err, denied) || meta.ID != "" || opens != 0 {
				t.Fatalf("refused capture: meta=%+v err=%v source opens=%d", meta, err, opens)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != string(payload) {
				t.Fatalf("refusal changed source: %q err=%v", got, err)
			}
			checked := ""
			ctx = browser.WithFrameReadCheck(context.Background(), func(url string) error { checked = url; return nil })
			if meta, err := capture(ctx); err != nil || meta.ID == "" || checked != entry.URL || opens != 1 {
				t.Fatalf("allowed capture: meta=%+v err=%v checked=%q source opens=%d", meta, err, checked, opens)
			}
		})
	}
}
