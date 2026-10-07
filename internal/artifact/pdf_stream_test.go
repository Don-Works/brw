package artifact

import (
	"context"
	"io"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

const capturedPDFBytes = 50 << 20

const streamedPDFPeakBound = 8 << 20

type syntheticPDF struct {
	remaining int
	closed    atomic.Bool
	filler    []byte
}

func newSyntheticPDF(size int) *syntheticPDF {
	filler := make([]byte, 4096)
	for index := range filler {
		filler[index] = byte('A' + index%26)
	}
	copy(filler, []byte("%PDF-1.7\n"))
	return &syntheticPDF{remaining: size, filler: filler}
}

func (p *syntheticPDF) Read(dst []byte) (int, error) {
	if p.remaining <= 0 {
		return 0, io.EOF
	}
	n := min(len(dst), p.remaining, len(p.filler))
	copy(dst, p.filler[:n])
	p.remaining -= n
	return n, nil
}

func (p *syntheticPDF) Close() error {
	p.closed.Store(true)
	return nil
}

type streamingPDFBrowser struct {
	browser.Controller
	size   int
	stream *syntheticPDF
}

func (b *streamingPDFBrowser) CapturePDFStream(context.Context) (io.ReadCloser, error) {
	b.stream = newSyntheticPDF(b.size)
	return b.stream, nil
}

func (b *streamingPDFBrowser) Evaluate(context.Context, string) (any, error) {
	return map[string]any{"url": "https://statements.example.test/", "title": "statement"}, nil
}

type bufferedPDFBrowser struct {
	browser.Controller
	size int
}

func (b *bufferedPDFBrowser) CapturePDF(context.Context) ([]byte, error) {
	data := make([]byte, b.size)
	copy(data, []byte("%PDF-1.7\n"))
	return data, nil
}

func (b *bufferedPDFBrowser) Evaluate(context.Context, string) (any, error) {
	return map[string]any{"url": "https://statements.example.test/", "title": "statement"}, nil
}

func heapPeak(fn func()) uint64 {
	runtime.GC()
	var start runtime.MemStats
	runtime.ReadMemStats(&start)
	var peak atomic.Uint64
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		var sample runtime.MemStats
		for {
			select {
			case <-done:
				return
			default:
			}
			runtime.ReadMemStats(&sample)
			if sample.HeapAlloc > peak.Load() {
				peak.Store(sample.HeapAlloc)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	fn()
	close(done)
	<-finished
	if peak.Load() < start.HeapAlloc {
		return 0
	}
	return peak.Load() - start.HeapAlloc
}

func TestPDFCaptureStreamsWithoutMaterialisingThePayload(t *testing.T) {
	streamStore := newTestStore(t, 64<<20, 256<<20)
	streamBrowser := &streamingPDFBrowser{size: capturedPDFBytes}
	streamService, err := NewService(streamStore, streamBrowser)
	if err != nil {
		t.Fatal(err)
	}

	var streamMeta Meta
	var streamErr error
	streamedPeak := heapPeak(func() {
		streamMeta, streamErr = streamService.CaptureArtifact(context.Background(), CaptureOptions{Kind: "pdf"})
	})
	if streamErr != nil {
		t.Fatalf("streamed PDF capture: %v", streamErr)
	}
	if streamMeta.SizeBytes != capturedPDFBytes {
		t.Fatalf("stored %d bytes, want %d", streamMeta.SizeBytes, capturedPDFBytes)
	}
	if streamBrowser.stream == nil || !streamBrowser.stream.closed.Load() {
		t.Fatal("the capture did not close the browser-side PDF stream")
	}

	bufferedStore := newTestStore(t, 64<<20, 256<<20)
	bufferedService, err := NewService(bufferedStore, &bufferedPDFBrowser{size: capturedPDFBytes})
	if err != nil {
		t.Fatal(err)
	}
	var bufferedErr error
	bufferedPeak := heapPeak(func() {
		_, bufferedErr = bufferedService.CaptureArtifact(context.Background(), CaptureOptions{Kind: "pdf"})
	})
	if bufferedErr != nil {
		t.Fatalf("buffered PDF capture: %v", bufferedErr)
	}

	t.Logf("payload=%d bytes streamed peak=%d bytes buffered peak=%d bytes",
		capturedPDFBytes, streamedPeak, bufferedPeak)
	if streamedPeak > streamedPDFPeakBound {
		t.Fatalf("streamed capture peaked at %d bytes, above the %d-byte bound for a %d-byte payload",
			streamedPeak, streamedPDFPeakBound, capturedPDFBytes)
	}
	if streamedPeak*2 > bufferedPeak {
		t.Fatalf("streamed peak %d is not meaningfully below the buffered peak %d; the capture is not streaming",
			streamedPeak, bufferedPeak)
	}

	window, _, more, err := streamStore.Read(streamMeta.ID, capturedPDFBytes-8, 8)
	if err != nil || more || len(window) != 8 {
		t.Fatalf("tail read window=%d more=%v err=%v", len(window), more, err)
	}
}

func TestPDFCaptureFallsBackWhenTheTransportHasNoStream(t *testing.T) {
	store := newTestStore(t, 4<<20, 8<<20)
	service, err := NewService(store, &bufferedPDFBrowser{size: 2048})
	if err != nil {
		t.Fatal(err)
	}
	meta, err := service.CaptureArtifact(context.Background(), CaptureOptions{Kind: "pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if meta.SizeBytes != 2048 || meta.MIMEType != "application/pdf" {
		t.Fatalf("meta = %+v", meta)
	}

	plainStore := newTestStore(t, 4<<20, 8<<20)
	plain, err := NewService(plainStore, serviceFakeBrowser{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.CaptureArtifact(context.Background(), CaptureOptions{Kind: "pdf"}); err == nil ||
		err.Error() != "PDF capture is unavailable on this browser transport" {
		t.Fatalf("error = %v, want the named capability failure", err)
	}
}
