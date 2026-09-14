package browser

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// Takeover is the handshake that lets a human drive the tab an agent is
// driving. Two actors want the same page, and a browser has no way to merge
// them: a click dispatched while a person is mid-form arrives as a click they
// did not make, on an element that may no longer be where the agent believed.
//
// So the two are mutually exclusive rather than interleaved. While a human
// holds takeover, every agent action that could reach that browser is REFUSED
// with an error that names the action and the hold, and the agent decides what
// to do about it. Refusing is deliberate: a queue would replay the agent's plan
// against a page the human has since changed, and a silent drop would have the
// agent believe it clicked.
//
// "Could reach that browser" is wider than the input verbs, and each of the
// three additions is a way the guard was escaped rather than a theory:
//
//   - A caller's own expression through brw_evaluate, which can click and type in
//     one string. Only the read scripts brw generates itself stay allowed, and
//     that is decided from the expression, never from a label a caller supplies.
//   - Every step of a plan or a batch, not only its first. A batch already in
//     flight when the human takes over would otherwise drive the page for the
//     whole of its remaining length.
//   - The tab verbs. Opening, focusing or closing moves or destroys the tab the
//     human is aiming at, so their next click lands somewhere they did not look.
//
// Reads are not refused. An agent that can still snapshot, read, get and wait is
// one that can tell what the human did, which is what it needs in order to
// resume. The hold is bound to one tab for the same reason it exists at all.
const (
	// TraceActionHumanInput labels a human's forwarded event in the trace, so
	// the activity feed shows who acted rather than presenting a person's click
	// as another anonymous step.
	TraceActionHumanInput = "human_input"

	defaultTakeoverTTL = 90 * time.Second
	maxTakeoverTTL     = 10 * time.Minute
)

// takeoverActionEvaluateScript is the guarded name for a caller-supplied
// expression. It is deliberately not the "evaluate" trace label: that label also
// covers the read scripts brw writes for brw_get and brw_frame, which stay
// allowed during a hold because refusing them would blind the agent.
const takeoverActionEvaluateScript = "evaluate_script"

// takeoverGuardedActions is the declared inventory of actions a hold refuses.
// Most of them change the page. The parity test reads it: a new action that
// records a trace entry must appear here or be declared an observation, and
// every name here must have a method that provably refuses.
var takeoverGuardedActions = map[string]bool{
	"click":        true,
	"click_text":   true,
	"click_button": true,
	"click_xy":     true,
	"drag":         true,
	"mouse_down":   true,
	"mouse_up":     true,
	"hover":        true,
	"type":         true,
	"fill":         true,
	"select":       true,
	"press":        true,
	"key_down":     true,
	"key_up":       true,
	"scroll":       true,
	"focus":        true,
	"clipboard":    true,
	"navigate":     true,
	"navigate_to":  true,
	"pushstate":    true,
	"upload_file":  true,
	"commit":       true,
	"plan":         true,
	"batch":        true,

	takeoverActionEvaluateScript: true,

	// The tab verbs are observations to a replayer — they record how a session
	// reached a page — but during a hold they move or destroy the tab the human
	// is aiming at, which is the same two-actor race a click is.
	TraceActionOpen:     true,
	TraceActionFocusTab: true,
	TraceActionCloseTab: true,
}

// takeoverReadOnlySteps are the batch and plan step verbs that only look at the
// page. They are step names rather than trace actions, so guardTakeover's
// fail-closed default would refuse them; a hold that stopped an agent waiting or
// asserting would stop it finding out what the human did.
var takeoverReadOnlySteps = map[string]bool{
	"wait":           true,
	"assert":         true,
	"assert_visible": true,
	"assert_text":    true,
	"assert_value":   true,
	"assert_hidden":  true,
}

// takeoverExemptActions change the page but are the human's own input, so they
// are the one thing takeover must not refuse.
var takeoverExemptActions = map[string]bool{
	TraceActionHumanInput: true,
}

// TakeoverGrant is what the holder keeps. The token is the whole authority:
// input carrying it is the human's, input without it is refused, and releasing
// needs it too so a second viewer cannot end someone else's hold.
type TakeoverGrant struct {
	Token  string `json:"token"`
	Holder string `json:"holder,omitempty"`
	// TabID is the tab the hold is bound to. Input is dispatched there rather
	// than to whatever is active at dispatch time, so an agent that moves the
	// active tab cannot redirect the human's next click into another page.
	TabID     string `json:"tab_id,omitempty"`
	GrantedAt string `json:"granted_at"`
	ExpiresAt string `json:"expires_at"`
}

// TakeoverStatus is the public view of the hold: no token, because everyone who
// can see the dashboard can read this and only the holder may act.
type TakeoverStatus struct {
	Held      bool   `json:"held"`
	Holder    string `json:"holder,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// TakeoverRefusedCode is the stable machine-readable name for this refusal. It
// travels as the "code" field of the HTTP error body and as the usage log's
// error class, so an agent on any transport can tell "a human has the browser,
// back off" from "your snapshot is stale, re-read" without reading prose.
const TakeoverRefusedCode = "takeover_held"

// TakeoverRefusedError is the named refusal an agent gets for an action a human
// currently owns. Named rather than a bare string so a caller can branch on it
// with errors.As instead of matching on prose.
type TakeoverRefusedError struct {
	Action    string
	Holder    string
	ExpiresAt string
}

func (e *TakeoverRefusedError) Error() string {
	holder := e.Holder
	if holder == "" {
		holder = "a human"
	}
	return fmt.Sprintf(
		"%s refused: %s holds takeover of this browser until %s; read the page to see what changed, then retry once the hold is released",
		e.Action, holder, e.ExpiresAt)
}

// ErrTakeoverNotHeld is returned when input or a release arrives without a live
// grant. It is the same answer for "never enabled" and "expired", because the
// caller's next step is identical.
var ErrTakeoverNotHeld = errors.New("takeover is not enabled for this session: acquire it before forwarding input")

// ErrTakeoverBusy is returned when a second viewer asks for a hold someone else
// already has. Two humans racing each other is the same failure as a human
// racing an agent.
var ErrTakeoverBusy = errors.New("takeover is already held by another session")

// TakeoverInput is one forwarded event. It is deliberately the CDP Input
// vocabulary rather than a DOM event: the point of takeover is that the human's
// click is indistinguishable, at the renderer, from the agent's.
type TakeoverInput struct {
	Kind                  string  `json:"kind"`
	Type                  string  `json:"type"`
	X                     float64 `json:"x,omitempty"`
	Y                     float64 `json:"y,omitempty"`
	Button                string  `json:"button,omitempty"`
	Buttons               int64   `json:"buttons,omitempty"`
	ClickCount            int64   `json:"click_count,omitempty"`
	DeltaX                float64 `json:"delta_x,omitempty"`
	DeltaY                float64 `json:"delta_y,omitempty"`
	Modifiers             int64   `json:"modifiers,omitempty"`
	Key                   string  `json:"key,omitempty"`
	Code                  string  `json:"code,omitempty"`
	Text                  string  `json:"text,omitempty"`
	WindowsVirtualKeyCode int64   `json:"windows_virtual_key_code,omitempty"`
}

// maxTakeoverTextBytes bounds one forwarded keystroke's text. A keypress is a
// character or two; anything larger is a paste being smuggled through the input
// channel, and it would reach the page as trusted keyboard input.
const maxTakeoverTextBytes = 8

func (t TakeoverInput) validate() error {
	switch strings.ToLower(strings.TrimSpace(t.Kind)) {
	case "mouse":
		switch t.Type {
		case "mousePressed", "mouseReleased", "mouseMoved", "mouseWheel":
		default:
			return fmt.Errorf("unsupported mouse event type %q", t.Type)
		}
	case "key":
		switch t.Type {
		case "keyDown", "keyUp", "char", "rawKeyDown":
		default:
			return fmt.Errorf("unsupported key event type %q", t.Type)
		}
		if len(t.Text) > maxTakeoverTextBytes {
			return errors.New("key event text is too long for a single keystroke")
		}
	default:
		return fmt.Errorf("unsupported takeover input kind %q", t.Kind)
	}
	return nil
}

// AcquireTakeover grants the hold to one session. holder is a human-readable
// label for the feed; it carries no authority, the token does.
func (m *Manager) AcquireTakeover(holder string, ttl time.Duration) (TakeoverGrant, error) {
	ttl = boundTakeoverTTL(ttl)
	now := time.Now()

	m.takeoverMu.Lock()
	defer m.takeoverMu.Unlock()
	if m.takeoverToken != "" && now.Before(m.takeoverExpiry) {
		return TakeoverGrant{}, ErrTakeoverBusy
	}
	token, err := newTakeoverToken()
	if err != nil {
		return TakeoverGrant{}, err
	}
	m.takeoverToken = token
	m.takeoverHolder = strings.TrimSpace(holder)
	m.takeoverExpiry = now.Add(ttl)
	// Bind the hold to the tab that is active now. The human aimed at the tab on
	// their screen; resolving the target again at dispatch time would send their
	// click wherever the browser had drifted to since. A Manager with no ref
	// store yet has no active tab to name; the first forwarded event pins it.
	if m.refs != nil {
		m.takeoverTab = m.refs.Active()
	}
	return TakeoverGrant{
		Token:     token,
		Holder:    m.takeoverHolder,
		TabID:     m.takeoverTab,
		GrantedAt: now.UTC().Format(time.RFC3339),
		ExpiresAt: m.takeoverExpiry.UTC().Format(time.RFC3339),
	}, nil
}

// RenewTakeover extends a live hold. The dashboard heartbeats through it, which
// is what releases the browser when a viewer closes the tab without saying so:
// a hold nobody is renewing expires and the agent resumes.
func (m *Manager) RenewTakeover(token string, ttl time.Duration) (TakeoverGrant, error) {
	ttl = boundTakeoverTTL(ttl)
	now := time.Now()

	m.takeoverMu.Lock()
	defer m.takeoverMu.Unlock()
	if !m.takeoverTokenValidLocked(token, now) {
		return TakeoverGrant{}, ErrTakeoverNotHeld
	}
	m.takeoverExpiry = now.Add(ttl)
	return TakeoverGrant{
		Token:     m.takeoverToken,
		Holder:    m.takeoverHolder,
		TabID:     m.takeoverTab,
		GrantedAt: now.UTC().Format(time.RFC3339),
		ExpiresAt: m.takeoverExpiry.UTC().Format(time.RFC3339),
	}, nil
}

// ReleaseTakeover ends the hold and lets agent actions through again.
//
// It waits for any human event already on its way to the renderer. Releasing
// out from under one would put the human's click on a page the agent had just
// been told it may drive again, which is the two-actor race running backwards.
func (m *Manager) ReleaseTakeover(token string) error {
	m.takeoverDispatchMu.Lock()
	defer m.takeoverDispatchMu.Unlock()
	m.takeoverMu.Lock()
	defer m.takeoverMu.Unlock()
	if !m.takeoverTokenValidLocked(token, time.Now()) {
		return ErrTakeoverNotHeld
	}
	m.takeoverToken = ""
	m.takeoverHolder = ""
	m.takeoverTab = ""
	m.takeoverExpiry = time.Time{}
	return nil
}

// TakeoverState reports whether a human currently holds the browser.
func (m *Manager) TakeoverState() TakeoverStatus {
	m.takeoverMu.Lock()
	defer m.takeoverMu.Unlock()
	if m.takeoverToken == "" || !time.Now().Before(m.takeoverExpiry) {
		return TakeoverStatus{}
	}
	return TakeoverStatus{
		Held:      true,
		Holder:    m.takeoverHolder,
		ExpiresAt: m.takeoverExpiry.UTC().Format(time.RFC3339),
	}
}

// guardTakeover is the refusal every agent input action goes through. It is the
// first statement in each of those methods rather than a check inside
// activeContext, because activeContext also serves reads and screenshots — and
// a dashboard that goes blank the moment a human takes over is a dashboard
// nobody can take over from.
func (m *Manager) guardTakeover(action string) error {
	// Fails closed: anything that is not a declared observation is treated as
	// input. A mistyped action name at a call site then still refuses, rather
	// than becoming the one input path takeover does not cover. The guarded set
	// wins over the observation set, because the tab verbs are in both.
	if takeoverExemptActions[action] {
		return nil
	}
	if observationActions[action] && !takeoverGuardedActions[action] {
		return nil
	}
	m.takeoverMu.Lock()
	defer m.takeoverMu.Unlock()
	if m.takeoverToken == "" || !time.Now().Before(m.takeoverExpiry) {
		return nil
	}
	return &TakeoverRefusedError{
		Action:    action,
		Holder:    m.takeoverHolder,
		ExpiresAt: m.takeoverExpiry.UTC().Format(time.RFC3339),
	}
}

// guardTakeoverStep is guardTakeover for one step of a batch or a plan. Step
// verbs are not trace actions, so the read-only ones have to be named here or
// the fail-closed default would refuse an agent's wait and assert steps too.
func (m *Manager) guardTakeoverStep(action string) error {
	if takeoverReadOnlySteps[action] {
		return nil
	}
	return m.guardTakeover(action)
}

// DispatchTakeoverInput forwards one human event to the held tab. The token
// is checked HERE, not only at the HTTP edge: this is the method that reaches
// the renderer, so it is the one that has to be unreachable without a grant.
func (m *Manager) DispatchTakeoverInput(ctx context.Context, token string, event TakeoverInput) error {
	if err := event.validate(); err != nil {
		return err
	}
	// Read-held for the whole dispatch; ReleaseTakeover takes it for write. The
	// token check and the CDP call are then one step as far as a release is
	// concerned, rather than a window a release can land in.
	m.takeoverDispatchMu.RLock()
	defer m.takeoverDispatchMu.RUnlock()

	m.takeoverMu.Lock()
	valid := m.takeoverTokenValidLocked(token, time.Now())
	pinned := m.takeoverTab
	m.takeoverMu.Unlock()
	if !valid {
		return ErrTakeoverNotHeld
	}

	start := time.Now()
	// The tab the hold was taken on, not whichever one is active now.
	tabID, tabCtx, cancel, err := m.contextForTab(ctx, pinned)
	if err != nil {
		return err
	}
	defer cancel()
	if pinned == "" {
		m.bindTakeoverTab(token, tabID)
	}

	err = chromedp.Run(tabCtx, chromedp.ActionFunc(func(c context.Context) error {
		return dispatchTakeoverEvent(c, event)
	}))
	// The hold can still expire mid-dispatch, which no mutex can hold off. Say
	// so rather than report the event as delivered under a hold that had already
	// ended: the caller's next act is to stop forwarding and re-acquire.
	if err == nil {
		m.takeoverMu.Lock()
		stillHeld := m.takeoverTokenValidLocked(token, time.Now())
		m.takeoverMu.Unlock()
		if !stillHeld {
			err = ErrTakeoverNotHeld
		}
	}
	// A human's input belongs in the same feed the agent's actions appear in.
	// Without it the page changes under the operator's own hands with nothing
	// in the record saying which of the two actors did it.
	m.recordTrace(tabID, NewObservationTrace(TraceActionHumanInput, takeoverInputLabel(event), start, err))
	return err
}

// bindTakeoverTab pins a grant that was taken before any tab existed to the
// first tab it reached, so every later event goes to that same tab.
func (m *Manager) bindTakeoverTab(token, tabID string) {
	if tabID == "" {
		return
	}
	m.takeoverMu.Lock()
	defer m.takeoverMu.Unlock()
	if m.takeoverTab == "" && m.takeoverTokenValidLocked(token, time.Now()) {
		m.takeoverTab = tabID
	}
}

func dispatchTakeoverEvent(ctx context.Context, event TakeoverInput) error {
	if strings.EqualFold(strings.TrimSpace(event.Kind), "mouse") {
		call := input.DispatchMouseEvent(input.MouseType(event.Type), event.X, event.Y).
			WithButton(normalizeMouseButton(event.Button)).
			WithButtons(event.Buttons).
			WithModifiers(input.Modifier(event.Modifiers))
		if event.ClickCount > 0 {
			call = call.WithClickCount(event.ClickCount)
		}
		if event.Type == "mouseWheel" {
			call = call.WithDeltaX(event.DeltaX).WithDeltaY(event.DeltaY)
		}
		return call.Do(ctx)
	}
	return input.DispatchKeyEvent(input.KeyType(event.Type)).
		WithKey(event.Key).
		WithCode(event.Code).
		WithText(event.Text).
		WithUnmodifiedText(event.Text).
		WithWindowsVirtualKeyCode(event.WindowsVirtualKeyCode).
		WithNativeVirtualKeyCode(event.WindowsVirtualKeyCode).
		WithModifiers(input.Modifier(event.Modifiers)).
		Do(ctx)
}

// takeoverInputLabel describes a forwarded event for the feed WITHOUT its text.
// A human types passwords through takeover; the trace is served over the
// control plane, so the keystroke's character never goes in it.
func takeoverInputLabel(event TakeoverInput) string {
	if strings.EqualFold(strings.TrimSpace(event.Kind), "mouse") {
		return fmt.Sprintf("human %s at (%.0f,%.0f)", event.Type, event.X, event.Y)
	}
	key := strings.TrimSpace(event.Key)
	if key == "" || len([]rune(key)) == 1 {
		// A single-rune key IS the character typed. Report the event only.
		return "human " + event.Type
	}
	return "human " + event.Type + " " + key
}

// takeoverTokenValidLocked reports whether token is the live grant. Compared in
// constant time: the token is the only thing standing between a page the
// operator visits and the ability to drive their signed-in browser.
func (m *Manager) takeoverTokenValidLocked(token string, now time.Time) bool {
	if m.takeoverToken == "" || strings.TrimSpace(token) == "" {
		return false
	}
	if !now.Before(m.takeoverExpiry) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(m.takeoverToken), []byte(token)) == 1
}

func boundTakeoverTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return defaultTakeoverTTL
	}
	if ttl > maxTakeoverTTL {
		return maxTakeoverTTL
	}
	return ttl
}

func newTakeoverToken() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
