package extensionbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

func (b *Bridge) AssertVisible(ctx context.Context, ref string, timeout time.Duration) error {
	if err := browser.GuardCrossOriginRefs("assert visible", browser.BridgeCrossOriginRemedy, ref); err != nil {
		return err
	}
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	return b.evalAssert(ctx, snapshot.AssertVisibleScript, ref, timeout.Milliseconds())
}

func (b *Bridge) AssertText(ctx context.Context, ref, text string, timeout time.Duration) error {
	if err := browser.GuardCrossOriginRefs("assert text", browser.BridgeCrossOriginRemedy, ref); err != nil {
		return err
	}
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	return b.evalAssert(ctx, snapshot.AssertTextScript, ref, text, timeout.Milliseconds())
}

func (b *Bridge) AssertValue(ctx context.Context, ref, value string, timeout time.Duration) error {
	if err := browser.GuardCrossOriginRefs("assert value", browser.BridgeCrossOriginRemedy, ref); err != nil {
		return err
	}
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	return b.evalAssert(ctx, snapshot.AssertValueScript, ref, value, timeout.Milliseconds())
}

func (b *Bridge) AssertValueContains(ctx context.Context, ref, value string, timeout time.Duration) error {
	if err := browser.GuardCrossOriginRefs("assert value contains", browser.BridgeCrossOriginRemedy, ref); err != nil {
		return err
	}
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	return b.evalAssert(ctx, snapshot.AssertValueContainsScript, ref, value, timeout.Milliseconds())
}

func (b *Bridge) AssertHidden(ctx context.Context, ref string, timeout time.Duration) error {
	if err := browser.GuardCrossOriginRefs("assert hidden", browser.BridgeCrossOriginRemedy, ref); err != nil {
		return err
	}
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	return b.evalAssert(ctx, snapshot.AssertHiddenScript, ref, timeout.Milliseconds())
}

func (b *Bridge) evalAssert(ctx context.Context, script string, args ...any) error {
	marshaled := make([]string, len(args))
	for i, arg := range args {
		value, _ := json.Marshal(arg)
		marshaled[i] = string(value)
	}
	var ok bool
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", script, strings.Join(marshaled, ",")), "", &ok); err != nil {
		return err
	}
	if !ok {
		return snapshot.ErrAssertionTimeout
	}
	return nil
}

func (b *Bridge) CommitField(ctx context.Context, ref string) error {
	if err := browser.GuardCrossOriginRefs("commit field", browser.BridgeCrossOriginRemedy, ref); err != nil {
		return err
	}
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	refJSON, _ := json.Marshal(ref)
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.CommitFieldScript, refJSON), "", &result); err != nil {
		return err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "commit failed"
		}
		return fmt.Errorf("commit: %s", result.Error)
	}
	return nil
}
