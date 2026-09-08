package artifact

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/readability"
)

// screencastFakeBrowser streams compositor frames and counts every screenshot
// round trip, so a test can prove the video path stopped making them.
type screencastFakeBrowser struct {
	serviceFakeBrowser
	frames      [][]byte
	stopped     atomic.Bool
	started     atomic.Int32
	screenshots atomic.Int32
	failStart   bool
}

func (f *screencastFakeBrowser) ScreencastFrames(ctx context.Context, _ browser.ScreencastOptions) (<-chan browser.ScreencastFrame, func(), error) {
	if f.failStart {
		return nil, nil, context.Canceled
	}
	f.started.Add(1)
	ch := make(chan browser.ScreencastFrame, len(f.frames)+1)
	for _, data := range f.frames {
		ch <- browser.ScreencastFrame{Data: data}
	}
	return ch, func() { f.stopped.Store(true) }, nil
}

func (f *screencastFakeBrowser) CaptureArtifactScreenshot(context.Context, string) (browser.Screenshot, error) {
	f.screenshots.Add(1)
	return f.screenshot, nil
}

func newScreencastFake(t *testing.T, failStart bool) *screencastFakeBrowser {
	t.Helper()
	jpegFrame := oneFrameJPEG(t)
	return &screencastFakeBrowser{
		serviceFakeBrowser: serviceFakeBrowser{
			read:       pageReadFixture(),
			screenshot: browser.Screenshot{MIMEType: "image/png", Data: onePixelPNG(t)},
		},
		frames:    [][]byte{jpegFrame},
		failStart: failStart,
	}
}

// A static page repaints once. The encoder still writes one frame per tick so
// the video keeps its requested duration, but every tick after the first
// reuses the compositor's last frame instead of asking the browser again.
func TestVideoUsesScreencastInsteadOfScreenshotRoundTrips(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("video fixtures are POSIX")
	}
	fake := newScreencastFake(t, false)
	store := newTestStore(t, 4<<20, 8<<20)
	service, err := NewService(store, fake)
	if err != nil {
		t.Fatal(err)
	}
	stubFFmpegAcceptingFrames(t)

	if _, err := service.CaptureArtifact(context.Background(), CaptureOptions{Kind: "video", DurationMS: 400, FPS: 5}); err != nil {
		t.Fatalf("capture: %v", err)
	}
	if got := fake.started.Load(); got != 1 {
		t.Fatalf("screencast started %d times, want 1", got)
	}
	if got := fake.screenshots.Load(); got != 0 {
		t.Fatalf("made %d screenshot round trips; the compositor stream should have supplied every frame", got)
	}
	if !fake.stopped.Load() {
		t.Fatal("screencast was never stopped; Chrome would keep encoding after the capture")
	}
}

// A transport that cannot start a screencast must still produce a video.
func TestVideoFallsBackToScreenshotsWhenScreencastRefuses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("video fixtures are POSIX")
	}
	fake := newScreencastFake(t, true)
	store := newTestStore(t, 4<<20, 8<<20)
	service, err := NewService(store, fake)
	if err != nil {
		t.Fatal(err)
	}
	stubFFmpegAcceptingFrames(t)

	if _, err := service.CaptureArtifact(context.Background(), CaptureOptions{Kind: "video", DurationMS: 400, FPS: 5}); err != nil {
		t.Fatalf("capture: %v", err)
	}
	if got := fake.screenshots.Load(); got == 0 {
		t.Fatal("screencast refused to start, so the screenshot loop should have supplied the frames")
	}
}

func oneFrameJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 75}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func pageReadFixture() readability.PageRead {
	return readability.PageRead{URL: "https://example.test", Title: "Video"}
}

// stubFFmpegAcceptingFrames stands in for the encoder: drain stdin, write a
// non-empty file at the output path (ffmpeg's last argument), succeed.
func stubFFmpegAcceptingFrames(t *testing.T) {
	t.Helper()
	stub := writeExecutableFixture(t, `#!/bin/sh
cat >/dev/null
out=""
for a in "$@"; do out="$a"; done
printf 'webmstub' > "$out"
exit 0
`)
	t.Setenv("BRW_FFMPEG_PATH", stub)
}
