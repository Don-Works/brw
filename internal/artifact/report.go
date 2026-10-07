package artifact

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/devtools"
)

// KindAccessibilityReport is the store kind for a complete axe-core audit document.
const KindAccessibilityReport = "a11y_report"

// KindPerformanceTrace and KindCPUProfile are the store kinds for a Chrome performance trace and a V8 CPU profile.
const (
	KindPerformanceTrace = "perf_trace"
	KindCPUProfile       = "cpu_profile"
)

const maxReportBytes = 32 << 20

// ReportOptions describes a caller-computed document being handed to the store.
type ReportOptions struct {
	Kind string
	// SourceURL and SourceTitle identify the page the report describes.
	SourceURL   string
	SourceTitle string
	// TTL may shorten the store default; it can never lengthen it.
	TTL time.Duration
}

// PutReport stores a document the caller already computed.
func (s *Service) PutReport(ctx context.Context, opts ReportOptions, data []byte) (Meta, error) {
	if s == nil || s.store == nil {
		return Meta{}, errors.New("artifact store is not configured on the browser host")
	}
	if opts.Kind != KindAccessibilityReport && opts.Kind != KindPerformanceTrace && opts.Kind != KindCPUProfile {
		return Meta{}, errors.New("unsupported report kind")
	}
	if len(data) == 0 {
		return Meta{}, errors.New("report is empty")
	}
	if len(data) > maxReportBytes {
		return Meta{}, errors.New("report is too large to store")
	}
	if err := guardCapturedPageURL(ctx, opts.SourceURL); err != nil {
		return Meta{}, err
	}
	return s.store.PutContext(ctx, PutOptions{
		Kind:       opts.Kind,
		MIMEType:   "application/json",
		SourceHash: sourceHash(opts.SourceURL, opts.SourceTitle),
		TTL:        s.clampReportTTL(opts.TTL),
	}, bytes.NewReader(data))
}

func (s *Service) clampReportTTL(ttl time.Duration) time.Duration {
	if ttl < time.Second || ttl > s.store.ttl {
		return 0
	}
	return ttl
}

type reportPutter interface {
	PutReport(context.Context, ReportOptions, []byte) (Meta, error)
}

// AttachAuditReport moves the complete audit document out of result and into the store, leaving a payload-free handle in its place.
func AttachAuditReport(ctx context.Context, api API, result devtools.AuditResult, ttl time.Duration) devtools.AuditResult {
	report := result.Report
	result.Report = nil
	if result.Artifact != nil || len(report) == 0 {
		return result
	}
	putter, ok := api.(reportPutter)
	if !ok || putter == nil {
		result.Note = noteAlso(result.Note, "the browser host has no artifact store, so the full report was discarded; this summary is all of it")
		return result
	}
	meta, err := putter.PutReport(ctx, ReportOptions{
		Kind:        KindAccessibilityReport,
		SourceURL:   result.URL,
		SourceTitle: result.Title,
		TTL:         ttl,
	}, report)
	if err != nil {
		result.Note = noteAlso(result.Note, "the full report could not be stored: "+err.Error())
		return result
	}
	result.Artifact = &devtools.ArtifactRef{
		ID:        meta.ID,
		MIMEType:  meta.MIMEType,
		SizeBytes: meta.SizeBytes,
		SHA256:    meta.SHA256,
		ExpiresAt: meta.ExpiresAt,
	}
	return result
}

func noteAlso(existing, added string) string {
	if existing == "" {
		return added
	}
	return existing + "; " + added
}

// AttachPerformanceReport moves a captured trace or CPU profile out of result and into the store, leaving a payload-free handle in its place.
func AttachPerformanceReport(ctx context.Context, api API, result browser.ProfileResult, ttl time.Duration) browser.ProfileResult {
	data := result.Data
	result.Data = nil
	if result.Artifact != nil || len(data) == 0 {
		return result
	}
	putter, ok := api.(reportPutter)
	if !ok || putter == nil {
		result.Note = noteAlso(result.Note, "the browser host has no artifact store, so the capture was discarded")
		return result
	}
	kind := KindPerformanceTrace
	if result.Kind == browser.ProfileKindCPU {
		kind = KindCPUProfile
	}
	meta, err := putter.PutReport(ctx, ReportOptions{Kind: kind, TTL: ttl}, data)
	if err != nil {
		result.Note = noteAlso(result.Note, "the capture could not be stored: "+err.Error())
		return result
	}
	result.Artifact = &browser.ProfileArtifact{
		ID:        meta.ID,
		MIMEType:  meta.MIMEType,
		SizeBytes: meta.SizeBytes,
		SHA256:    meta.SHA256,
		ExpiresAt: meta.ExpiresAt,
	}
	return result
}
