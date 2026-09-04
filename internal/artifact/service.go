package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// API is implemented locally by Service and remotely by httpclient.Controller.
// This optional interface keeps the main browser.Controller stable while
// ensuring upstream MCP processes create and read artifacts on the browser host.
type API interface {
	CaptureArtifact(context.Context, CaptureOptions) (Meta, error)
	ArtifactInfo(context.Context, string) (Meta, error)
	ReadArtifact(context.Context, string, int64, int) (Chunk, error)
	SearchArtifact(context.Context, string, string, int) ([]TextHit, error)
	DeleteArtifact(context.Context, string) error
}

type CaptureOptions struct {
	Kind         string        `json:"kind"`
	Ref          string        `json:"ref,omitempty"`
	DurationMS   int           `json:"duration_ms,omitempty"`
	FPS          int           `json:"fps,omitempty"`
	DownloadGUID string        `json:"download_guid,omitempty"`
	Filename     string        `json:"filename,omitempty"`
	Redaction    string        `json:"redaction,omitempty"`
	TTL          time.Duration `json:"-"`
	TTLSeconds   int           `json:"ttl_seconds,omitempty"`
}

type pdfCapturer interface {
	CapturePDF(context.Context) ([]byte, error)
}

type rawScreenshotCapturer interface {
	CaptureArtifactScreenshot(context.Context, string) (browser.Screenshot, error)
}

type managedDownloadCleaner interface {
	CleanupManagedDownload(browser.DownloadEntry) (bool, error)
}

type Service struct {
	store   *Store
	browser browser.Controller

	// Extension-backed downloads can live in OS-protected user folders. On
	// macOS, opening such a path from a background LaunchAgent can block inside
	// TCC while it waits for a permission prompt; an os.Open goroutine cannot be
	// cancelled while it is in that syscall. Serialize source opens so retries
	// fail immediately instead of pinning another goroutine/OS thread each time.
	downloadOpenGate          chan struct{}
	downloadSourceOpener      func(string) (*os.File, error)
	downloadSourceOpenTimeout time.Duration
}

type recipeCaptureContinuity struct {
	provider browser.DocumentIdentityProvider
	allowed  []string
	start    browser.DocumentIdentity
}

const (
	// Video capture is paced for the requested duration. Leave a small amount of
	// bounded time for browser/encoder startup, the final mux, and persistence,
	// without allowing a stuck browser call or child process to inherit an
	// unbounded caller context.
	videoCaptureMinHeadroom = 2 * time.Second
	videoCaptureMaxHeadroom = 5 * time.Second
	videoProcessWaitDelay   = time.Second
	videoStderrLimit        = 4 << 10
	videoErrorStderrLimit   = 1000
	videoContinuityInterval = 500 * time.Millisecond

	// Opening a completed local browser download should be effectively instant.
	// A short bound surfaces OS privacy/file-provider stalls while holding the
	// single-flight gate through the subsequent capture prevents retry fan-out.
	defaultDownloadSourceOpenTimeout = 3 * time.Second
)

func NewService(store *Store, controller browser.Controller) (*Service, error) {
	if store == nil {
		return nil, errors.New("artifact store is required")
	}
	if controller == nil {
		return nil, errors.New("browser controller is required")
	}
	return &Service{
		store:                     store,
		browser:                   controller,
		downloadOpenGate:          make(chan struct{}, 1),
		downloadSourceOpener:      os.Open,
		downloadSourceOpenTimeout: defaultDownloadSourceOpenTimeout,
	}, nil
}

func (s *Service) Store() *Store { return s.store }

func (s *Service) CaptureArtifact(ctx context.Context, opts CaptureOptions) (Meta, error) {
	put, err := s.putOptions(opts)
	if err != nil {
		return Meta{}, err
	}
	continuity, err := s.beginRecipeCapture(ctx)
	if err != nil {
		return Meta{}, err
	}
	meta, err := s.captureArtifact(ctx, opts, put, continuity)
	if err != nil {
		return Meta{}, err
	}
	if err := continuity.verify(ctx); err != nil {
		deleteErr := s.store.Delete(meta.ID)
		return Meta{}, errors.Join(err, deleteErr)
	}
	return meta, nil
}

func (s *Service) captureArtifact(ctx context.Context, opts CaptureOptions, put PutOptions, continuity *recipeCaptureContinuity) (Meta, error) {
	switch opts.Kind {
	case "text":
		read, err := s.browser.Read(ctx)
		if err != nil {
			return Meta{}, err
		}
		if err := guardCapturedPageURL(ctx, read.URL); err != nil {
			return Meta{}, err
		}
		put.MIMEType = "text/plain; charset=utf-8"
		put.SourceHash = sourceHash(read.URL, read.Title)
		return s.store.PutContext(ctx, put, strings.NewReader(read.Main))
	case "semantic_json":
		snap, err := s.browser.Snapshot(ctx, snapshot.SnapshotOptions{Mode: "all", ViewportOnly: false})
		if err != nil {
			return Meta{}, err
		}
		if err := guardCapturedPageURL(ctx, snap.URL); err != nil {
			return Meta{}, err
		}
		data, err := json.MarshalIndent(snap, "", "  ")
		if err != nil {
			return Meta{}, err
		}
		put.MIMEType = "application/json"
		put.SourceHash = sourceHash(snap.URL, snap.Title)
		return s.store.PutContext(ctx, put, bytes.NewReader(data))
	case "screenshot":
		var (
			shot browser.Screenshot
			err  error
		)
		if capture, ok := s.browser.(rawScreenshotCapturer); ok {
			shot, err = capture.CaptureArtifactScreenshot(ctx, opts.Ref)
		} else if strings.TrimSpace(opts.Ref) == "" {
			shot, err = s.browser.Screenshot(ctx)
		} else {
			shot, err = s.browser.ScreenshotElement(ctx, opts.Ref)
		}
		if err != nil {
			return Meta{}, err
		}
		data, err := screenshotData(shot)
		if err != nil {
			return Meta{}, err
		}
		put.MIMEType = shot.MIMEType
		put.SourceHash = s.currentSourceHash(ctx)
		return s.store.PutContext(ctx, put, bytes.NewReader(data))
	case "pdf":
		capture, ok := s.browser.(pdfCapturer)
		if !ok {
			return Meta{}, errors.New("PDF capture is unavailable on this browser transport")
		}
		data, err := capture.CapturePDF(ctx)
		if err != nil {
			return Meta{}, err
		}
		put.MIMEType = "application/pdf"
		put.SourceHash = s.currentSourceHash(ctx)
		return s.store.PutContext(ctx, put, bytes.NewReader(data))
	case "download":
		return s.captureDownload(ctx, opts, put)
	case "video":
		return s.captureVideo(ctx, opts, put, continuity)
	default:
		return Meta{}, fmt.Errorf("unsupported capture kind %q", opts.Kind)
	}
}

func (s *Service) putOptions(opts CaptureOptions) (PutOptions, error) {
	if err := validateCaptureOptions(opts); err != nil {
		return PutOptions{}, err
	}
	if opts.TTL == 0 && opts.TTLSeconds != 0 {
		const maxDurationSeconds = int64(^uint64(0)>>1) / int64(time.Second)
		if opts.TTLSeconds < 1 || int64(opts.TTLSeconds) > maxDurationSeconds {
			return PutOptions{}, errors.New("ttl_seconds is outside the supported duration range")
		}
		opts.TTL = time.Duration(opts.TTLSeconds) * time.Second
	}
	return PutOptions{Kind: opts.Kind, Redaction: opts.Redaction, TTL: opts.TTL}, nil
}

func (s *Service) ArtifactInfo(_ context.Context, id string) (Meta, error) {
	return s.store.Info(id)
}

func (s *Service) ReadArtifact(_ context.Context, id string, offset int64, maxBytes int) (Chunk, error) {
	if maxBytes == 0 {
		maxBytes = 64 << 10
	}
	data, meta, more, err := s.store.Read(id, offset, maxBytes)
	if err != nil {
		return Chunk{}, err
	}
	chunk := Chunk{
		ArtifactID: id, Offset: offset, SizeBytes: len(data), TotalBytes: meta.SizeBytes,
		More: more, Encoding: "base64",
	}
	if more {
		chunk.NextOffset = offset + int64(len(data))
	}
	if (strings.HasPrefix(meta.MIMEType, "text/") || meta.MIMEType == "application/json") && utf8.Valid(data) {
		chunk.Encoding = "utf-8"
		chunk.Text = string(data)
	} else {
		// A byte window can bisect a multi-byte rune even when the complete text
		// artifact is valid UTF-8. Return exact base64 bytes for that window rather
		// than letting JSON silently replace them with U+FFFD.
		chunk.Base64 = base64.StdEncoding.EncodeToString(data)
	}
	return chunk, nil
}

func (s *Service) SearchArtifact(ctx context.Context, id, query string, limit int) ([]TextHit, error) {
	if limit == 0 {
		limit = 20
	}
	return s.store.SearchTextContext(ctx, id, query, limit)
}

func (s *Service) DeleteArtifact(_ context.Context, id string) error {
	return s.store.Delete(id)
}

func (s *Service) captureDownload(ctx context.Context, opts CaptureOptions, put PutOptions) (Meta, error) {
	if opts.DownloadGUID == "" && opts.Filename == "" {
		return Meta{}, errors.New("download capture requires download_guid or filename")
	}
	result, err := s.browser.Downloads(ctx)
	if err != nil {
		return Meta{}, err
	}
	if !result.Supported {
		if note := strings.TrimSpace(result.Note); note != "" {
			return Meta{}, errors.New(note)
		}
		return Meta{}, errors.New("download capture is unavailable on this browser transport")
	}
	tabID := browser.TabIDFromContext(ctx)
	_, recipeScoped := browser.AllowedOriginsFromContext(ctx)
	matches := make([]browser.DownloadEntry, 0, 1)
	matchingIncomplete := false
	for _, item := range result.Downloads {
		if opts.DownloadGUID != "" && item.GUID != opts.DownloadGUID {
			continue
		}
		if opts.DownloadGUID == "" && item.SuggestedFilename != opts.Filename {
			continue
		}
		// Direct CDP records the initiating target. Unknown provenance remains
		// compatible with transports that cannot report it, while a known mismatch
		// is never allowed to capture another tab's staged file.
		if tabID != "" && (item.TabID != "" && item.TabID != tabID || recipeScoped && item.TabID == "") {
			continue
		}
		if item.State != "completed" || strings.TrimSpace(item.Path) == "" {
			matchingIncomplete = true
			continue
		}
		matches = append(matches, item)
	}
	if len(matches) > 1 {
		return Meta{}, errors.New("multiple completed downloads match; use a unique download_guid")
	}
	if len(matches) == 1 {
		return s.captureDownloadEntry(ctx, matches[0], put)
	}
	if matchingIncomplete {
		return Meta{}, errors.New("matching download is not complete or has no browser-host path")
	}
	return Meta{}, errors.New("matching completed download was not found")
}

// CaptureCompletedDownload preserves the causality between a recipe's
// pre-armed download postcondition and its following capture step. BrowserSurface
// hands the exact completed entry it observed to this browser-host-only method
// instead of reselecting from a registry that may contain same-name downloads.
func (s *Service) CaptureCompletedDownload(ctx context.Context, item browser.DownloadEntry, opts CaptureOptions) (Meta, error) {
	put, err := s.putOptions(opts)
	if err != nil {
		return Meta{}, err
	}
	if opts.Kind != "download" || item.State != "completed" || strings.TrimSpace(item.Path) == "" {
		return Meta{}, errors.New("completed download entry is invalid")
	}
	if opts.DownloadGUID != "" && item.GUID != opts.DownloadGUID || opts.Filename != "" && item.SuggestedFilename != opts.Filename {
		return Meta{}, errors.New("completed download does not match the capture selector")
	}
	if tabID := browser.TabIDFromContext(ctx); tabID != "" {
		_, recipeScoped := browser.AllowedOriginsFromContext(ctx)
		if item.TabID != "" && item.TabID != tabID || recipeScoped && item.TabID == "" {
			return Meta{}, errors.New("completed download lacks matching source-tab provenance")
		}
	}
	continuity, err := s.beginRecipeCapture(ctx)
	if err != nil {
		return Meta{}, err
	}
	meta, err := s.captureDownloadEntry(ctx, item, put)
	if err != nil {
		return Meta{}, err
	}
	if err := continuity.verify(ctx); err != nil {
		deleteErr := s.store.Delete(meta.ID)
		return Meta{}, errors.Join(err, deleteErr)
	}
	return meta, nil
}

func (s *Service) captureDownloadEntry(ctx context.Context, item browser.DownloadEntry, put PutOptions) (Meta, error) {
	info, err := os.Lstat(item.Path)
	if err != nil {
		return Meta{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Meta{}, errors.New("download path is not a regular file")
	}
	file, releaseOpen, err := s.openDownloadSource(ctx, item.Path)
	if err != nil {
		return Meta{}, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		releaseOpen()
	}()
	openedInfo, err := file.Stat()
	if err != nil {
		return Meta{}, err
	}
	// The browser supplies a host path, but another local process could replace
	// that path between Lstat and Open. Persist only the exact regular file we
	// inspected; reads from the open descriptor remain stable after this check.
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return Meta{}, errors.New("download path changed before capture")
	}
	prefix := make([]byte, 512)
	n, err := file.Read(prefix)
	if err != nil && !errors.Is(err, io.EOF) {
		return Meta{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Meta{}, err
	}
	put.MIMEType = http.DetectContentType(prefix[:n])
	put.SourceHash = sourceHash(item.URL, item.SuggestedFilename)
	meta, putErr := s.store.PutContext(ctx, put, file)
	closeErr := file.Close()
	closed = true
	if putErr != nil {
		return Meta{}, putErr
	}
	if closeErr != nil {
		deleteErr := s.store.Delete(meta.ID)
		return Meta{}, errors.Join(fmt.Errorf("close captured download: %w", closeErr), deleteErr)
	}
	cleaner, ok := s.browser.(managedDownloadCleaner)
	if !ok {
		// A transport without an explicit managed-staging capability owns no
		// source files; preserve the browser/user original.
		return meta, nil
	}
	_, cleanupErr := cleaner.CleanupManagedDownload(item)
	if cleanupErr != nil {
		deleteErr := s.store.Delete(meta.ID)
		return Meta{}, errors.Join(fmt.Errorf("remove managed download after artifact persistence: %w", cleanupErr), deleteErr)
	}
	return meta, nil
}

type downloadOpenResult struct {
	file *os.File
	err  error
}

// openDownloadSource isolates the potentially uninterruptible os.Open syscall
// in one bounded worker. Go cannot forcibly cancel a syscall already blocked in
// the kernel, but the request can return and the occupied gate makes every retry
// fail fast. A successful caller holds the gate through artifact persistence;
// once a timed-out worker unblocks it closes the unused file and releases the
// gate, allowing captures to recover without restarting the daemon.
func (s *Service) openDownloadSource(ctx context.Context, sourcePath string) (*os.File, func(), error) {
	if ctx == nil {
		return nil, nil, errors.New("download capture context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	select {
	case s.downloadOpenGate <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	default:
		return nil, nil, errors.New("download source access is already in progress or blocked; refusing another capture to avoid exhausting OS threads")
	}

	openTimeout := s.downloadSourceOpenTimeout
	if openTimeout <= 0 {
		openTimeout = defaultDownloadSourceOpenTimeout
	}
	openCtx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	result := make(chan downloadOpenResult)
	go func() {
		opener := s.downloadSourceOpener
		if opener == nil {
			opener = os.Open
		}
		file, err := opener(sourcePath)
		if err == nil && file == nil {
			err = errors.New("download source opener returned no file")
		}
		if err != nil {
			if file != nil {
				_ = file.Close()
				file = nil
			}
			<-s.downloadOpenGate
		}
		select {
		case result <- downloadOpenResult{file: file, err: err}:
			// A successful handoff transfers the gate to the request. An error
			// released it above before publishing the result.
		case <-openCtx.Done():
			if file != nil {
				_ = file.Close()
			}
			if err == nil {
				<-s.downloadOpenGate
			}
		}
	}()

	select {
	case completed := <-result:
		if err := openCtx.Err(); err != nil {
			if completed.file != nil {
				_ = completed.file.Close()
				<-s.downloadOpenGate
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, nil, ctxErr
			}
			return nil, nil, fmt.Errorf("download source open did not complete within %s; OS privacy or file-provider access may be awaiting approval", openTimeout)
		}
		if completed.err != nil {
			return nil, nil, fmt.Errorf("open download source: %w", completed.err)
		}
		return completed.file, func() { <-s.downloadOpenGate }, nil
	case <-openCtx.Done():
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		return nil, nil, fmt.Errorf("download source open did not complete within %s; OS privacy or file-provider access may be awaiting approval", openTimeout)
	}
}

func (s *Service) captureVideo(ctx context.Context, opts CaptureOptions, put PutOptions, continuity *recipeCaptureContinuity) (Meta, error) {
	if opts.DurationMS < 100 || opts.DurationMS > 30_000 || opts.FPS < 1 || opts.FPS > 30 {
		return Meta{}, errors.New("video capture requires duration_ms 100..30000 and fps 1..30")
	}
	frames := max(1, (opts.DurationMS*opts.FPS+999)/1000)
	if frames > 300 {
		return Meta{}, errors.New("video capture is limited to 300 frames; reduce duration_ms or fps")
	}
	videoCtx, cancel := context.WithTimeout(ctx, videoCaptureBudget(opts.DurationMS))
	defer cancel()
	ffmpeg, err := resolveFFmpegPath()
	if err != nil {
		return Meta{}, err
	}
	outputFile, err := os.CreateTemp(s.store.Root(), ".video-*.webm")
	if err != nil {
		return Meta{}, err
	}
	output := outputFile.Name()
	defer os.Remove(output)
	if err := outputFile.Chmod(0o600); err != nil {
		_ = outputFile.Close()
		return Meta{}, err
	}
	if err := outputFile.Close(); err != nil {
		return Meta{}, err
	}

	command := exec.CommandContext(videoCtx, ffmpeg,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "image2pipe", "-framerate", strconv.Itoa(opts.FPS), "-vcodec", "mjpeg", "-i", "pipe:0",
		"-frames:v", strconv.Itoa(frames),
		"-c:v", "libvpx-vp9", "-pix_fmt", "yuv420p", "-an",
		"-fs", strconv.FormatInt(s.store.maxArtifactBytes, 10), output,
	)
	// CommandContext terminates the encoder when the operation deadline expires.
	// WaitDelay additionally bounds Wait when a faulty executable leaves inherited
	// pipes open in a descendant process.
	command.WaitDelay = videoProcessWaitDelay
	stdin, err := command.StdinPipe()
	if err != nil {
		return Meta{}, err
	}
	stderr := newBoundedTailBuffer(videoStderrLimit)
	command.Stderr = &stderr
	command.Stdout = io.Discard
	if err := command.Start(); err != nil {
		return Meta{}, err
	}
	waited := false
	defer func() {
		if !waited {
			_ = stdin.Close()
			if command.Process != nil {
				_ = command.Process.Kill()
			}
			_ = command.Wait()
		}
	}()

	interval := time.Second / time.Duration(opts.FPS)
	started := time.Now()
	lastContinuityCheck := started
	for frame := 0; frame < frames; frame++ {
		if wait := time.Until(started.Add(time.Duration(frame) * interval)); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-videoCtx.Done():
				timer.Stop()
				return Meta{}, videoCaptureContextError(ctx, videoCtx)
			}
		}
		shot, err := s.captureVideoScreenshot(videoCtx)
		if err != nil {
			if videoCtx.Err() != nil {
				return Meta{}, videoCaptureContextError(ctx, videoCtx)
			}
			return Meta{}, fmt.Errorf("capture video frame %d: %w", frame, err)
		}
		if err := writeMJPEGFrame(stdin, shot); err != nil {
			if videoCtx.Err() != nil {
				return Meta{}, videoCaptureContextError(ctx, videoCtx)
			}
			return Meta{}, fmt.Errorf("stream video frame %d: %w", frame, err)
		}
		// The transport identity contains a monotonic replacement-document epoch,
		// so the final check is sufficient for safety even after A -> B -> A/BFCache.
		// Poll at a bounded cadence only to stop wasting encoder work soon after a
		// transition; checking every 30-fps frame would double browser round trips.
		if continuity != nil && time.Since(lastContinuityCheck) >= videoContinuityInterval {
			if err := continuity.verify(videoCtx); err != nil {
				if videoCtx.Err() != nil {
					return Meta{}, videoCaptureContextError(ctx, videoCtx)
				}
				return Meta{}, err
			}
			lastContinuityCheck = time.Now()
		}
	}
	closeErr := stdin.Close()
	err = command.Wait()
	waited = true
	if closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		if videoCtx.Err() != nil {
			return Meta{}, videoCaptureContextError(ctx, videoCtx)
		}
		message := strings.ToValidUTF8(strings.TrimSpace(stderr.String()), "�")
		if len(message) > videoErrorStderrLimit {
			start := len(message) - videoErrorStderrLimit
			for start < len(message) && !utf8.RuneStart(message[start]) {
				start++
			}
			message = message[start:]
		}
		if message == "" {
			return Meta{}, fmt.Errorf("encode video artifact: %w", err)
		}
		return Meta{}, fmt.Errorf("encode video artifact: %w: %s", err, message)
	}
	if videoCtx.Err() != nil {
		return Meta{}, videoCaptureContextError(ctx, videoCtx)
	}
	if err := os.Chmod(output, 0o600); err != nil {
		return Meta{}, err
	}
	file, err := os.Open(output)
	if err != nil {
		return Meta{}, err
	}
	defer file.Close()
	put.MIMEType = "video/webm"
	put.SourceHash = s.currentSourceHash(videoCtx)
	// PutContext is the concurrency-correct quota gate. Deliberately do not do
	// an unlocked quota preflight before encoding: without a Store reservation
	// transaction it can only race concurrent writers and promise capacity that
	// no longer exists by commit time.
	return s.store.PutContext(videoCtx, put, file)
}

func (s *Service) captureVideoScreenshot(ctx context.Context) (browser.Screenshot, error) {
	if err := ctx.Err(); err != nil {
		return browser.Screenshot{}, err
	}
	type result struct {
		shot browser.Screenshot
		err  error
	}
	finished := make(chan result, 1)
	go func() {
		var value result
		if capture, ok := s.browser.(rawScreenshotCapturer); ok {
			value.shot, value.err = capture.CaptureArtifactScreenshot(ctx, "")
		} else {
			value.shot, value.err = s.browser.Screenshot(ctx)
		}
		finished <- value
	}()
	select {
	case <-ctx.Done():
		// Browser transports are expected to honor ctx. Selecting here still
		// bounds the artifact request if a faulty implementation does not; the
		// buffered result channel lets a late cooperative return exit cleanly.
		return browser.Screenshot{}, ctx.Err()
	case value := <-finished:
		if err := ctx.Err(); err != nil {
			return browser.Screenshot{}, err
		}
		return value.shot, value.err
	}
}

func videoCaptureBudget(durationMS int) time.Duration {
	duration := time.Duration(durationMS) * time.Millisecond
	headroom := duration / 5
	if headroom < videoCaptureMinHeadroom {
		headroom = videoCaptureMinHeadroom
	}
	if headroom > videoCaptureMaxHeadroom {
		headroom = videoCaptureMaxHeadroom
	}
	return duration + headroom
}

func videoCaptureContextError(parent, operation context.Context) error {
	if err := parent.Err(); err != nil {
		return err
	}
	if err := operation.Err(); err != nil {
		return fmt.Errorf("video capture exceeded its bounded runtime: %w", err)
	}
	return errors.New("video capture stopped without a context error")
}

// boundedTailBuffer retains only the most recent limit bytes while always
// reporting a complete write. Child-process stderr can therefore never grow
// service memory without bound or deadlock the encoder after the limit is hit.
type boundedTailBuffer struct {
	buf   []byte
	limit int
}

func newBoundedTailBuffer(limit int) boundedTailBuffer {
	if limit < 0 {
		limit = 0
	}
	return boundedTailBuffer{buf: make([]byte, 0, limit), limit: limit}
}

func (b *boundedTailBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if b.limit == 0 || written == 0 {
		return written, nil
	}
	if written >= b.limit {
		b.buf = append(b.buf[:0], p[written-b.limit:]...)
		return written, nil
	}
	overflow := len(b.buf) + written - b.limit
	if overflow > 0 {
		copy(b.buf, b.buf[overflow:])
		b.buf = b.buf[:len(b.buf)-overflow]
	}
	b.buf = append(b.buf, p...)
	return written, nil
}

func (b *boundedTailBuffer) String() string { return string(b.buf) }

// resolveFFmpegPath handles the deliberately sparse PATH used by service
// managers such as launchd. Interactive shells commonly find Homebrew's
// ffmpeg while the same signed browser host cannot, which would make video
// artifacts work in tests but fail after installation. An explicit override
// wins; otherwise use PATH and then the conventional package-manager paths.
func resolveFFmpegPath() (string, error) {
	if override := strings.TrimSpace(os.Getenv("BRW_FFMPEG_PATH")); override != "" {
		if !filepath.IsAbs(override) {
			return "", errors.New("BRW_FFMPEG_PATH must be absolute")
		}
		if path, err := exec.LookPath(override); err == nil {
			return path, nil
		}
		return "", errors.New("BRW_FFMPEG_PATH is not an executable file")
	}
	if path, err := exec.LookPath("ffmpeg"); err == nil {
		return path, nil
	}
	for _, candidate := range []string{
		"/opt/homebrew/bin/ffmpeg",
		"/usr/local/bin/ffmpeg",
		"/usr/bin/ffmpeg",
	} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", errors.New("video capture requires ffmpeg on the browser host (install it or set BRW_FFMPEG_PATH)")
}

func validateCaptureOptions(opts CaptureOptions) error {
	if len(opts.Redaction) > 256 || !utf8.ValidString(opts.Redaction) {
		return errors.New("redaction label must be valid UTF-8 and at most 256 bytes")
	}
	if opts.TTL != 0 && opts.TTLSeconds != 0 {
		return errors.New("set only one of TTL or ttl_seconds")
	}
	if opts.TTLSeconds < 0 {
		return errors.New("ttl_seconds must not be negative")
	}
	if opts.Kind == "screenshot" {
		if len(opts.Ref) > 500 {
			return errors.New("screenshot ref is too long")
		}
		if opts.DurationMS != 0 || opts.FPS != 0 || opts.DownloadGUID != "" || opts.Filename != "" {
			return errors.New("screenshot capture received fields for another capture kind")
		}
		return nil
	}
	if opts.Ref != "" {
		return errors.New("ref is only valid for screenshot capture")
	}
	if opts.Kind == "video" {
		if opts.DownloadGUID != "" || opts.Filename != "" {
			return errors.New("video capture received download fields")
		}
		return nil
	}
	if opts.DurationMS != 0 || opts.FPS != 0 {
		return errors.New("duration_ms and fps are only valid for video capture")
	}
	if opts.Kind == "download" {
		if (opts.DownloadGUID == "") == (opts.Filename == "") {
			return errors.New("download capture requires exactly one of download_guid or filename")
		}
		if len(opts.DownloadGUID) > 500 || len(opts.Filename) > 1000 {
			return errors.New("download identifier is too long")
		}
		return nil
	}
	if opts.DownloadGUID != "" || opts.Filename != "" {
		return errors.New("download fields are only valid for download capture")
	}
	return nil
}

func writeMJPEGFrame(dst io.Writer, shot browser.Screenshot) error {
	data, err := screenshotData(shot)
	if err != nil {
		return err
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(shot.MIMEType, ";", 2)[0]))
	if mediaType == "image/jpeg" {
		_, err = io.Copy(dst, bytes.NewReader(data))
		return err
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return err
	}
	return jpeg.Encode(dst, decoded, &jpeg.Options{Quality: 75})
}

func screenshotData(shot browser.Screenshot) ([]byte, error) {
	if len(shot.Data) > 0 {
		return shot.Data, nil
	}
	if shot.Base64 == "" {
		return nil, errors.New("browser returned an empty screenshot")
	}
	data, err := base64.StdEncoding.DecodeString(shot.Base64)
	if err != nil {
		return nil, fmt.Errorf("decode screenshot: %w", err)
	}
	return data, nil
}

func (s *Service) currentSourceHash(ctx context.Context) string {
	value, err := s.browser.Evaluate(ctx, `({url:location.href,title:document.title})`)
	if err != nil {
		return ""
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *Service) beginRecipeCapture(ctx context.Context) (*recipeCaptureContinuity, error) {
	allowed, guarded := browser.AllowedOriginsFromContext(ctx)
	if !guarded {
		// Ordinary/manual artifact capture keeps its existing behavior and incurs
		// no document-identity round trip.
		return nil, nil
	}
	provider, ok := s.browser.(browser.DocumentIdentityProvider)
	if !ok {
		return nil, errors.New("deterministic recipe artifact capture requires main-document identity support")
	}
	identity, err := provider.DocumentIdentity(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// Transport errors can include current URLs. Preserve the failure class but
		// never copy those details into recipe/MCP results.
		return nil, errors.New("could not verify the main document for recipe artifact capture")
	}
	continuity := &recipeCaptureContinuity{provider: provider, allowed: allowed, start: identity}
	if err := continuity.validate(identity); err != nil {
		return nil, err
	}
	return continuity, nil
}

func (c *recipeCaptureContinuity) verify(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := c.provider.DocumentIdentity(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return errors.New("could not verify the main document for recipe artifact capture")
	}
	if err := c.validate(current); err != nil {
		return err
	}
	if current.ID != c.start.ID {
		return errors.New("recipe artifact capture crossed a main-document boundary")
	}
	return nil
}

func (c *recipeCaptureContinuity) validate(identity browser.DocumentIdentity) error {
	if c == nil {
		return nil
	}
	if identity.ID == "" || len(identity.ID) > 2048 || !utf8.ValidString(identity.ID) ||
		identity.Origin == "" || identity.Origin == "null" || len(identity.Origin) > 2048 || !utf8.ValidString(identity.Origin) {
		return errors.New("browser returned an invalid main-document identity for recipe artifact capture")
	}
	for _, candidate := range c.allowed {
		if identity.Origin == candidate {
			return nil
		}
	}
	return errors.New("recipe artifact capture origin is not allowed")
}

// guardCapturedPageURL checks the URL returned with the captured payload, not
// only a separate location.origin probe before/after capture. This closes the
// race where navigation occurs between the probe and Read/Snapshot and the
// transport returns bytes from a disallowed document.
func guardCapturedPageURL(ctx context.Context, rawURL string) error {
	allowed, guarded := browser.AllowedOriginsFromContext(ctx)
	if !guarded {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return errors.New("recipe artifact capture returned an invalid page URL")
	}
	origin := parsed.Scheme + "://" + parsed.Host
	for _, candidate := range allowed {
		if origin == candidate {
			return nil
		}
	}
	return errors.New("captured page origin is not allowed for recipe artifact capture")
}

func sourceHash(url, title string) string {
	sum := sha256.Sum256([]byte(url + "\x00" + title))
	return hex.EncodeToString(sum[:])
}
