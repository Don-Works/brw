// Package devtools holds the read-oriented page observations an agent reaches
// for when it is debugging a page rather than driving it: Core Web Vitals, an
// accessibility audit, and a visual highlight for a human watching the browser.
//
// Every script here is shared by both transports. The direct-CDP Manager and
// the extension Bridge evaluate the same expression, so an observation cannot
// mean one thing on one transport and something else on the other.
package devtools

import (
	"context"
	"errors"
)

// ErrUnsupported is returned by a transport that cannot run these observations
// at all. It is a named capability error rather than an empty result, so a
// caller can tell "this browser connection cannot do it" from "the page is
// clean".
var ErrUnsupported = errors.New("this browser transport does not support developer observations (vitals, accessibility audit, highlight)")

// ErrAxeInstall is returned when the embedded engine was evaluated in the page
// but did not define window.axe. The usual cause is a document that cannot run
// script at all — an error page, a PDF viewer, or a chrome:// URL.
var ErrAxeInstall = errors.New("axe-core did not install in this document: it may be an error page, a PDF or a browser-internal URL that cannot run script")

// Observer is the optional transport capability behind brw_vitals, brw_a11y and
// brw_highlight. Both first-party transports implement it; the interface exists
// so an upstream HTTP controller that has not been upgraded degrades to a named
// capability error instead of failing to compile.
type Observer interface {
	Vitals(context.Context, VitalsOptions) (Vitals, error)
	AccessibilityAudit(context.Context, AuditOptions) (AuditResult, error)
	Highlight(context.Context, HighlightOptions) (HighlightResult, error)
}
