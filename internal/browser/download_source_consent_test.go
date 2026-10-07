package browser

import (
	"context"
	"errors"
	"testing"
)

func TestDownloadSourceRequiresVerifiableOriginWhenGated(t *testing.T) {
	for _, source := range []string{"", "relative/file", "data:text/plain,fixture", "file:///tmp/fixture", "blob:null/fixture", "https:opaque", "https://", "https://[invalid"} {
		t.Run(source, func(t *testing.T) {
			called := false
			ctx := WithFrameReadCheck(context.Background(), func(string) error { called = true; return nil })
			if err := CheckDownloadSource(ctx, source); err == nil || called {
				t.Fatalf("unknown source check: err=%v hook called=%v", err, called)
			}
			if err := CheckDownloadSource(context.Background(), source); err != nil {
				t.Fatalf("ungated capability changed: %v", err)
			}
		})
	}
}

func TestDownloadSourceChecksFullHTTPAndBlobDestination(t *testing.T) {
	denied := errors.New("read refused")
	for _, source := range []string{"https://fixture.example.test/operator/path", "blob:https://fixture.example.test/operator/path"} {
		checked := ""
		ctx := WithFrameReadCheck(context.Background(), func(url string) error { checked = url; return denied })
		if err := CheckDownloadSource(ctx, source); !errors.Is(err, denied) || checked != "https://fixture.example.test/operator/path" {
			t.Fatalf("source=%q checked=%q err=%v", source, checked, err)
		}
	}
}
