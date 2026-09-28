package extensionbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

func (b *Bridge) NetworkRequests(ctx context.Context, filter string) ([]browser.NetworkRequest, error) {
	filterJSON, _ := json.Marshal(filter)
	expr := fmt.Sprintf(`(function(filter) {
	  var entries = performance.getEntriesByType('resource');
	  if (filter) {
	    var lower = filter.toLowerCase();
	    entries = entries.filter(function(e) { return e.name.toLowerCase().indexOf(lower) !== -1; });
	  }
	  return entries.map(function(e) {
	    return {
	      url: e.name,
	      initiator_type: e.initiatorType || '',
	      start_time: Math.round(e.startTime),
	      duration: Math.round(e.duration),
	      transfer_size: e.transferSize || 0,
	      status: 0
	    };
	  });
	})(%s)`, filterJSON)
	var requests []browser.NetworkRequest
	if err := b.evaluate(ctx, expr, "", &requests); err != nil {
		return nil, err
	}
	return requests, nil
}

type bridgeActionBaseline struct {
	State   *browser.SemanticState
	Started time.Time
	// Trace carries the action's structured operands (ref, text, value) from the
	// call site to the recorder. Without it the recorder can only parse them back
	// out of a display message, which loses every operand it did not print.
	Trace browser.TraceEntry
	// Elements is the pre-action element list, kept so an action can resolve what
	// its ref pointed at BEFORE it acted. Resolving afterwards would record what
	// the element became — a button whose label flips on click would be recorded
	// under its new label, and a replay guard built from it could never pass.
	Elements []snapshot.Element
}

func (b *Bridge) observeActionWithBefore(ctx context.Context, message string, before bridgeActionBaseline) browser.ActionResult {
	return b.observeActionWithBeforeAndTabs(ctx, message, before, nil)
}

func (b *Bridge) observeActionWithBeforeAndTabs(ctx context.Context, message string, before bridgeActionBaseline, beforeTabIDs map[string]bool) browser.ActionResult {
	result := browser.ActionResult{OK: true, Message: message, TabID: b.contextTabID(ctx)}
	snap, err := b.snapshotLive(ctx, snapshot.SnapshotOptions{ViewportOnly: true})
	if err != nil {
		result.OK = false
		result.Message = message + "; observation failed: " + err.Error()
		b.finishObservedTrace(before, message, &result)
		return result
	}
	result.URL = snap.URL
	result.Title = snap.Title
	if snap.Metadata != nil {
		result.Version = browser.MetadataInt64(snap.Metadata["version"])
		if focus, ok := snap.Metadata["focused_ref"].(string); ok {
			result.Focus = focus
		}
	}
	after := browser.NewSemanticState(snap)
	browser.ApplyStateDiff(&result, before.State, after)
	frontier := browser.SelectFrontierElements(snap.Elements, result.Focus, 12)
	result.Elements = frontier
	result.Changed = browser.SummarizeElements(frontier, 12)
	if tabs, err := b.ListTabs(ctx); err == nil {
		result.Targets = actionTargets(tabs, b.activeTabID(), 8)
		// Detect if a new tab was opened by this action.
		if beforeTabIDs != nil {
			result.NewTabID = openedChildTabID(tabs, beforeTabIDs, result.TabID)
		}
	}
	if browser.WantSnapshotFromCtx(ctx) {
		result.Snapshot = &snap
	}
	b.finishObservedTrace(before, message, &result)
	return result
}

// openedChildTabID attributes a newly observed target only when Chrome reports
// that the acted-on tab opened it. A raw before/after set difference is unsafe
// in the shared daemon: another agent may legitimately open a tab during this
// action, and claiming that unrelated tab would violate its lease boundary.
func openedChildTabID(tabs []browser.Tab, before map[string]bool, sourceTabID string) string {
	for _, tab := range tabs {
		if !before[tab.ID] && tab.ID != sourceTabID && tab.OpenerTabID == sourceTabID {
			return tab.ID
		}
	}
	return ""
}

// traceOperands enriches a trace entry with the semantic identity of the ref it
// acts on, and withholds the value when that ref is a credential-bearing field.
// The trace is readable over the HTTP control plane and is not scoped to the
// lease that produced it, so a typed password must never reach it.
func (b *Bridge) traceOperands(before bridgeActionBaseline, entry browser.TraceEntry) browser.TraceEntry {
	if entry.Ref == "" {
		return entry
	}
	for _, el := range before.Elements {
		if el.Ref != entry.Ref {
			continue
		}
		entry.Name = el.Name
		entry.Role = el.Role
		entry.NameIsVisibleText = el.NameIsVisibleText
		if el.Sensitive {
			entry.Text = ""
			entry.Value = ""
			entry.Redacted = true
		}
		break
	}
	return entry
}

func (b *Bridge) captureSemanticState(ctx context.Context) bridgeActionBaseline {
	started := time.Now()
	snap, err := b.Snapshot(ctx, snapshot.SnapshotOptions{ViewportOnly: true})
	if err != nil {
		return bridgeActionBaseline{Started: started}
	}
	state := browser.NewSemanticState(snap)
	return bridgeActionBaseline{State: &state, Started: started, Elements: snap.Elements}
}

// captureTabIDs returns the set of current tab IDs, used to detect new tabs
// opened by an action. Returns nil on error (detection is best-effort).
func (b *Bridge) captureTabIDs(ctx context.Context) map[string]bool {
	tabs, err := b.ListTabs(ctx)
	if err != nil {
		return nil
	}
	ids := make(map[string]bool, len(tabs))
	for _, t := range tabs {
		ids[t.ID] = true
	}
	return ids
}

func (b *Bridge) Observe(ctx context.Context) (browser.ObserveResult, error) {
	tabID := browser.TabIDFromContext(ctx)
	if tabID == "" {
		tabID = b.ResolveActiveTabID(ctx)
		if tabID != "" {
			ctx = browser.WithTabID(ctx, tabID)
		}
	}
	snap, err := b.Snapshot(ctx, snapshot.SnapshotOptions{ViewportOnly: true})
	if err != nil {
		return browser.ObserveResult{}, err
	}
	if tabID == "" {
		tabID = b.activeTabID()
	}
	focus := ""
	if snap.Metadata != nil {
		if f, ok := snap.Metadata["focused_ref"].(string); ok {
			focus = f
		}
	}
	after := browser.NewSemanticState(snap)
	version, stateChanged := b.advanceObservation(tabID, after)

	var changed []string
	if stateChanged {
		changed = browser.SummarizeElements(browser.SelectFrontierElements(snap.Elements, focus, 12), 12)
	}
	return browser.ObserveResult{
		Version: version,
		URL:     snap.URL,
		Title:   snap.Title,
		Focus:   focus,
		Changed: changed,
		// brw_route's description promises that active routes are reported here so
		// mocked traffic is never invisible, and that promise is not about a
		// transport: an abort rule is exactly as invisible in a transcript when a
		// declarativeNetRequest rule enforces it as when Fetch does. A HAR
		// fixture's miss counts have no equivalent, because this transport refuses
		// a replay by name rather than installing one.
		ActiveRoutes: len(b.routes.list(tabID)),
	}, nil
}

func (b *Bridge) advanceObservation(tabID string, after browser.SemanticState) (int64, bool) {
	b.observeMu.Lock()
	next, version, stateChanged := browser.AdvanceObservationState(b.observedState[tabID], b.observeVersions[tabID], after)
	b.observedState[tabID] = next
	b.observeVersions[tabID] = version
	b.observeMu.Unlock()
	return version, stateChanged
}
