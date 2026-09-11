package artifact

import (
	"context"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// formatStallSeconds renders a duration for POSIX sleep, which takes seconds.
func formatStallSeconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 2, 64)
}

// The recording budget used to own the encoder too, so a capture that had
// recorded every frame was thrown away whenever the child process took longer
// to finish than the recording took to run. On a loaded machine that is
// routine: the child-process wait alone measured 2.2s against the 2.4s total
// budget a 400ms capture was given, while the frame loop itself finished in the
// 201ms it was paced for.
func TestSlowEncoderOutlivesTheRecordingBudget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake ffmpeg fixture is a POSIX shell script")
	}
	recording := videoCaptureBudget(400)
	// Finishes well after the recording budget, well inside the encode budget.
	stall := recording + time.Second
	if stall >= videoEncodeBudget(2) {
		t.Fatalf("fixture stall %s must sit inside the encode budget %s", stall, videoEncodeBudget(2))
	}

	stub := writeExecutableFixture(t, `#!/bin/sh
cat >/dev/null
out=""
for a in "$@"; do out="$a"; done
sleep `+formatStallSeconds(stall)+`
printf 'webmstub' > "$out"
exit 0
`)
	t.Setenv("BRW_FFMPEG_PATH", stub)

	fake := newScreencastFake(t, false)
	store := newTestStore(t, 4<<20, 8<<20)
	service, err := NewService(store, fake)
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	meta, err := service.CaptureArtifact(context.Background(), CaptureOptions{Kind: "video", DurationMS: 400, FPS: 5})
	if err != nil {
		t.Fatalf("capture with a slow encoder: %v", err)
	}
	if elapsed := time.Since(started); elapsed <= recording {
		t.Fatalf("capture finished in %s, inside the %s recording budget: the fixture did not exercise the split", elapsed, recording)
	}
	if meta.SizeBytes == 0 {
		t.Error("captured video is empty")
	}
	assertNoVideoTemps(t, store.Root())
}

// The encode allowance scales with the frames it has to encode, not with how
// long the caller asked to record: 30s at 1fps is 30 frames, 10s at 30fps is
// 300.
func TestVideoEncodeBudgetScalesWithFrames(t *testing.T) {
	few := videoEncodeBudget(30)
	many := videoEncodeBudget(300)
	if many <= few {
		t.Fatalf("300 frames budgeted %s, not more than the %s for 30", many, few)
	}
	if got := videoEncodeBudget(0); got != videoEncodeBudget(1) {
		t.Errorf("zero frames budgeted %s, want the single-frame budget %s", got, videoEncodeBudget(1))
	}
	if videoEncodeBudget(1) <= videoEncodeBaseBudget {
		t.Error("a one-frame encode is not given more than the bare base budget")
	}
}

// The recording budget must stay independent of the encoder's: it bounds the
// paced frame loop and the browser round trips that feed it, and widening the
// encode allowance must not let a stalled browser hold the page for longer.
func TestRecordingBudgetIsUnchangedByTheEncodeAllowance(t *testing.T) {
	for _, durationMS := range []int{100, 400, 5_000, 30_000} {
		duration := time.Duration(durationMS) * time.Millisecond
		budget := videoCaptureBudget(durationMS)
		headroom := budget - duration
		if headroom < videoCaptureMinHeadroom || headroom > videoCaptureMaxHeadroom {
			t.Errorf("duration %s got recording headroom %s, want within [%s, %s]",
				duration, headroom, videoCaptureMinHeadroom, videoCaptureMaxHeadroom)
		}
	}
}
