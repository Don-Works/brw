package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// FailureCapturePolicy is the daemon-level half of the decision.
type FailureCapturePolicy string

const (
	// FailureCaptureOff writes nothing on failure.
	FailureCaptureOff FailureCapturePolicy = "off"
	// FailureCaptureRecipe honours the per-recipe opt-in.
	FailureCaptureRecipe FailureCapturePolicy = "recipe"
	// FailureCaptureAll bundles every failed run, opted in or not.
	FailureCaptureAll FailureCapturePolicy = "all"
)

// EncryptionPolicy decides which captures are encrypted at rest.
type EncryptionPolicy string

const (
	// EncryptOff stores every artifact as plain bytes.
	EncryptOff EncryptionPolicy = "off"
	// EncryptRecipe encrypts artifacts captured during a private-recipe run, which is where brw handles a caller's own signed-in pages.
	EncryptRecipe EncryptionPolicy = "recipe"
	// EncryptAll encrypts every artifact.
	EncryptAll EncryptionPolicy = "all"
)

// ErrFailureBundlesDisabled is returned, by name, when a run asks for evidence that policy forbids.
var ErrFailureBundlesDisabled = errors.New("failure evidence bundles are disabled on this browser host")

const (
	defaultFailureBundleTTL = time.Hour

	maxBundleTraceEntries   = 200
	maxBundleNetworkEntries = 50
	maxBundleConsoleLines   = 100
	maxBundleTextBytes      = 1000
	maxBundleURLBytes       = 2048
)

func ParseFailureCapturePolicy(value string) (FailureCapturePolicy, error) {
	switch FailureCapturePolicy(strings.ToLower(strings.TrimSpace(value))) {
	case "", FailureCaptureOff:
		return FailureCaptureOff, nil
	case FailureCaptureRecipe:
		return FailureCaptureRecipe, nil
	case FailureCaptureAll:
		return FailureCaptureAll, nil
	}
	return "", fmt.Errorf("unknown failure-capture policy %q (valid: off, recipe, all)", value)
}

func ParseEncryptionPolicy(value string) (EncryptionPolicy, error) {
	switch EncryptionPolicy(strings.ToLower(strings.TrimSpace(value))) {
	case "", EncryptOff:
		return EncryptOff, nil
	case EncryptRecipe:
		return EncryptRecipe, nil
	case EncryptAll:
		return EncryptAll, nil
	}
	return "", fmt.Errorf("unknown artifact encryption policy %q (valid: off, recipe, all)", value)
}

// FailureBundleOptions describes one failure.
type FailureBundleOptions struct {
	Reason        string
	RecipeID      string
	RecipeVersion string
	FailedStep    string
	RecipeOptIn   bool
	TTL           time.Duration
}

// SetFailureCapturePolicy configures the daemon half of the failure-bundle decision.
func (s *Service) SetFailureCapturePolicy(policy FailureCapturePolicy) error {
	parsed, err := ParseFailureCapturePolicy(string(policy))
	if err != nil {
		return err
	}
	s.failureCapture = parsed
	return nil
}

// SetEncryptionPolicy configures at-rest encryption.
func (s *Service) SetEncryptionPolicy(policy EncryptionPolicy) error {
	parsed, err := ParseEncryptionPolicy(string(policy))
	if err != nil {
		return err
	}
	if parsed != EncryptOff && !s.store.EncryptionAvailable() {
		return errors.New("artifact encryption policy requires an artifact encryption key")
	}
	s.encryption = parsed
	return nil
}

// SetFailureBundleTTL shortens how long evidence is retained.
func (s *Service) SetFailureBundleTTL(ttl time.Duration) error {
	if ttl < time.Second {
		return errors.New("failure bundle TTL must be at least one second")
	}
	s.failureBundleTTL = ttl
	return nil
}

func (s *Service) failureCaptureAllowed(optIn bool) bool {
	switch s.failureCapture {
	case FailureCaptureAll:
		return true
	case FailureCaptureRecipe:
		return optIn
	default:
		return false
	}
}

func (s *Service) shouldEncrypt(ctx context.Context) bool {
	switch s.encryption {
	case EncryptAll:
		return true
	case EncryptRecipe:
		_, recipeScoped := browser.AllowedOriginsFromContext(ctx)
		return recipeScoped
	default:
		return false
	}
}

type bundlePart struct {
	role string
	meta Meta
}

// CaptureFailureBundle collects the evidence for one failure and returns the manifest handle.
func (s *Service) CaptureFailureBundle(ctx context.Context, opts FailureBundleOptions) (Meta, error) {
	if !s.failureCaptureAllowed(opts.RecipeOptIn) {
		return Meta{}, ErrFailureBundlesDisabled
	}
	if err := ctx.Err(); err != nil {
		return Meta{}, err
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = s.failureBundleTTL
	}
	if ttl <= 0 {
		ttl = defaultFailureBundleTTL
	}
	if storeTTL := s.store.TTL(); ttl > storeTTL {
		ttl = storeTTL
	}
	encrypt := s.shouldEncrypt(ctx)

	manifest := Manifest{
		SchemaVersion: ManifestSchemaVersion,
		CreatedAt:     s.store.now().UTC(),
		Reason:        boundedText(opts.Reason, maxManifestReasonBytes),
		RecipeID:      opts.RecipeID,
		RecipeVersion: opts.RecipeVersion,
		FailedStep:    opts.FailedStep,
	}

	capture := bundleCapture{ttl: ttl, encrypt: encrypt}
	continuity, continuityErr := s.beginRecipeCapture(ctx)
	capture.continuity = continuity

	var parts []bundlePart
	for _, collector := range s.bundleCollectors() {
		if collector.pageDerived && continuityErr != nil {

			manifest.Missing = append(manifest.Missing, MissingPart{
				Role: collector.role, Reason: bundleFailureReason(continuityErr),
			})
			continue
		}
		meta, err := collector.collect(s, ctx, capture)
		if err != nil {
			manifest.Missing = append(manifest.Missing, MissingPart{
				Role: collector.role, Reason: bundleFailureReason(err),
			})
			continue
		}
		parts = append(parts, bundlePart{role: collector.role, meta: meta})
		manifest.Entries = append(manifest.Entries, ManifestEntry{
			Role: collector.role, ArtifactID: meta.ID, Kind: meta.Kind,
			MIMEType: meta.MIMEType, SizeBytes: meta.SizeBytes, ExpiresAt: meta.ExpiresAt,
		})
	}
	meta, err := s.store.PutManifest(ctx, manifest, ttl, encrypt)
	if err != nil {

		for _, part := range parts {
			_ = s.store.Delete(part.meta.ID)
		}
		return Meta{}, err
	}
	return meta, nil
}

type bundleCapture struct {
	ttl        time.Duration
	encrypt    bool
	continuity *recipeCaptureContinuity
}

type bundleCollector struct {
	role string

	pageDerived bool
	collect     func(*Service, context.Context, bundleCapture) (Meta, error)
}

func (s *Service) bundleCollectors() []bundleCollector {
	return []bundleCollector{
		{role: "action_trace", collect: (*Service).captureBundleTrace},
		{role: "console", pageDerived: true, collect: (*Service).captureBundleConsole},
		{role: "network", pageDerived: true, collect: (*Service).captureBundleNetwork},
		{role: "semantic_snapshot", pageDerived: true, collect: (*Service).captureBundleSnapshot},
		{role: "screenshot", pageDerived: true, collect: (*Service).captureBundleScreenshot},
	}
}

func (s *Service) putEvidence(ctx context.Context, value any, capture bundleCapture) (Meta, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return Meta{}, err
	}
	return s.store.PutContext(ctx, PutOptions{
		Kind: "evidence", MIMEType: "application/json", TTL: capture.ttl,
		Redaction: "failure-bundle", Encrypt: capture.encrypt,
	}, bytes.NewReader(data))
}

// BundleTrace is the recorded action history, already stripped of any value the transport marked sensitive when it recorded it.
type BundleTrace struct {
	Entries  []browser.TraceEntry `json:"entries"`
	Total    int                  `json:"total"`
	Returned int                  `json:"returned"`
}

func (s *Service) captureBundleTrace(ctx context.Context, capture bundleCapture) (Meta, error) {
	trace := s.browser.GetTrace()
	tabID := browser.TabIDFromContext(ctx)
	entries := make([]browser.TraceEntry, 0, len(trace.Entries))
	for _, entry := range trace.Entries {
		if tabID != "" && entry.TabID != "" && entry.TabID != tabID {
			continue
		}
		if entry.Redacted {

			entry.Text, entry.Value = "", ""
		}
		entry.Text = boundedText(entry.Text, maxBundleTextBytes)
		entry.Value = boundedText(entry.Value, maxBundleTextBytes)
		entries = append(entries, entry)
	}
	total := len(entries)
	if len(entries) > maxBundleTraceEntries {
		entries = entries[len(entries)-maxBundleTraceEntries:]
	}
	return s.putEvidence(ctx, BundleTrace{Entries: entries, Total: total, Returned: len(entries)}, capture)
}

// BundleConsole is a summary, not a dump: level counts for the whole buffer and the tail that is most likely to name the failure.
type BundleConsole struct {
	Counts   map[string]int           `json:"counts"`
	Total    int                      `json:"total"`
	Returned int                      `json:"returned"`
	Messages []browser.ConsoleMessage `json:"messages"`
}

func (s *Service) captureBundleConsole(ctx context.Context, capture bundleCapture) (Meta, error) {
	messages, err := s.browser.ConsoleMessages(ctx)
	if err != nil {
		return Meta{}, err
	}

	if err := capture.continuity.verify(ctx); err != nil {
		return Meta{}, err
	}
	counts := map[string]int{}
	for index := range messages {
		counts[messages[index].Level]++
		messages[index].Text = boundedText(messages[index].Text, maxBundleTextBytes)
	}
	total := len(messages)
	if len(messages) > maxBundleConsoleLines {
		messages = messages[len(messages)-maxBundleConsoleLines:]
	}
	return s.putEvidence(ctx, BundleConsole{
		Counts: counts, Total: total, Returned: len(messages), Messages: messages,
	}, capture)
}

// BundleNetworkEntry is metadata only.
type BundleNetworkEntry struct {
	Method     string            `json:"method"`
	URL        string            `json:"url"`
	Status     int               `json:"status"`
	OK         bool              `json:"ok"`
	Completed  bool              `json:"completed"`
	DurationMS float64           `json:"duration_ms"`
	Error      string            `json:"error,omitempty"`
	Headers    map[string]string `json:"request_headers,omitempty"`
	// WithheldHeaders names the credential-bearing headers this request carried.
	WithheldHeaders []string `json:"withheld_headers,omitempty"`
}

type BundleNetwork struct {
	Requests []BundleNetworkEntry `json:"requests"`
	Total    int                  `json:"total"`
	Returned int                  `json:"returned"`
}

func (s *Service) captureBundleNetwork(ctx context.Context, capture bundleCapture) (Meta, error) {
	captured, err := s.browser.NetworkCapture(ctx, "")
	if err != nil {
		return Meta{}, err
	}

	if err := capture.continuity.verify(ctx); err != nil {
		return Meta{}, err
	}

	captured = snapshot.RedactCapturedCredentials(captured)
	total := len(captured)
	if len(captured) > maxBundleNetworkEntries {
		captured = captured[len(captured)-maxBundleNetworkEntries:]
	}
	requests := make([]BundleNetworkEntry, 0, len(captured))
	for _, item := range captured {
		entry := BundleNetworkEntry{
			Method: item.Method, URL: boundedText(item.URL, maxBundleURLBytes),
			Status: item.Status, OK: item.OK, Completed: item.Completed,
			DurationMS: item.DurationMS, Error: boundedText(item.Error, maxBundleTextBytes),
		}
		for name, value := range item.RequestHeaders {
			if snapshot.SensitiveHeader(name) {
				entry.WithheldHeaders = append(entry.WithheldHeaders, strings.ToLower(strings.TrimSpace(name)))
				continue
			}
			if entry.Headers == nil {
				entry.Headers = map[string]string{}
			}
			entry.Headers[name] = boundedText(value, maxBundleTextBytes)
		}
		slices.Sort(entry.WithheldHeaders)
		requests = append(requests, entry)
	}
	return s.putEvidence(ctx, BundleNetwork{
		Requests: requests, Total: total, Returned: len(requests),
	}, capture)
}

func (s *Service) captureBundleSnapshot(ctx context.Context, capture bundleCapture) (Meta, error) {
	snap, err := s.browser.Snapshot(ctx, snapshot.SnapshotOptions{Mode: "all", ViewportOnly: false})
	if err != nil {
		return Meta{}, err
	}
	if err := guardCapturedPageURL(ctx, snap.URL); err != nil {
		return Meta{}, err
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return Meta{}, err
	}
	return s.store.PutContext(ctx, PutOptions{
		Kind: "semantic_json", MIMEType: "application/json", TTL: capture.ttl,
		SourceHash: sourceHash(snap.URL, snap.Title), Redaction: "failure-bundle",
		Encrypt: capture.encrypt,
	}, bytes.NewReader(data))
}

func (s *Service) captureBundleScreenshot(ctx context.Context, capture bundleCapture) (Meta, error) {
	var (
		shot browser.Screenshot
		err  error
	)
	if capturer, ok := s.browser.(rawScreenshotCapturer); ok {
		shot, err = capturer.CaptureArtifactScreenshot(ctx, "")
	} else {
		shot, err = s.browser.Screenshot(ctx)
	}
	if err != nil {
		return Meta{}, err
	}
	data, err := screenshotData(shot)
	if err != nil {
		return Meta{}, err
	}

	if err := capture.continuity.verify(ctx); err != nil {
		return Meta{}, err
	}
	return s.store.PutContext(ctx, PutOptions{
		Kind: "screenshot", MIMEType: shot.MIMEType, TTL: capture.ttl,
		Redaction: "failure-bundle", Encrypt: capture.encrypt,
	}, bytes.NewReader(data))
}

func bundleFailureReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	default:
		return "unavailable"
	}
}

func boundedText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
