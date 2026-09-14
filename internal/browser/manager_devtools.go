package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/Don-Works/brw/internal/devtools"
)

// Every transport that can run these observations declares it here, so a
// signature that drifts is a compile error rather than a tool that quietly
// reports the capability missing.
var _ devtools.Observer = (*Manager)(nil)

// auditFloor is the minimum deadline an accessibility audit gets, regardless of
// the manager's ordinary per-call timeout. axe evaluates every rule against
// every node: on a large application that is seconds of renderer work, and a
// 20-second page timeout tuned for a click would abandon it mid-run.
const auditFloor = 90 * time.Second

// Vitals reads the Core Web Vitals for the document in the target tab.
func (m *Manager) Vitals(ctx context.Context, opts devtools.VitalsOptions) (devtools.Vitals, error) {
	start := time.Now()
	opts = opts.Normalize()
	// The script waits out its own settle window inside the page, so the call
	// deadline has to clear it with room for the round trip.
	budget := m.timeout + time.Duration(opts.SettleMS)*time.Millisecond
	tabID, tabCtx, cancel, err := m.devtoolsContext(ctx, budget)
	if err != nil {
		return devtools.Vitals{}, err
	}
	defer cancel()

	var vitals devtools.Vitals
	err = evaluateAwait(tabCtx, devtools.BuildVitalsExpression(opts), &vitals)
	m.recordObservation(tabID, TraceActionVitals, vitals.URL, start, err)
	if err != nil {
		return devtools.Vitals{}, err
	}
	return vitals, nil
}

// AccessibilityAudit runs the embedded axe-core engine against the target tab.
// The engine is injected from the binary; nothing is fetched over the network.
func (m *Manager) AccessibilityAudit(ctx context.Context, opts devtools.AuditOptions) (devtools.AuditResult, error) {
	start := time.Now()
	opts = opts.Normalize()
	tabID, tabCtx, cancel, err := m.devtoolsContext(ctx, auditFloor)
	if err != nil {
		return devtools.AuditResult{}, err
	}
	defer cancel()

	result, err := func() (devtools.AuditResult, error) {
		if err := ensureAxeInstalled(tabCtx); err != nil {
			return devtools.AuditResult{}, err
		}
		var raw devtools.RawAudit
		if err := evaluateAwait(tabCtx, devtools.BuildAuditExpression(opts), &raw); err != nil {
			return devtools.AuditResult{}, err
		}
		return devtools.SummarizeAudit(raw, opts, time.Now())
	}()
	m.recordObservation(tabID, TraceActionAudit, result.URL, start, err)
	return result, err
}

// Highlight draws or removes the brw overlay in the target tab.
func (m *Manager) Highlight(ctx context.Context, opts devtools.HighlightOptions) (devtools.HighlightResult, error) {
	start := time.Now()
	opts, err := opts.Normalize()
	if err != nil {
		return devtools.HighlightResult{}, err
	}
	tabID, tabCtx, cancel, err := m.devtoolsContext(ctx, m.timeout)
	if err != nil {
		return devtools.HighlightResult{}, err
	}
	defer cancel()

	var result devtools.HighlightResult
	err = evaluateAwait(tabCtx, devtools.BuildHighlightExpression(opts), &result)
	// An input action, not an observation: this one appends an element to the
	// document, so it belongs in the trace a human reads to see what brw did.
	if tabID != "" {
		entry := NewObservationTrace(TraceActionHighlight, HighlightTraceText(opts), start, err)
		if len(opts.Refs) > 0 {
			entry.Ref = opts.Refs[0]
		}
		m.recordTrace(tabID, entry)
	}
	if err != nil {
		return devtools.HighlightResult{}, err
	}
	return result, nil
}

// HighlightTraceText renders what a highlight call did, for the trace. The
// caller's label is left out on purpose: it is free text that has already been
// drawn into the page, and the trace is served over the HTTP control plane to
// every caller of a shared daemon.
func HighlightTraceText(opts devtools.HighlightOptions) string {
	if opts.Clear {
		return "clear"
	}
	return strings.Join(opts.Refs, " ")
}

// devtoolsContext resolves the target tab with a deadline of its own. The
// ordinary activeContext deadline is sized for an interaction; these three
// observations each have their own honest cost.
func (m *Manager) devtoolsContext(ctx context.Context, timeout time.Duration) (string, context.Context, context.CancelFunc, error) {
	if timeout < m.timeout {
		timeout = m.timeout
	}
	if tabID := tabIDFromCtx(ctx); tabID != "" {
		tabCtx, err := m.tabContext(tabID)
		if err != nil {
			return "", nil, nil, err
		}
		timeoutCtx, cancel := context.WithTimeout(tabCtx, timeout)
		return tabID, timeoutCtx, cancel, nil
	}
	return m.activeContextWithTimeout(ctx, timeout)
}

// ensureAxeInstalled injects the embedded engine unless a usable one is already
// in the document. A page that ships its own axe keeps it: replacing another
// script's global is a side effect an audit has no business having.
func ensureAxeInstalled(tabCtx context.Context) error {
	var probe struct {
		Present bool `json:"present"`
	}
	if err := evaluateAwait(tabCtx, devtools.AxeProbeScript, &probe); err != nil {
		return err
	}
	if probe.Present {
		return nil
	}
	var installed struct {
		Installed bool `json:"installed"`
	}
	if err := evaluateAwait(tabCtx, devtools.AxeInstallExpression(), &installed); err != nil {
		return err
	}
	if !installed.Installed {
		return devtools.ErrAxeInstall
	}
	return nil
}

// evaluateAwait runs one internal expression and decodes its value. It is not
// Manager.Evaluate: these expressions are machine-written, so recording them as
// brw_evaluate entries would make the trace claim the agent ran raw JavaScript.
func evaluateAwait(tabCtx context.Context, expression string, dst any) error {
	return chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		obj, exception, err := runtime.Evaluate(expression).
			WithAwaitPromise(true).
			WithReturnByValue(true).
			Do(ctx)
		if err != nil {
			return err
		}
		if exception != nil {
			if message := FormatRuntimeException(exception); message != "" {
				return fmt.Errorf("runtime exception: %s", message)
			}
			details, _ := json.Marshal(exception)
			return fmt.Errorf("runtime exception: %s", details)
		}
		if obj == nil || len(obj.Value) == 0 || string(obj.Value) == "null" {
			// Every expression here resolves to an object. An undefined
			// completion value means the evaluation did not reach the script —
			// a detached target, a document swapped mid-call — and decoding it
			// into a zero struct would report an unmeasured page as a clean one.
			return devtools.ErrNoObservation
		}
		return json.Unmarshal(obj.Value, dst)
	}))
}
