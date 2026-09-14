package artifact

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/Don-Works/brw/internal/devtools"
)

// KindAccessibilityReport is the store kind for a complete axe-core audit
// document. It is not a brw_artifact_capture kind: a report is produced by the
// tool that ran the audit, not by asking the store to go and fetch something.
const KindAccessibilityReport = "a11y_report"

// maxReportBytes bounds one stored report. An axe document for a large single
// page application runs to a few megabytes; anything past this is a runaway, not
// a report, and the store quota should not be spent finding that out.
const maxReportBytes = 32 << 20

// ReportOptions describes a caller-computed document being handed to the store.
type ReportOptions struct {
	Kind string
	// SourceURL and SourceTitle identify the page the report describes. They
	// become the same opaque source hash a capture carries, so a recipe-scoped
	// continuity check treats a report like any other artifact.
	SourceURL   string
	SourceTitle string
	// TTL may shorten the store default; it can never lengthen it. A request
	// longer than the store's own retention is clamped to it rather than
	// refused, because the caller asking for a longer life is not a reason to
	// throw the report away.
	TTL time.Duration
}

// PutReport stores a document the caller already computed.
//
// It exists because a tool that both observes and summarizes has the full
// result in hand, and round-tripping the page a second time through
// CaptureArtifact would audit a page that may have changed in between. The
// kinds it accepts are closed: this is not a general upload path into the
// browser host's cache.
func (s *Service) PutReport(ctx context.Context, opts ReportOptions, data []byte) (Meta, error) {
	if s == nil || s.store == nil {
		return Meta{}, errors.New("artifact store is not configured on the browser host")
	}
	if opts.Kind != KindAccessibilityReport {
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

// clampReportTTL folds a caller's retention request into what the store will
// accept. Zero, anything sub-second and anything past the store's own retention
// all become the store default, so a caller asking for a week on a store that
// keeps artifacts for a day still gets a report rather than an error.
func (s *Service) clampReportTTL(ttl time.Duration) time.Duration {
	if ttl < time.Second || ttl > s.store.ttl {
		return 0
	}
	return ttl
}

// reportPutter is what AttachAuditReport needs and only the local Service has.
// In --upstream-http mode the API is the proxy, the browser host has already
// stored the report, and the disposable process must not open a second store.
type reportPutter interface {
	PutReport(context.Context, ReportOptions, []byte) (Meta, error)
}

// AttachAuditReport moves the complete audit document out of result and into
// the store, leaving a payload-free handle in its place.
//
// The report is dropped from result either way. A summary that quietly carried
// a megabyte of JSON would defeat the arrangement the artifact exists for, so
// every failure here becomes a note saying the report is gone — never a larger
// answer. It lives in this package because both the MCP tool and the HTTP route
// have to do exactly this, and two copies would drift.
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
