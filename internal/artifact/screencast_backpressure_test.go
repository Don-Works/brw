package artifact

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

// starvedScreencastBrowser delivers fewer compositor frames than the encoder
// asks for and then has nothing left: a quiet page, or a compositor that has
// stopped repainting. It is NOT the backpressure drop — that happens inside the
// stream, where a full consumer channel discards the surplus, and is covered
// against real Chrome in internal/browser. Downstream the two look alike, which
// is why this fixture is named for what it actually produces.
type starvedScreencastBrowser struct {
	serviceFakeBrowser
	delivered   int
	screenshots atomic.Int32
	stopped     atomic.Bool
}

func (f *starvedScreencastBrowser) ScreencastFrames(context.Context, browser.ScreencastOptions) (<-chan browser.ScreencastFrame, func(), error) {
	ch := make(chan browser.ScreencastFrame, f.delivered+1)
	for i := 0; i < f.delivered; i++ {
		ch <- browser.ScreencastFrame{Data: distinctFrameJPEG(i), Timestamp: time.Now().Add(time.Duration(i) * time.Millisecond)}
	}
	return ch, func() { f.stopped.Store(true) }, nil
}

// distinctFrameJPEG renders a frame whose bytes differ from its neighbours, so a
// decoder cannot pass a video that silently collapsed to one repeated image.
func distinctFrameJPEG(seed int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	fill := color.RGBA{R: uint8(40 + seed*60), G: 0x20, B: 0x40, A: 0xff}
	draw.Draw(img, img.Bounds(), &image.Uniform{C: fill}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 75}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func (f *starvedScreencastBrowser) CaptureArtifactScreenshot(context.Context, string) (browser.Screenshot, error) {
	f.screenshots.Add(1)
	return f.screenshot, nil
}

// A capture that receives fewer frames than it encodes still produces the
// requested frame count and a file a decoder accepts. The missing frames cost
// smoothness — the last one is held — and nothing else; a stream that truncated
// the encode instead would leave a short or unreadable webm.
//
// Regression coverage for captureVideo's drain loop, which predates the native
// screencast: it passes with or without that work. What it does not cover is a
// backpressure drop reaching the encoded file; nothing connects the two.
func TestAStarvedCompositorDegradesFramerateWithoutCorruptingTheFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("video fixtures are POSIX")
	}
	if _, err := resolveFFmpegPath(); err != nil {
		t.Skip("ffmpeg not installed")
	}
	probe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed; the output cannot be verified as decodable")
	}

	const (
		durationMS = 2000
		fps        = 10
		wantFrames = durationMS * fps / 1000
	)
	fake := &starvedScreencastBrowser{
		serviceFakeBrowser: serviceFakeBrowser{
			read:       pageReadFixture(),
			screenshot: browser.Screenshot{MIMEType: "image/png", Data: onePixelPNG(t)},
		},
		// Three frames for twenty ticks: seventeen ticks reuse the last one.
		delivered: 3,
	}
	service, err := NewService(newTestStore(t, 8<<20, 16<<20), fake)
	if err != nil {
		t.Fatal(err)
	}

	meta, err := service.CaptureArtifact(context.Background(), CaptureOptions{
		Kind: "video", DurationMS: durationMS, FPS: fps,
	})
	if err != nil {
		t.Fatalf("capture from a starved compositor: %v", err)
	}
	if meta.MIMEType != "video/webm" || meta.SizeBytes == 0 {
		t.Fatalf("meta = %+v, want a non-empty webm", meta)
	}
	if got := fake.screenshots.Load(); got != 0 {
		t.Errorf("made %d screenshot round trips; a quiet compositor is not a reason to fall back", got)
	}
	if !fake.stopped.Load() {
		t.Error("the screencast was never stopped")
	}

	blob := filepath.Join(service.Store().Root(), meta.ID+".blob")
	frames := probeFrameCount(t, probe, blob)
	if frames != wantFrames {
		t.Fatalf("encoded %d frames, want %d: a missing frame must cost smoothness, not length", frames, wantFrames)
	}
	// Real time, not compressed time: 20 frames at 10fps is two seconds of
	// playback whether the compositor sent 20 frames or 3.
	const wantSeconds = float64(durationMS) / 1000
	if seconds := probeDuration(t, probe, blob); seconds < wantSeconds-0.25 || seconds > wantSeconds+0.25 {
		t.Fatalf("encoded %.3fs of video for a %.3fs capture; the output must play at real time", seconds, wantSeconds)
	}
}

// The extension bridge drives a tab through chrome.debugger and has no
// compositor stream at all, so its controller does not implement
// ScreencastFrames. That transport, and the locked-session print-renderer case
// README.md documents, keep the screenshot loop — and it still has to produce a
// file a decoder accepts, not merely an error-free return.
//
// Regression coverage for the pre-existing fallback path: it passes whether or
// not the compositor lane exists at all, which is the point of a fallback.
func TestVideoFallbackProducesADecodableFileWithoutAScreencast(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("video fixtures are POSIX")
	}
	if _, err := resolveFFmpegPath(); err != nil {
		t.Skip("ffmpeg not installed")
	}
	probe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed; the output cannot be verified as decodable")
	}

	fake := &screenshotOnlyBrowser{serviceFakeBrowser: serviceFakeBrowser{
		read:       pageReadFixture(),
		screenshot: browser.Screenshot{MIMEType: "image/png", Data: onePixelPNG(t)},
	}}
	// Compile-time proof that this transport has no compositor stream: the
	// capture must reach the fallback because the capability is absent, not
	// because a stub returned an error.
	if _, isCaster := any(fake).(videoScreencaster); isCaster {
		t.Fatal("the fallback fixture implements ScreencastFrames; it cannot stand in for a bridge transport")
	}

	service, err := NewService(newTestStore(t, 8<<20, 16<<20), fake)
	if err != nil {
		t.Fatal(err)
	}
	const (
		durationMS = 1000
		fps        = 5
		wantFrames = durationMS * fps / 1000
	)
	meta, err := service.CaptureArtifact(context.Background(), CaptureOptions{
		Kind: "video", DurationMS: durationMS, FPS: fps,
	})
	if err != nil {
		t.Fatalf("fallback capture: %v", err)
	}
	if meta.MIMEType != "video/webm" || meta.SizeBytes == 0 {
		t.Fatalf("meta = %+v, want a non-empty webm", meta)
	}
	if got := fake.screenshots.Load(); int(got) < wantFrames {
		t.Errorf("made %d screenshot round trips for %d frames; the fallback is what feeds this encode", got, wantFrames)
	}
	blob := filepath.Join(service.Store().Root(), meta.ID+".blob")
	if frames := probeFrameCount(t, probe, blob); frames != wantFrames {
		t.Fatalf("fallback encoded %d frames, want %d", frames, wantFrames)
	}
}

// screenshotOnlyBrowser is the bridge shape: screenshots, no screencast.
type screenshotOnlyBrowser struct {
	serviceFakeBrowser
	screenshots atomic.Int32
}

func (f *screenshotOnlyBrowser) CaptureArtifactScreenshot(context.Context, string) (browser.Screenshot, error) {
	f.screenshots.Add(1)
	return f.screenshot, nil
}

// probeFrameCount decodes the file and counts what came out. A truncated or
// corrupt container fails here rather than merely being short.
func probeFrameCount(t *testing.T, probe, path string) int {
	t.Helper()
	out, err := exec.Command(probe,
		"-v", "error",
		"-select_streams", "v:0",
		"-count_frames",
		"-show_entries", "stream=nb_read_frames",
		"-of", "default=nokey=1:noprint_wrappers=1",
		path,
	).Output()
	if err != nil {
		t.Fatalf("ffprobe rejected the encoded video: %v", err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("ffprobe frame count %q: %v", out, err)
	}
	return count
}

// probeDuration reports how long the encoded video plays.
func probeDuration(t *testing.T, probe, path string) float64 {
	t.Helper()
	out, err := exec.Command(probe,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=nokey=1:noprint_wrappers=1",
		path,
	).Output()
	if err != nil {
		t.Fatalf("ffprobe could not read the duration: %v", err)
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("ffprobe duration %q: %v", out, err)
	}
	return seconds
}
