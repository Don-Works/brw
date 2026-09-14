package extensionbridge

import (
	"context"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// findActuator exposes the extension transport's raw element primitives — the
// same ones the equivalent single-verb batch/plan steps call, so a find_act step
// behaves exactly like a find followed by that step on this transport too.
func (b *Bridge) findActuator() browser.FindActuator {
	return browser.FindActuator{
		Click: b.clickRef,
		Fill: func(ctx context.Context, ref, value string) error {
			_, err := b.fillOptions(ctx, snapshot.FillOptions{Ref: ref, Text: value, Replace: true})
			return err
		},
		Type: b.typeRef,
		Select: func(ctx context.Context, ref, value string) error {
			_, err := b.selectValue(ctx, ref, value)
			return err
		},
		Hover: b.hoverRef,
	}
}
