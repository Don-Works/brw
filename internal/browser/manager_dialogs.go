package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// maxDialogRecords bounds the per-tab answered-dialog ring. Dialogs are rare
// compared with console lines, so a short ring answers "did anything pop up
// during my last few actions?" without unbounded growth.
const maxDialogRecords = 20

// maxArmedDialogs caps how many dialogs a single arm may answer, so a stray
// arm cannot silently rubber-stamp an unbounded run of prompts.
const maxArmedDialogs = 50

// DialogOptions selects one brw_dialog operation.
//
// Action is one of:
//
//	expect — pre-declare the answer for the next Count dialog(s) on the tab
//	status — read (and by default consume) the answered-dialog ring
//	clear  — discard a pending arm
type DialogOptions struct {
	Action     string `json:"action"`
	Response   string `json:"response"`
	PromptText string `json:"prompt_text"`
	Count      int    `json:"count"`
	Peek       bool   `json:"peek"`
	TabID      string `json:"tab_id"`
}

// DialogRecord is one JavaScript dialog that was opened and answered.
type DialogRecord struct {
	Type          string `json:"type"`
	Message       string `json:"message"`
	DefaultPrompt string `json:"default_prompt,omitempty"`
	URL           string `json:"url,omitempty"`
	Accepted      bool   `json:"accepted"`
	PromptText    string `json:"prompt_text,omitempty"`
	// DecidedBy records which rule answered: armed (a caller pre-declared it),
	// agent_acting (brw was driving the tab), or user_safe_default (the
	// non-destructive choice).
	DecidedBy string `json:"decided_by"`
	At        string `json:"at"`
}

// DialogArmState reports an outstanding pre-declared answer.
type DialogArmState struct {
	Accept     bool   `json:"accept"`
	Remaining  int    `json:"remaining"`
	PromptText string `json:"prompt_text,omitempty"`
}

// DialogResult is the brw_dialog reply.
type DialogResult struct {
	Action    string          `json:"action"`
	Armed     *DialogArmState `json:"armed,omitempty"`
	Dialogs   []DialogRecord  `json:"dialogs"`
	Count     int             `json:"count"`
	TabID     string          `json:"tab_id,omitempty"`
	Supported bool            `json:"supported"`
}

type dialogArm struct {
	accept     bool
	promptText string
	hasPrompt  bool
	remaining  int
}

// dialogState is a VALUE on Manager, not a pointer, and its maps are created on
// first use under the lock. Manager is constructed in more than one place (New,
// plus test harnesses that list fields by hand), so a state that needs a
// constructor call would be nil in some of them. Lazy init makes the zero value
// correct everywhere.
type dialogState struct {
	mu  sync.Mutex
	arm map[string]*dialogArm
	log map[string][]DialogRecord
}

// initLocked must be called with mu held.
func (d *dialogState) initLocked() {
	if d.arm == nil {
		d.arm = make(map[string]*dialogArm)
	}
	if d.log == nil {
		d.log = make(map[string][]DialogRecord)
	}
}

// handleDialogEvent answers and records one JavaScript dialog. It is called from
// the tab's single event subscription (see eventHub.attachTab), which is why
// there is no per-tab arming step and no feature flag.
//
// Answering is NOT optional. chromedp enables the Page domain on every target it
// attaches, and an enabled Page domain suppresses Chrome's native dialog UI: the
// dialog arrives as Page.javascriptDialogOpening and the renderer BLOCKS until
// Page.handleJavaScriptDialog answers it. With nothing answering, a single
// alert() wedges the tab permanently — the triggering action times out and so
// does every later evaluate against that tab.
func (m *Manager) handleDialogEvent(tabID string, tabCtx context.Context, ev any) {
	opening, ok := ev.(*page.EventJavascriptDialogOpening)
	if !ok {
		return
	}
	accept, promptText, hasPrompt, decidedBy := m.decideDialog(tabID, string(opening.Type))
	m.recordDialog(tabID, DialogRecord{
		Type:          string(opening.Type),
		Message:       clipDialogText(opening.Message),
		DefaultPrompt: clipDialogText(opening.DefaultPrompt),
		URL:           clipDialogText(opening.URL),
		Accepted:      accept,
		PromptText:    promptText,
		DecidedBy:     decidedBy,
		At:            time.Now().UTC().Format(time.RFC3339Nano),
	})
	// A CDP command must not be issued from inside the event loop that delivered
	// the event, so answer from a goroutine. The renderer stays blocked only for
	// this hop.
	go func() {
		action := page.HandleJavaScriptDialog(accept)
		if hasPrompt && opening.Type == page.DialogTypePrompt {
			action = action.WithPromptText(promptText)
		}
		answerCtx, cancel := context.WithTimeout(tabCtx, 5*time.Second)
		defer cancel()
		_ = chromedp.Run(answerCtx, action)
	}()
}

// decideDialog resolves the answer for one dialog, consuming a pending arm.
//
// Without an arm the default is deliberately the NON-DESTRUCTIVE choice rather
// than a blanket accept: alert has only an OK button, and a beforeunload fires
// because brw is navigating, but auto-confirming an arbitrary confirm()/prompt()
// would rubber-stamp "Delete this account?". A caller that wants the other
// answer pre-declares it with brw_dialog action:"expect".
func (m *Manager) decideDialog(tabID, dialogType string) (accept bool, promptText string, hasPrompt bool, decidedBy string) {
	m.dialogs.mu.Lock()
	defer m.dialogs.mu.Unlock()
	m.dialogs.initLocked()
	if arm := m.dialogs.arm[tabID]; arm != nil && arm.remaining > 0 {
		arm.remaining--
		if arm.remaining <= 0 {
			delete(m.dialogs.arm, tabID)
		}
		return arm.accept, arm.promptText, arm.hasPrompt, "armed"
	}
	switch dialogType {
	case string(page.DialogTypeAlert), string(page.DialogTypeBeforeunload):
		return true, "", false, "agent_acting"
	default:
		return false, "", false, "user_safe_default"
	}
}

func (m *Manager) recordDialog(tabID string, record DialogRecord) {
	m.dialogs.mu.Lock()
	defer m.dialogs.mu.Unlock()
	m.dialogs.initLocked()
	entries := append(m.dialogs.log[tabID], record)
	if len(entries) > maxDialogRecords {
		entries = entries[len(entries)-maxDialogRecords:]
	}
	m.dialogs.log[tabID] = entries
}

func clipDialogText(s string) string {
	const limit = 2000
	if len(s) > limit {
		return s[:limit]
	}
	return s
}

// Dialog implements the DialogController capability for the direct-CDP transport.
func (m *Manager) Dialog(ctx context.Context, opts DialogOptions) (DialogResult, error) {
	tabID := strings.TrimSpace(opts.TabID)
	if tabID == "" {
		active, err := m.ensureActive(ctx)
		if err != nil {
			return DialogResult{}, err
		}
		tabID = active
	}
	// Touching the tab context is what guarantees the listener is installed, so
	// a caller that arms before the first dialog is never racing it.
	if _, err := m.tabContext(tabID); err != nil {
		return DialogResult{}, err
	}

	switch strings.ToLower(strings.TrimSpace(opts.Action)) {
	case "", "status":
		m.dialogs.mu.Lock()
		m.dialogs.initLocked()
		entries := m.dialogs.log[tabID]
		if !opts.Peek {
			delete(m.dialogs.log, tabID)
		}
		armed := armStateLocked(m.dialogs.arm[tabID])
		m.dialogs.mu.Unlock()
		if entries == nil {
			entries = []DialogRecord{}
		}
		return DialogResult{Action: "status", Dialogs: entries, Count: len(entries), Armed: armed, TabID: tabID, Supported: true}, nil

	case "expect":
		accept, err := DialogResponseAccepts(opts.Response)
		if err != nil {
			return DialogResult{}, err
		}
		count := opts.Count
		if count <= 0 {
			count = 1
		}
		if count > maxArmedDialogs {
			count = maxArmedDialogs
		}
		arm := &dialogArm{accept: accept, remaining: count}
		if opts.PromptText != "" {
			arm.promptText = opts.PromptText
			arm.hasPrompt = true
		}
		m.dialogs.mu.Lock()
		m.dialogs.initLocked()
		m.dialogs.arm[tabID] = arm
		armed := armStateLocked(arm)
		m.dialogs.mu.Unlock()
		return DialogResult{Action: "expect", Armed: armed, Dialogs: []DialogRecord{}, TabID: tabID, Supported: true}, nil

	case "clear":
		m.dialogs.mu.Lock()
		m.dialogs.initLocked()
		delete(m.dialogs.arm, tabID)
		m.dialogs.mu.Unlock()
		return DialogResult{Action: "clear", Dialogs: []DialogRecord{}, TabID: tabID, Supported: true}, nil

	default:
		return DialogResult{}, fmt.Errorf("unknown dialog action %q: use expect, status, or clear", opts.Action)
	}
}

func armStateLocked(arm *dialogArm) *DialogArmState {
	if arm == nil || arm.remaining <= 0 {
		return nil
	}
	return &DialogArmState{Accept: arm.accept, Remaining: arm.remaining, PromptText: arm.promptText}
}

// DialogResponseAccepts maps the caller-facing response word to the CDP accept
// boolean. Exported so every transport validates the same vocabulary.
func DialogResponseAccepts(response string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(response)) {
	case "accept":
		return true, nil
	case "dismiss":
		return false, nil
	case "":
		return false, errors.New("dialog action \"expect\" requires response: accept or dismiss")
	default:
		return false, fmt.Errorf("unknown dialog response %q: use accept or dismiss", response)
	}
}
