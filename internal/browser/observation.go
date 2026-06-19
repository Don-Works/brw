package browser

import (
	"sort"
	"strconv"
	"strings"

	"github.com/Don-Works/brw/internal/snapshot"
)

const noSemanticStateChangeWarning = "action dispatched but no observable semantic state change"

type SemanticState struct {
	URL       string
	Title     string
	Focus     string
	Signature string
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
	return SemanticState{
		URL:       snap.URL,
		Title:     snap.Title,
		Focus:     focus,
		Signature: strings.Join(parts, "\x1e"),
	}
}

func ApplyStateDiff(result *ActionResult, before *SemanticState, after SemanticState) {
	if before == nil {
		return
	}
	changed := before.URL != after.URL ||
		before.Title != after.Title ||
		before.Focus != after.Focus ||
		before.Signature != after.Signature
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
