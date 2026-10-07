package extensionbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Don-Works/brw/internal/browser"
)

const dialogsUnsupportedNote = "Dialog control is unavailable: the connected brw extension predates brw_dialog support. Dialogs are still answered automatically (alert accepted, confirm/prompt given the non-destructive answer), but they cannot be pre-armed or listed. Reload the brw extension to enable it, or check supported=false to detect this programmatically"

// Dialog implements browser.DialogController.
func (b *Bridge) Dialog(ctx context.Context, opts browser.DialogOptions) (browser.DialogResult, error) {
	tabID := strings.TrimSpace(opts.TabID)
	if tabID == "" {
		tabID = browser.TabIDFromContext(ctx)
	}
	if tabID == "" && (!b.navPolicy.Empty() || browser.FrameReadCheckFromContext(ctx) != nil) {
		tabID = b.contextTabID(ctx)
	}
	if tabID != "" {
		ctx = browser.WithTabID(ctx, tabID)
	}
	if err := b.guardCurrentURL(ctx); err != nil {
		return browser.DialogResult{}, err
	}
	action := strings.ToLower(strings.TrimSpace(opts.Action))
	if action == "" {
		action = "status"
	}

	params := map[string]any{}
	if tabID != "" {
		params["tabId"] = parseTabID(tabID)
	}

	switch action {
	case "status":
		params["peek"] = opts.Peek
		raw, err := b.call(ctx, "get_dialogs", params)
		if err != nil {
			if isUnknownMessageTypeErr(err) {
				return browser.DialogResult{Action: action, Dialogs: []browser.DialogRecord{}, Supported: false, TabID: tabID}, nil
			}
			return browser.DialogResult{}, err
		}
		var payload struct {
			Dialogs []browser.DialogRecord `json:"dialogs"`
			Armed   *struct {
				Accept     bool   `json:"accept"`
				Remaining  int    `json:"remaining"`
				PromptText string `json:"prompt_text"`
			} `json:"armed"`
		}
		if len(raw) > 0 {
			if jsonErr := json.Unmarshal(raw, &payload); jsonErr != nil {
				return browser.DialogResult{}, fmt.Errorf("parse dialogs: %w", jsonErr)
			}
		}
		if err := b.guardCurrentURL(ctx); err != nil {
			return browser.DialogResult{}, err
		}
		for _, dialog := range payload.Dialogs {
			if dialog.URL == "" {
				continue
			}
			if err := b.enforceFinalURL(ctx, dialog.URL); err != nil {
				return browser.DialogResult{}, err
			}
		}
		if payload.Dialogs == nil {
			payload.Dialogs = []browser.DialogRecord{}
		}
		result := browser.DialogResult{
			Action:    "status",
			Dialogs:   payload.Dialogs,
			Count:     len(payload.Dialogs),
			TabID:     tabID,
			Supported: true,
		}
		if payload.Armed != nil {
			result.Armed = &browser.DialogArmState{
				Accept:     payload.Armed.Accept,
				Remaining:  payload.Armed.Remaining,
				PromptText: payload.Armed.PromptText,
			}
		}
		return result, nil

	case "expect":
		accept, err := browser.DialogResponseAccepts(opts.Response)
		if err != nil {
			return browser.DialogResult{}, err
		}
		params["accept"] = accept
		if opts.Count > 0 {
			params["count"] = opts.Count
		}
		if opts.PromptText != "" {
			params["promptText"] = opts.PromptText
		}
		raw, err := b.call(ctx, "arm_dialog", params)
		if err != nil {
			if isUnknownMessageTypeErr(err) {
				return browser.DialogResult{Action: action, Dialogs: []browser.DialogRecord{}, Supported: false, TabID: tabID}, errors.New(dialogsUnsupportedNote)
			}
			return browser.DialogResult{}, err
		}
		var payload struct {
			Accept    bool `json:"accept"`
			Remaining int  `json:"remaining"`
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &payload)
		}
		return browser.DialogResult{
			Action:    "expect",
			Dialogs:   []browser.DialogRecord{},
			TabID:     tabID,
			Supported: true,
			Armed:     &browser.DialogArmState{Accept: payload.Accept, Remaining: payload.Remaining, PromptText: opts.PromptText},
		}, nil

	case "clear":
		params["clear"] = true
		if _, err := b.call(ctx, "arm_dialog", params); err != nil {
			if isUnknownMessageTypeErr(err) {
				return browser.DialogResult{Action: action, Dialogs: []browser.DialogRecord{}, Supported: false, TabID: tabID}, nil
			}
			return browser.DialogResult{}, err
		}
		return browser.DialogResult{Action: "clear", Dialogs: []browser.DialogRecord{}, TabID: tabID, Supported: true}, nil

	default:
		return browser.DialogResult{}, fmt.Errorf("unknown dialog action %q: use expect, status, or clear", opts.Action)
	}
}
