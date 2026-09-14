package extensionbridge

import (
	"context"
	"time"

	"github.com/Don-Works/brw/internal/devtools"
)

var _ devtools.Observer = (*Bridge)(nil)

// Vitals reads the Core Web Vitals through the extension's debugger session.
// It evaluates the same expression the direct-CDP transport does, so a reading
// means the same thing on both.
func (b *Bridge) Vitals(ctx context.Context, opts devtools.VitalsOptions) (devtools.Vitals, error) {
	opts = opts.Normalize()
	var vitals devtools.Vitals
	if err := b.evaluateReadOnly(ctx, devtools.BuildVitalsExpression(opts), "", &vitals); err != nil {
		return devtools.Vitals{}, err
	}
	return vitals, nil
}

// AccessibilityAudit runs the embedded axe-core engine in the bridged tab. The
// engine crosses the bridge as an expression, never as a network fetch the page
// makes, so a signed-in profile is never asked to load third-party script.
func (b *Bridge) AccessibilityAudit(ctx context.Context, opts devtools.AuditOptions) (devtools.AuditResult, error) {
	opts = opts.Normalize()
	if err := b.ensureAxeInstalled(ctx); err != nil {
		return devtools.AuditResult{}, err
	}
	var raw devtools.RawAudit
	if err := b.evaluate(ctx, devtools.BuildAuditExpression(opts), "", &raw); err != nil {
		return devtools.AuditResult{}, err
	}
	return devtools.SummarizeAudit(raw, opts, time.Now())
}

// Highlight draws or removes the brw overlay in the bridged tab.
func (b *Bridge) Highlight(ctx context.Context, opts devtools.HighlightOptions) (devtools.HighlightResult, error) {
	opts, err := opts.Normalize()
	if err != nil {
		return devtools.HighlightResult{}, err
	}
	var result devtools.HighlightResult
	if err := b.evaluate(ctx, devtools.BuildHighlightExpression(opts), "", &result); err != nil {
		return devtools.HighlightResult{}, err
	}
	return result, nil
}

// ensureAxeInstalled injects the embedded engine unless the document already
// has a usable one. The probe is what keeps the half-megabyte payload to once
// per document rather than once per audit.
func (b *Bridge) ensureAxeInstalled(ctx context.Context) error {
	var probe struct {
		Present bool `json:"present"`
	}
	if err := b.evaluateReadOnly(ctx, devtools.AxeProbeScript, "", &probe); err != nil {
		return err
	}
	if probe.Present {
		return nil
	}
	var installed struct {
		Installed bool `json:"installed"`
	}
	if err := b.evaluate(ctx, devtools.AxeInstallExpression(), "", &installed); err != nil {
		return err
	}
	if !installed.Installed {
		return devtools.ErrAxeInstall
	}
	return nil
}
