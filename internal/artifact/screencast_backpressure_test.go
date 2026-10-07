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

	const wantSeconds = float64(durationMS) / 1000
	if seconds := probeDuration(t, probe, blob); seconds < wantSeconds-0.25 || seconds > wantSeconds+0.25 {
		t.Fatalf("encoded %.3fs of video for a %.3fs capture; the output must play at real time", seconds, wantSeconds)
	}
}

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

type screenshotOnlyBrowser struct {
	serviceFakeBrowser
	screenshots atomic.Int32
}

func (f *screenshotOnlyBrowser) CaptureArtifactScreenshot(context.Context, string) (browser.Screenshot, error) {
	f.screenshots.Add(1)
	return f.screenshot, nil
}

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
