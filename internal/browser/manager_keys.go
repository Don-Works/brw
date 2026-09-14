package browser

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Don-Works/brw/internal/actions"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// releaseAllKeyword releases every key a tab is holding in one call, so a flow
// that fails between KeyDown and KeyUp has one recovery that does not require
// remembering what it pressed.
const releaseAllKeyword = "all"

// KeyHoldOptions selects the key for a press-and-hold half.
type KeyHoldOptions struct {
	Key   string `json:"key"`
	TabID string `json:"tab_id,omitempty"`
}

// KeyHoldResult is an action result plus the keys the tab is still holding
// after it. Held is what tells a caller whether a modifier is still down —
// nothing in the page reports that, because a held modifier is brw state, not
// browser state.
type KeyHoldResult struct {
	ActionResult
	Key  string   `json:"key,omitempty"`
	Held []string `json:"held"`
}

// KeyDown presses a key WITHOUT releasing it, so the modifier stays applied to
// every later input event on that tab until KeyUp. This is what makes Ctrl+drag,
// Shift+click-range and modifier-held clicking possible: brw_press sends
// keydown and keyup back to back, which cannot express a held key, and a page
// that reads event.ctrlKey during a drag sees nothing.
func (m *Manager) KeyDown(ctx context.Context, opts KeyHoldOptions) (KeyHoldResult, error) {
	if err := m.guardTakeover("key_down"); err != nil {
		return KeyHoldResult{}, err
	}
	desc, err := describeHoldKey(opts.Key)
	if err != nil {
		return KeyHoldResult{}, err
	}
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return KeyHoldResult{}, err
	}
	defer cancel()

	before := m.cachedBefore(tabID, tabCtx)
	// Record the hold before dispatching so the keydown itself carries the
	// modifier bit a real Shift/Control keydown reports.
	m.holdKey(tabID, desc)
	mask := m.heldModifierMask(tabID)
	if err := m.runWithPrearmedSettle(tabCtx, actionSettleDelay, func() error {
		return chromedp.Run(tabCtx, chromedp.ActionFunc(func(c context.Context) error {
			return dispatchKeyHalf(c, desc, mask, true)
		}))
	}); err != nil {
		m.releaseKey(tabID, desc.Key)
		return KeyHoldResult{}, err
	}
	result := KeyHoldResult{
		ActionResult: m.observeActionWithBefore(tabID, tabCtx, "held "+desc.Key, before),
		Key:          desc.Key,
		Held:         m.heldKeyNames(tabID),
	}
	m.recordTrace(tabID, TraceEntry{
		Action:     "key_down",
		Value:      desc.Key,
		OK:         result.OK,
		Error:      result.Warning,
		DurationMS: result.DurationMS,
		Timestamp:  time.Now().Format(time.RFC3339),
	})
	return result, nil
}

// KeyUp releases a key held by KeyDown. Pass key "all" to release everything
// this tab is holding, in reverse press order.
func (m *Manager) KeyUp(ctx context.Context, opts KeyHoldOptions) (KeyHoldResult, error) {
	if err := m.guardTakeover("key_up"); err != nil {
		return KeyHoldResult{}, err
	}
	releaseAll := strings.EqualFold(strings.TrimSpace(opts.Key), releaseAllKeyword)
	var desc actions.KeyDescriptor
	if !releaseAll {
		var err error
		if desc, err = describeHoldKey(opts.Key); err != nil {
			return KeyHoldResult{}, err
		}
	}
	tabID, tabCtx, cancel, err := m.activeContext(ctx)
	if err != nil {
		return KeyHoldResult{}, err
	}
	defer cancel()

	release := []actions.KeyDescriptor{desc}
	if releaseAll {
		release = m.heldDescriptors(tabID)
		if len(release) == 0 {
			return KeyHoldResult{
				ActionResult: ActionResult{OK: true, Message: "no keys were held"},
				Held:         []string{},
			}, nil
		}
	}

	before := m.cachedBefore(tabID, tabCtx)
	if err := m.runWithPrearmedSettle(tabCtx, actionSettleDelay, func() error {
		return chromedp.Run(tabCtx, chromedp.ActionFunc(func(c context.Context) error {
			for _, d := range release {
				// Drop the key from the held set first: the keyup for Shift
				// reports shiftKey false, and any modifier still down stays in
				// the mask.
				m.releaseKey(tabID, d.Key)
				if err := dispatchKeyHalf(c, d, m.heldModifierMask(tabID), false); err != nil {
					return err
				}
			}
			return nil
		}))
	}); err != nil {
		return KeyHoldResult{}, err
	}

	message := "released " + desc.Key
	if releaseAll {
		message = fmt.Sprintf("released %d held key(s)", len(release))
	}
	result := KeyHoldResult{
		ActionResult: m.observeActionWithBefore(tabID, tabCtx, message, before),
		Key:          desc.Key,
		Held:         m.heldKeyNames(tabID),
	}
	m.recordTrace(tabID, TraceEntry{
		Action:     "key_up",
		Value:      desc.Key,
		OK:         result.OK,
		Error:      result.Warning,
		DurationMS: result.DurationMS,
		Timestamp:  time.Now().Format(time.RFC3339),
	})
	return result, nil
}

func describeHoldKey(raw string) (actions.KeyDescriptor, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return actions.KeyDescriptor{}, errors.New("key is required")
	}
	// A chord collapses to ONE descriptor carrying every bit: "ctrl+shift"
	// describes as key Shift with Ctrl|Shift set. Holding that would dispatch a
	// single Shift keydown the page never sees a Control keydown for, stamp
	// ctrlKey onto later mouse events anyway, and leave KeyUp "Control" with
	// nothing under that name to release. The held set is keyed by key name, so
	// one call must name one key.
	if strings.Contains(trimmed, "+") && utf8.RuneCountInString(trimmed) > 1 {
		return actions.KeyDescriptor{}, fmt.Errorf("hold one key at a time: %q is a chord, call key_down once per key", raw)
	}
	desc := actions.DescribeKey(trimmed)
	if desc.Key == "" {
		return actions.KeyDescriptor{}, fmt.Errorf("unrecognized key %q", raw)
	}
	return desc, nil
}

// dispatchKeyHalf sends one half of a key event. A keydown for a key with no
// text uses rawKeyDown for the same reason pressKey does: Chrome only performs
// native default actions from rawKeyDown.
func dispatchKeyHalf(ctx context.Context, desc actions.KeyDescriptor, modifiers int64, down bool) error {
	desc = actions.ApplyModifiers(desc, modifiers)
	eventType := input.KeyUp
	if down {
		eventType = input.KeyDown
		if desc.Text == "" {
			eventType = input.KeyRawDown
		}
	}
	event := input.DispatchKeyEvent(eventType).
		WithModifiers(input.Modifier(modifiers)).
		WithKey(desc.Key).
		WithCode(desc.Code).
		WithWindowsVirtualKeyCode(desc.WindowsVirtualKeyCode).
		WithNativeVirtualKeyCode(desc.WindowsVirtualKeyCode)
	if down && desc.Text != "" {
		event = event.WithText(desc.Text).WithUnmodifiedText(desc.Text)
	}
	return event.Do(ctx)
}

func (m *Manager) holdKey(tabID string, desc actions.KeyDescriptor) {
	m.heldMu.Lock()
	defer m.heldMu.Unlock()
	if m.heldKeys == nil {
		m.heldKeys = map[string]map[string]actions.KeyDescriptor{}
	}
	if m.heldKeys[tabID] == nil {
		m.heldKeys[tabID] = map[string]actions.KeyDescriptor{}
	}
	m.heldKeys[tabID][desc.Key] = desc
}

func (m *Manager) releaseKey(tabID, key string) {
	m.heldMu.Lock()
	defer m.heldMu.Unlock()
	if held := m.heldKeys[tabID]; held != nil {
		delete(held, key)
		if len(held) == 0 {
			delete(m.heldKeys, tabID)
		}
	}
}

// heldModifierMask is the modifier bitfield every input event on this tab must
// carry while keys are held.
func (m *Manager) heldModifierMask(tabID string) int64 {
	m.heldMu.Lock()
	defer m.heldMu.Unlock()
	var mask int64
	for _, desc := range m.heldKeys[tabID] {
		mask |= desc.Modifiers
	}
	return mask
}

func (m *Manager) heldKeyNames(tabID string) []string {
	m.heldMu.Lock()
	defer m.heldMu.Unlock()
	names := make([]string, 0, len(m.heldKeys[tabID]))
	for name := range m.heldKeys[tabID] {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// warnHeldKeys reports the keys a tab is still holding on an action result.
// Recovery is brw_key_up key="all", which the message names because a flow that
// lost track of its holds cannot name them itself.
func (m *Manager) warnHeldKeys(tabID string, result *ActionResult) {
	held := m.heldKeyNames(tabID)
	if len(held) == 0 {
		return
	}
	appendWarning(result, fmt.Sprintf("keys still held on this tab: %s — every later click, drag and keystroke carries them until key_up (key \"all\" releases everything)", strings.Join(held, "+")))
}

func (m *Manager) heldDescriptors(tabID string) []actions.KeyDescriptor {
	m.heldMu.Lock()
	defer m.heldMu.Unlock()
	held := m.heldKeys[tabID]
	names := make([]string, 0, len(held))
	for name := range held {
		names = append(names, name)
	}
	sort.Strings(names)
	descs := make([]actions.KeyDescriptor, 0, len(held))
	for _, name := range names {
		descs = append(descs, held[name])
	}
	return descs
}
