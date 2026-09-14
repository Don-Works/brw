package browser

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"github.com/Don-Works/brw/internal/snapshot"
)

const noSemanticStateChangeWarning = "action dispatched but no observable semantic state change"

type SemanticState struct {
	URL   string
	Title string
	Focus string
	// Signature is a digest of every element's semantic fields, never those
	// fields themselves. Equality is its only use, and Manager.storeState keeps
	// the post-action state of every tab until that tab navigates: holding the
	// raw values would park whatever was typed into a form — a credential a
	// recipe resolved among them — in the daemon's heap for the life of the tab.
	Signature string
}

func (s SemanticState) Equal(other SemanticState) bool {
	return s.URL == other.URL && s.Title == other.Title && s.Focus == other.Focus && s.Signature == other.Signature
}

// AdvanceObservationState returns the state/version for a change detector. The
// version advances only when semantic state changes, so clients can compare it
// cheaply; the first observation establishes version 1.
func AdvanceObservationState(previous *SemanticState, version int64, after SemanticState) (*SemanticState, int64, bool) {
	if previous != nil && previous.Equal(after) {
		return previous, version, false
	}
	next := after
	return &next, version + 1, true
}

func NewSemanticState(snap snapshot.PageSnapshot) SemanticState {
	focus := ""
	if snap.Metadata != nil {
		if value, ok := snap.Metadata["focused_ref"].(string); ok {
			focus = value
		}
	}
	parts := make([]string, 0, len(snap.Elements))
	for _, el := range snap.Elements {
		selected := ""
		if el.Selected != nil {
			selected = strconv.FormatBool(*el.Selected)
		}
		checked := ""
		if el.Checked != nil {
			checked = strconv.FormatBool(*el.Checked)
		}
		expanded := ""
		if el.Expanded != nil {
			expanded = strconv.FormatBool(*el.Expanded)
		}
		parts = append(parts, strings.Join([]string{
			el.Ref,
			el.Role,
			el.Name,
			el.Value,
			selected,
			checked,
			expanded,
			strconv.FormatBool(el.Visible),
			strconv.FormatBool(el.Disabled),
			strings.Join(el.Signals, ","),
		}, "\x1f"))
	}
	sort.Strings(parts)
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x1e")))
	return SemanticState{
		URL:       snap.URL,
		Title:     snap.Title,
		Focus:     focus,
		Signature: hex.EncodeToString(digest[:]),
	}
}

func ApplyStateDiff(result *ActionResult, before *SemanticState, after SemanticState) {
	if before == nil {
		return
	}
	changed := !before.Equal(after)
	result.ChangedState = &changed
	if !changed {
		appendWarning(result, noSemanticStateChangeWarning)
	}
}

func appendWarning(result *ActionResult, warning string) {
	warning = strings.TrimSpace(warning)
	if result == nil || warning == "" {
		return
	}
	if result.Warning == "" {
		result.Warning = warning
		return
	}
	if strings.Contains(result.Warning, warning) {
		return
	}
	result.Warning += "; " + warning
}
