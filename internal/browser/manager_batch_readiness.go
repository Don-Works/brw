package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

type batchReadinessKey struct{}

type batchReadiness struct {
	owner     context.Context
	document  string
	target    string
	condition string
	timeout   time.Duration
}

func batchReadinessContext(owner, tabCtx context.Context, step, next BatchStep) context.Context {
	switch step.Action {
	case "click", "click_text", "type", "fill", "select", "press":
	default:
		return tabCtx
	}
	if next.Action != "wait" || next.TimeoutMS <= 0 || !strings.HasPrefix(next.Condition, "fn:") {
		return tabCtx
	}
	source := strings.TrimSpace(strings.TrimPrefix(next.Condition, "fn:"))
	if source == "" {
		return tabCtx
	}
	encoded, err := json.Marshal(source)
	if err != nil {
		return tabCtx
	}
	document := batchDocumentIdentity(tabCtx)
	if document == "" {
		return tabCtx
	}
	var probe struct {
		Valid bool `json:"valid"`
		Value bool `json:"value"`
	}
	expr := fmt.Sprintf(`(() => {try {let fn; try {fn=new Function('"use strict"; return ('+%s+');');} catch (_) {fn=new Function('"use strict"; '+%s);} const value=fn(); if(value && typeof value.then==='function') return {valid:false,value:false}; return {valid:true,value:!!value};} catch (_) {return {valid:false,value:false};}})()`, encoded, encoded)
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(expr, &probe)); err != nil || !probe.Valid || probe.Value || document != batchDocumentIdentity(tabCtx) {
		return tabCtx
	}
	return context.WithValue(tabCtx, batchReadinessKey{}, batchReadiness{
		owner: owner, document: document, target: eventScopeFromCtx(tabCtx), condition: next.Condition, timeout: time.Duration(next.TimeoutMS) * time.Millisecond,
	})
}

func (m *Manager) runWithBatchReadiness(tabCtx context.Context, readiness batchReadiness, action func() error) error {
	ctx, cancel := context.WithCancel(tabCtx)
	defer cancel()
	stop := context.AfterFunc(readiness.owner, cancel)
	defer stop()
	if err := readiness.owner.Err(); err != nil {
		return err
	}
	if err := action(); err != nil {
		return err
	}
	deadline := time.Now().Add(readiness.timeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("timed out waiting for %q", readiness.condition)
		}
		matched, err := snapshot.WaitForConditionCancellable(ctx, readiness.condition, remaining.Milliseconds())
		if err == nil {
			if matched {
				return nil
			}
			return fmt.Errorf("timed out waiting for %q", readiness.condition)
		}
		if !isTransientNavigationError(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func batchDocumentIdentity(ctx context.Context) string {
	var identity string
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		tree, err := page.GetFrameTree().Do(ctx)
		if err != nil {
			return err
		}
		if tree != nil && tree.Frame != nil && tree.Frame.ID != "" && tree.Frame.LoaderID != "" {
			identity = string(tree.Frame.ID) + "/" + string(tree.Frame.LoaderID)
		}
		return nil
	}))
	if err != nil {
		return ""
	}
	return identity
}
