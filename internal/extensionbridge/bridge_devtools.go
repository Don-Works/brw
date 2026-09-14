package extensionbridge

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/devtools"
)

var _ devtools.Observer = (*Bridge)(nil)

// Vitals reads the Core Web Vitals through the extension's debugger session.
// It evaluates the same expression the direct-CDP transport does, so a reading
// means the same thing on both.
func (b *Bridge) Vitals(ctx context.Context, opts devtools.VitalsOptions) (devtools.Vitals, error) {
	start := time.Now()
	opts = opts.Normalize()
	var vitals devtools.Vitals
	err := b.evaluateObservation(ctx, devtools.BuildVitalsExpression(opts), true, &vitals)
	b.recordObservation(b.contextTabID(ctx), browser.TraceActionVitals, vitals.URL, start, err)
	if err != nil {
		return devtools.Vitals{}, err
	}
	return vitals, nil
}

// AccessibilityAudit runs the embedded axe-core engine in the bridged tab. The
// engine crosses the bridge as an expression, never as a network fetch the page
// makes, so a signed-in profile is never asked to load third-party script.
func (b *Bridge) AccessibilityAudit(ctx context.Context, opts devtools.AuditOptions) (devtools.AuditResult, error) {
	start := time.Now()
	opts = opts.Normalize()
	result, err := func() (devtools.AuditResult, error) {
		if err := b.ensureAxeInstalled(ctx); err != nil {
			return devtools.AuditResult{}, err
		}
		var raw devtools.RawAudit
		if err := b.evaluateObservation(ctx, devtools.BuildAuditExpression(opts), false, &raw); err != nil {
			return devtools.AuditResult{}, err
		}
		return devtools.SummarizeAudit(raw, opts, time.Now())
	}()
	b.recordObservation(b.contextTabID(ctx), browser.TraceActionAudit, result.URL, start, err)
	return result, err
}

// Highlight draws or removes the brw overlay in the bridged tab.
func (b *Bridge) Highlight(ctx context.Context, opts devtools.HighlightOptions) (devtools.HighlightResult, error) {
	start := time.Now()
	opts, err := opts.Normalize()
	if err != nil {
		return devtools.HighlightResult{}, err
	}
	var result devtools.HighlightResult
	err = b.evaluateObservation(ctx, devtools.BuildHighlightExpression(opts), false, &result)
	// An input action, not an observation: this appends an element to the
	// document, so a human reading the daemon's activity sees it as a change.
	if tabID := b.contextTabID(ctx); strings.TrimSpace(tabID) != "" {
		entry := browser.NewObservationTrace(browser.TraceActionHighlight, browser.HighlightTraceText(opts), start, err)
		if len(opts.Refs) > 0 {
			entry.Ref = opts.Refs[0]
		}
		entry.TabID = tabID
		b.appendTrace(entry)
	}
	if err != nil {
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
	if err := b.evaluateObservation(ctx, devtools.AxeProbeScript, true, &probe); err != nil {
		return err
	}
	if probe.Present {
		return nil
	}
	var installed struct {
		Installed bool `json:"installed"`
	}
	if err := b.evaluateObservation(ctx, devtools.AxeInstallExpression(), false, &installed); err != nil {
		return err
	}
	if !installed.Installed {
		return devtools.ErrAxeInstall
	}
	return nil
}

// evaluateObservation runs one of the shared devtools expressions and decodes
// its value.
//
// It does not decode straight into dst, because the generic evaluate path turns
// an undefined completion value into JSON null on purpose — a page script that
// returns nothing is a successful evaluation. These expressions always resolve
// to an object, so the same null here means the script never ran, and decoding
// it would answer with a zero reading that reads exactly like a clean page.
func (b *Bridge) evaluateObservation(ctx context.Context, expression string, readOnly bool, dst any) error {
	var value json.RawMessage
	var err error
	if readOnly {
		err = b.evaluateReadOnly(ctx, expression, "", &value)
	} else {
		err = b.evaluate(ctx, expression, "", &value)
	}
	if err != nil {
		return err
	}
	if len(value) == 0 || string(value) == "null" {
		return devtools.ErrNoObservation
	}
	return json.Unmarshal(value, dst)
}
