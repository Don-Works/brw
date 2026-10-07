// Package devtools holds the read-oriented page observations an agent reaches for when it is debugging a page rather than driving it: Core Web Vitals, an accessibility audit, and a visual highlight for a human watching the browser.
package devtools

import (
	"context"
	"errors"
)

// ErrUnsupported is returned by a transport that cannot run these observations at all.
var ErrUnsupported = errors.New("this browser transport does not support developer observations (vitals, accessibility audit, highlight)")

// ErrNoObservation is returned when one of these internal expressions resolved to no value at all.
var ErrNoObservation = errors.New("the page returned no value for this observation; the document may have navigated or been detached mid-read")

// ErrAxeInstall is returned when the embedded engine was evaluated in the page but did not define window.axe.
var ErrAxeInstall = errors.New("axe-core did not install in this document: it may be an error page, a PDF or a browser-internal URL that cannot run script")

// Observer is the optional transport capability behind brw_vitals, brw_a11y and brw_highlight.
type Observer interface {
	Vitals(context.Context, VitalsOptions) (Vitals, error)
	AccessibilityAudit(context.Context, AuditOptions) (AuditResult, error)
	Highlight(context.Context, HighlightOptions) (HighlightResult, error)
}
