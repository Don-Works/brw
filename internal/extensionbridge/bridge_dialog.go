package extensionbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Don-Works/brw/internal/browser"
)

// dialogsUnsupportedNote is returned when the connected extension predates
// dialog arming. The extension has always ANSWERED dialogs (so the renderer
// never hangs on either build); what an old build lacks is the arm/observe
// surface, hence a graceful Supported=false rather than an error.
const dialogsUnsupportedNote = "Dialog control is unavailable: the connected brw extension predates brw_dialog support. Dialogs are still answered automatically (alert accepted, confirm/prompt given the non-destructive answer), but they cannot be pre-armed or listed. Reload the brw extension to enable it, or check supported=false to detect this programmatically"

// Dialog implements the browser.DialogController capability over the extension
// bridge. Arming is stored extension-side so the answer is already present when
// Page.javascriptDialogOpening fires, and the renderer is never held open across
// a daemon round trip.
func (b *Bridge) Dialog(ctx context.Context, opts browser.DialogOptions) (browser.DialogResult, error) {
	tabID := strings.TrimSpace(opts.TabID)
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
