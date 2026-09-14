package browser

import (
	"context"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/input"
)

// findInTab adapts a bare search function to FindActFinder. Batch and plan steps
// run against an ALREADY-RESOLVED tab context, so they search with the raw
// in-page primitive rather than Manager.Find, which would re-resolve the active
// tab and could search a different tab than the step is acting on.
type findInTab func(context.Context, snapshot.FindOptions) (snapshot.FindResult, error)

func (f findInTab) Find(ctx context.Context, opts snapshot.FindOptions) (snapshot.FindResult, error) {
	return f(ctx, opts)
}

// tabFinder is the searcher a batch/plan find_act step uses.
func tabFinder() FindActFinder { return findInTab(snapshot.Find) }

// findActuator exposes the direct-CDP transport's raw element primitives — the
// same ones the equivalent single-verb batch steps call, so a find_act step
// behaves exactly like a find followed by that step, minus the round trip and
// minus the chance of acting on a ref that was one of several matches.
func (m *Manager) findActuator(tabID string) FindActuator {
	return FindActuator{
		Click: func(ctx context.Context, ref string) error {
			if err := snapshot.WaitForActionable(ctx, ref, 5000); err != nil {
				return err
			}
			// A held modifier has to reach the click, exactly as the plain click
			// step does: a find_act inside a Shift+click flow that dropped the
			// mask would report an ordinary click and be wrong about it.
			if modifiers := input.Modifier(m.heldModifierMask(tabID)); modifiers != 0 {
				_, err := m.clickElementCenterWithModifiers(ctx, ref, modifiers)
				return err
			}
			_, err := m.clickElementCenter(ctx, ref, actionSettleDelay)
			return err
		},
		Fill: func(ctx context.Context, ref, value string) error {
			return m.fillRef(ctx, ref, value, true)
		},
		Type: func(ctx context.Context, ref, value string) error {
			return m.typeRef(ctx, ref, value)
		},
		Select: func(ctx context.Context, ref, value string) error {
			_, err := m.selectValue(ctx, ref, value)
			return err
		},
		Hover: func(ctx context.Context, ref string) error {
			_, err := m.hoverRef(ctx, tabID, ref)
			return err
		},
	}
}
