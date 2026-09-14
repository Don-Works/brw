package browser

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// animatedFixture repaints on every frame, which is what makes the ack path
// observable: Chrome holds the next frame until the previous one is acked, so
// the delivered framerate against a continuously repainting page is the
// measurement of whether Page.screencastFrameAck is being sent.
const animatedFixture = `<!doctype html><html><head><title>Screencast fixture</title></head>
<body style="margin:0;background:#101418">
<div id="box" style="width:120px;height:120px;background:#37d67a"></div>
<script>
var box = document.getElementById('box');
var x = 0;
function step(){ x = (x + 7) % 400; box.style.transform = 'translateX(' + x + 'px)'; requestAnimationFrame(step); }
requestAnimationFrame(step);
</script>
</body></html>`

// mostlyStaticFixture repaints about twice a second. It is the shape the cost
// comparison is about: a page an agent is working, not a video. The screenshot
// loop pays a full capture per tick regardless; the compositor sends a frame
// only when something moved.
const mostlyStaticFixture = `<!doctype html><html><head><title>Static fixture</title></head>
<body style="margin:0;background:#101418;color:#e6e8ec;font:16px system-ui">
<p id="tick">0</p>
<script>
var n = 0;
setInterval(function(){ n++; document.getElementById('tick').textContent = String(n); }, 500);
</script>
</body></html>`

func screencastFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/animated", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, animatedFixture)
	})
	mux.HandleFunc("/static", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, mostlyStaticFixture)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// Frames must keep arriving (which only happens if every frame is acked), carry
// Chrome's own swap timestamp in strictly increasing order, and stop when the
// stream does.
//
// "Chrome's own" is asserted rather than assumed: a frame's swap time is when
// the page repainted, so it is always BEHIND the moment brw read the event about
// it. brw's own clock reading never is, which is what tells a real compositor
// timestamp apart from a fallback wearing its name.
func TestScreencastDeliversAckedMonotonicFrames(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := screencastFixtureServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := manager.Open(ctx, srv.URL+"/animated"); err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	frames, stop, stats, err := manager.screencastFrames(ctx, ScreencastOptions{Quality: 60, MaxWidth: 640})
	if err != nil {
		t.Fatalf("start screencast: %v", err)
	}

	// A fixed window, because the ack path shows up as a RATE. Chrome will not
	// send the next frame while one is unacked; it eventually retries anyway, so
	// a stream with no ack still dribbles frames out and only a rate tells the
	// two apart. On this fixture an acked stream runs at several frames a second
	// and an unacked one at well under one.
	const (
		window   = 3 * time.Second
		wantRate = 8
	)
	var collected []ScreencastFrame
	deadline := time.After(window)
collect:
	for {
		select {
		case frame, open := <-frames:
			if !open {
				break collect
			}
			collected = append(collected, frame)
		case <-deadline:
			break collect
		}
	}
	stop()

	if len(collected) < wantRate {
		t.Fatalf("received %d frames in %s; an unacked screencast trickles, so this is the ack round trip failing", len(collected), window)
	}
	previous := time.Time{}
	for i, frame := range collected {
		if !bytes.HasPrefix(frame.Data, []byte{0xff, 0xd8}) {
			t.Errorf("frame %d is not a JPEG (% x)", i, frame.Data[:min(4, len(frame.Data))])
		}
		if frame.Timestamp.IsZero() {
			t.Fatalf("frame %d carries no swap timestamp", i)
		}
		if !previous.IsZero() && !frame.Timestamp.After(previous) {
			t.Fatalf("frame %d swapped at %s, not after %s: the encoder would see time run backwards",
				i, frame.Timestamp, previous)
		}
		previous = frame.Timestamp
		if frame.Width <= 0 || frame.Height <= 0 {
			t.Errorf("frame %d reports a %vx%v viewport; a viewer cannot map a click onto it", i, frame.Width, frame.Height)
		}
	}

	// Stopping releases Chrome's encoder and ends the channel, or a dashboard
	// that disconnects leaves the browser screencasting to nobody.
	select {
	case _, open := <-frames:
		if open {
			// One buffered frame may still be in flight; the next read must close.
			if _, stillOpen := <-frames; stillOpen {
				t.Fatal("the frame channel stayed open after stop")
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the frame channel did not close after stop")
	}

	counts := stats()
	t.Logf("screencast: %d frames, %d bytes, %d dropped, %d out of order, %d unstamped",
		counts.Frames, counts.Bytes, counts.Dropped, counts.OutOfOrder, counts.UnstampedFrames)
	if counts.Frames == 0 {
		t.Error("the stream reported no frames")
	}
	// Every frame headless Chrome delivers carries a swap time, and every one of
	// them swapped before we read it. A frame counted here is one whose stamp brw
	// could not use — including one that came from our own clock instead of the
	// compositor's, which is the shape a deleted swap-timestamp read takes.
	if counts.UnstampedFrames != 0 {
		t.Errorf("%d of %d frames carried no usable swap time; these are timestamps from brw's clock, not Chrome's",
			counts.UnstampedFrames, counts.Frames+counts.UnstampedFrames)
	}
}

// A consumer that stops reading must cost framerate, not the stream. Chrome
// delivers on the CDP event loop, so a blocking send would stall every other
// event on the tab; dropping keeps the stream healthy and the loss countable.
func TestScreencastDropsFramesUnderBackpressureWithoutStalling(t *testing.T) {
	manager := newHeadlessManager(t)
	srv := screencastFixtureServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := manager.Open(ctx, srv.URL+"/animated"); err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	frames, stop, stats, err := manager.screencastFrames(ctx, ScreencastOptions{Quality: 60, MaxWidth: 640})
	if err != nil {
		t.Fatalf("start screencast: %v", err)
	}
	defer stop()

	// Read nothing while the page repaints: the channel fills and Chrome keeps
	// producing, so the surplus has to go somewhere.
	time.Sleep(3 * time.Second)
	if dropped := stats().Dropped; dropped == 0 {
		t.Fatalf("a consumer that read nothing for 3s dropped no frames; the stream is queueing or has stalled (%+v)", stats())
	}

	// The stream is still live: frames keep arriving, still in order.
	previous := time.Time{}
	for i := 0; i < 3; i++ {
		select {
		case frame, open := <-frames:
			if !open {
				t.Fatal("the stream closed under backpressure")
			}
			if !previous.IsZero() && !frame.Timestamp.After(previous) {
				t.Fatalf("frame %d swapped at %s, not after %s: dropping must not reorder the stream", i, frame.Timestamp, previous)
			}
			previous = frame.Timestamp
		case <-time.After(10 * time.Second):
			t.Fatal("the stream stalled after dropping frames")
		}
	}
	counts := stats()
	t.Logf("backpressure: %d frames delivered, %d dropped, %d out of order, %d unstamped",
		counts.Frames, counts.Dropped, counts.OutOfOrder, counts.UnstampedFrames)
	// Dropping frames must not cost the ordering floor. Chrome stamps every frame
	// on this fixture and the CDP stream is ordered, so every frame it sent is
	// behind the previous one's swap and ahead of the floor: a single discard
	// here is the gate measuring the floor against something other than the last
	// swap it forwarded — brw's own clock, say — and once it does that it eats
	// the whole stream.
	if counts.OutOfOrder != 0 {
		t.Errorf("discarded %d of %d frames as out of order; the ordering gate is eating the stream",
			counts.OutOfOrder, counts.Frames+counts.Dropped+counts.OutOfOrder)
	}
}

// The compositor stream has to be measurably cheaper than the screenshot loop it
// replaces, on the page shape brw actually captures: one an agent is working,
// which repaints occasionally rather than continuously. Both numbers are logged
// so a regression is visible as a number rather than as a pass/fail.
//
// Two limits on what these numbers are. The screencast round-trip figure is
// DERIVED from the frame counters — start, stop and one ack per frame event —
// not observed on the wire, and the screenshot side counts calls rather than the
// commands each one issues, so it is a floor. Both biases favour the screenshot
// path, which is the side the assertion has to beat. CPU is not asserted at all:
// processCPU covers this process only, and the compositing the screencast moves
// the work to happens in Chrome.
func TestScreencastCostsLessThanTheScreenshotLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("the cost comparison records two 20s captures")
	}
	manager := newHeadlessManager(t)
	srv := screencastFixtureServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if _, err := manager.Open(ctx, srv.URL+"/static"); err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	// Warm the capture path so neither phase pays for first-call setup.
	if _, err := manager.CaptureArtifactScreenshot(ctx, ""); err != nil {
		t.Fatalf("warm-up capture: %v", err)
	}

	const (
		window = 20 * time.Second
		fps    = 5
	)

	shotCPUStart, cpuAvailable := processCPU()
	shotStarted := time.Now()
	var shotFrames, shotBytes, shotRoundTrips int64
	ticker := time.NewTicker(time.Second / fps)
	for time.Since(shotStarted) < window {
		select {
		case <-ticker.C:
			shotRoundTrips++
			shot, err := manager.CaptureArtifactScreenshot(ctx, "")
			if err != nil {
				ticker.Stop()
				t.Fatalf("screenshot capture: %v", err)
			}
			shotFrames++
			shotBytes += int64(len(shot.Data))
		case <-ctx.Done():
			ticker.Stop()
			t.Fatal(ctx.Err())
		}
	}
	ticker.Stop()
	shotCPUEnd, _ := processCPU()

	castCPUStart, _ := processCPU()
	frames, stop, stats, err := manager.screencastFrames(ctx, ScreencastOptions{Quality: videoScreencastQualityForTest})
	if err != nil {
		t.Fatalf("start screencast: %v", err)
	}
	castStarted := time.Now()
	drain := time.NewTicker(time.Second / fps)
	for time.Since(castStarted) < window {
		select {
		case _, open := <-frames:
			if !open {
				t.Fatal("the screencast ended early")
			}
		case <-drain.C:
			// The encoder wakes on its own cadence and reuses the last frame
			// when nothing repainted; nothing is fetched on this tick.
		case <-ctx.Done():
			drain.Stop()
			t.Fatal(ctx.Err())
		}
	}
	drain.Stop()
	stop()
	castCPUEnd, _ := processCPU()
	counts := stats()

	// Derived, not observed: start, stop, and one ack per frame event Chrome
	// delivered, whether it was forwarded, dropped or discarded.
	castRoundTrips := counts.Frames + counts.Dropped + counts.OutOfOrder + counts.UnstampedFrames + 2

	t.Logf("screenshot loop over %s: %d frames, %d capture calls, %d bytes",
		window, shotFrames, shotRoundTrips, shotBytes)
	t.Logf("native screencast over %s: %d frames, %d CDP round trips (derived from the frame counters), %d bytes (%d dropped)",
		window, counts.Frames, castRoundTrips, counts.Bytes, counts.Dropped)
	if cpuAvailable {
		t.Logf("daemon CPU: screenshot loop %v, screencast %v — this process only, so the Chrome compositing the screencast moves the work to is not in either number",
			shotCPUEnd-shotCPUStart, castCPUEnd-castCPUStart)
	}

	if counts.Bytes >= shotBytes {
		t.Errorf("screencast transferred %d bytes, screenshot loop %d: the compositor stream must move less over CDP",
			counts.Bytes, shotBytes)
	}
	if castRoundTrips >= shotRoundTrips {
		t.Errorf("screencast made %d CDP round trips, screenshot loop %d", castRoundTrips, shotRoundTrips)
	}
}

// videoScreencastQualityForTest mirrors the quality the artifact encoder asks
// for, so the comparison measures the transport rather than a different setting.
const videoScreencastQualityForTest = 75
