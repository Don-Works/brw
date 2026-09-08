package recipe

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// TraceAction is one entry of brw_trace, reduced to the fields a draft can use.
// It deliberately mirrors browser.TraceEntry's JSON rather than importing it:
// drafting is an offline transform over a saved trace, and must not drag the
// browser package into the CLI.
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
	OK                bool   `json:"ok"`
}

// DraftOptions parameterises a draft. Everything a human must decide is either
// asked for here or emitted as a TODO marker.
type DraftOptions struct {
	ID          string
	Version     string
	Name        string
	Description string
	Origins     []string
}

// TodoMarker is planted wherever the generator refuses to guess. It is not a
// valid value for any field it appears in, so `brwctl recipe validate` fails
// loudly until a human resolves it. That is the point: this removes typing,
// never review.
const TodoMarker = "TODO"

// writeActions are the browser actuations that must declare an effect.
var writeActions = map[string]bool{
	"click": true, "fill": true, "type": true,
	"select": true, "press": true, "navigate_to": true,
}

// sendish marks actions whose accessible name suggests they deliver something
// to a third party. A trace containing one is split into two drafts so a
// stored recipe cannot turn prior authorisation into standing permission.
var sendishNames = []string{"send", "post", "submit", "publish", "reply", "confirm", "pay", "delete"}

// DraftFromTrace converts a recorded trace into a schema-v1 recipe skeleton.
//
// It generates the mechanical parts — schema version, step ids, ordering,
// action mapping, and a stable Target for each step derived from the recorded
// role plus accessible name. It refuses to generate the judgement parts:
// every actuation gets effect TODO and a postcondition TODO, because a
// generator cannot know whether a click is a read or an external write, and a
// wrong guess there is exactly the failure the effect field exists to prevent.
//
// When the trace contains a send-shaped action, two drafts come back: a
// prepare draft ending before it, and a send draft containing it. Callers must
// keep them separate.
func DraftFromTrace(actions []TraceAction, opts DraftOptions) ([]Recipe, error) {
	usable := make([]TraceAction, 0, len(actions))
	for _, a := range actions {
		// A failed action is not evidence of a working flow.
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
				// The value was withheld from the trace because the field is
				// credential-bearing. Never invent one, and never inline one:
				// point at a declared input instead.
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
			// Element-kind events need a target of their own. Pre-fill it with
			// the step's, which is the overwhelmingly common case (assert on
			// the thing you just acted on) and is otherwise a second manual
			// edit for information the draft already has. A human choosing a
			// page- or url-kind event deletes it.
			if step.Target != nil {
				t := *step.Target
				step.Postcondition.Target = &t
			}
		}
		steps = append(steps, step)
	}
	return steps
}

// draftTarget turns a recorded ref into stable semantic identity. A ref is
// meaningful only against the page state that produced it, so it is never
// carried into a recipe; the role plus accessible name is what a runner can
// resolve again.
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
		// The element's own text carried its accessible name when the action
		// ran, so an exact-name match can be asserted and will pass.
		t.Name = name
	default:
		t.NameContains = name
	}
	return t
}

// splitAtSend divides a trace at the first send-shaped actuation, so message
// composition and delivery become separate reviewable recipes.
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
	out := make([]string, 0, len(seen))
	for o := range seen {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}
