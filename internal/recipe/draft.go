package recipe

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/Don-Works/brw/internal/credential"
)

// TraceAction is one entry of brw_trace, reduced to the fields a draft can use.
type TraceAction struct {
	Action            string `json:"action"`
	Ref               string `json:"ref,omitempty"`
	Text              string `json:"text,omitempty"`
	Value             string `json:"value,omitempty"`
	Name              string `json:"name,omitempty"`
	Role              string `json:"role,omitempty"`
	URL               string `json:"url,omitempty"`
	NameIsVisibleText bool   `json:"name_is_visible_text,omitempty"`
	Redacted          bool   `json:"redacted,omitempty"`
	CredentialSourced bool   `json:"credential_sourced,omitempty"`
	OK                bool   `json:"ok"`
}

// ErrCredentialSourcedAction is the compile failure for a recorded action whose typed value came from a credential provider.
var ErrCredentialSourcedAction = errors.New("trace action typed a value that came from a credential provider")

// GuardTraceActionForCompilation is the refusal every trace-to-recipe compilation must apply once per recorded action before emitting a step.
func GuardTraceActionForCompilation(index int, action TraceAction) error {
	if !action.CredentialSourced {
		return nil
	}
	return fmt.Errorf("action %d (%s): %w: write the step by hand with value %s<name>",
		index+1, action.Action, ErrCredentialSourcedAction, credential.Scheme)
}

// DraftOptions parameterises a draft.
type DraftOptions struct {
	ID          string
	Version     string
	Name        string
	Description string
	Origins     []string
}

// TodoMarker is planted wherever the generator refuses to guess.
const TodoMarker = "TODO"

var writeActions = map[string]bool{
	"click": true, "fill": true, "type": true,
	"select": true, "press": true, "navigate_to": true,
}

var sendishNames = []string{"send", "post", "submit", "publish", "reply", "confirm", "pay", "delete"}

// DraftFromTrace converts a recorded trace into a schema-v1 recipe skeleton.
func DraftFromTrace(actions []TraceAction, opts DraftOptions) ([]Recipe, error) {
	usable := make([]TraceAction, 0, len(actions))
	for index, a := range actions {

		if err := GuardTraceActionForCompilation(index, a); err != nil {
			return nil, err
		}

		if !a.OK {
			continue
		}
		if strings.TrimSpace(a.Action) == "" {
			continue
		}
		usable = append(usable, a)
	}
	if len(usable) == 0 {
		return nil, errors.New("trace contains no successful actions to draft from")
	}
	if strings.TrimSpace(opts.ID) == "" {
		return nil, errors.New("draft requires an --id like google.chat.search-conversations")
	}
	if !recipeIDPattern.MatchString(opts.ID) {
		return nil, fmt.Errorf("draft id %q must be dotted lowercase, e.g. google.chat.search-conversations", opts.ID)
	}
	version := strings.TrimSpace(opts.Version)
	if version == "" {
		version = "1.0.0"
	}
	if !versionPattern.MatchString(version) {
		return nil, fmt.Errorf("draft version %q must be major.minor.patch", version)
	}

	origins := normaliseOrigins(opts.Origins, usable)
	if len(origins) == 0 {
		origins = []string{TodoMarker + ":origin, e.g. https://mail.google.com"}
	}

	split := splitAtSend(usable)
	drafts := make([]Recipe, 0, len(split))
	for i, group := range split {
		id, name := opts.ID, opts.Name
		if len(split) > 1 {
			suffix := []string{"prepare", "send"}[i]
			id = opts.ID + "-" + suffix
			if name != "" {
				name = name + " (" + suffix + ")"
			}
		}
		if name == "" {
			name = TodoMarker + ": human-readable name"
		}
		description := opts.Description
		if description == "" {
			description = TodoMarker + ": what this does and when to use it"
		}
		drafts = append(drafts, Recipe{
			SchemaVersion: 1,
			ID:            id,
			Version:       version,
			Name:          name,
			Description:   description,
			Intents:       []string{TodoMarker + ": one phrasing an agent would search for"},
			Origins:       origins,
			Risk:          TodoMarker + ": read_only or external_write",
			Steps:         draftSteps(group),
			Metadata:      map[string]string{"drafted_from": "brw_trace"},
		})
	}
	return drafts, nil
}

func draftSteps(actions []TraceAction) []Step {
	steps := make([]Step, 0, len(actions))
	for i, a := range actions {
		step := Step{
			ID:     fmt.Sprintf("s%d", i+1),
			Action: a.Action,
		}
		if a.Action == "navigate_to" && a.URL != "" {
			step.URL = a.URL
		}
		if target := draftTarget(a); target != nil {
			step.Target = target
		}
		switch a.Action {
		case "fill", "type":
			if a.Redacted {

				step.Value = "${input:" + TodoMarker + "_secret_input}"
			} else if a.Text != "" {
				step.Value = a.Text
			}
		case "press":
			step.Key = a.Text
		}
		if writeActions[a.Action] {
			step.Effect = TodoMarker + ": read or external_write"
			step.Postcondition = &Event{
				Kind:      TodoMarker + ": page.ready | text.present | element.value | url.match | download.completed",
				TimeoutMS: 15000,
			}

			if step.Target != nil {
				t := *step.Target
				step.Postcondition.Target = &t
			}
		}
		steps = append(steps, step)
	}
	return steps
}

func draftTarget(a TraceAction) *Target {
	role := strings.TrimSpace(a.Role)
	name := strings.TrimSpace(a.Name)
	if role == "" && name == "" {
		return nil
	}
	t := &Target{Role: role}
	if role == "" {
		t.Role = TodoMarker + ": role"
	}
	switch {
	case name == "":
		t.Name = TodoMarker + ": exact accessible name, test id, or href fragment"
	case a.NameIsVisibleText:

		t.Name = name
	default:
		t.NameContains = name
	}
	return t
}

func splitAtSend(actions []TraceAction) [][]TraceAction {
	for i, a := range actions {
		if !writeActions[a.Action] {
			continue
		}
		haystack := strings.ToLower(a.Name + " " + a.Text)
		for _, needle := range sendishNames {
			if strings.Contains(haystack, needle) {
				if i == 0 {
					return [][]TraceAction{actions}
				}
				return [][]TraceAction{actions[:i], actions[i:]}
			}
		}
	}
	return [][]TraceAction{actions}
}

func normaliseOrigins(explicit []string, actions []TraceAction) []string {
	seen := map[string]struct{}{}
	for _, o := range explicit {
		if o = strings.TrimSpace(o); o != "" {
			seen[strings.TrimRight(o, "/")] = struct{}{}
		}
	}
	for _, a := range actions {
		if a.URL == "" {
			continue
		}
		u, err := url.Parse(a.URL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			continue
		}
		seen[u.Scheme+"://"+u.Host] = struct{}{}
	}
	return slices.Sorted(maps.Keys(seen))
}
