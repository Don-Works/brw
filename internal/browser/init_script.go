package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// InitScriptController is the optional transport capability for scripts that run
// before every new document. It is separate from Controller for the same reason
// EnvironmentController is: a lane without it degrades to a named capability
// error instead of failing to compile.
type InitScriptController interface {
	InitScript(context.Context, InitScriptOptions) (InitScriptResult, error)
}

// ErrInitScriptUnsupported is returned by transports that cannot hold an init
// script registration.
var ErrInitScriptUnsupported = errors.New("init scripts are not supported on the extension-bridge transport: Page.addScriptToEvaluateOnNewDocument lives on the debugger session, which the extension attaches and detaches per operation, so a registered script would not survive to the next navigation; use a direct-CDP profile")

// InitScript is one script brw has registered to run before every new document
// in a tab. CDP returns an opaque identifier when the script is added and that
// identifier — not the source — is what removes it again, so the pair is kept
// per tab.
type InitScript struct {
	// ID is Chrome's ScriptIdentifier, which is only meaningful to the debugger
	// session that minted it.
	ID string `json:"id"`
	// Bytes is the source length, so a caller can see the size without the
	// source being echoed back into every listing.
	Bytes int `json:"bytes"`
	// Preview is a bounded, single-line excerpt for a human reading brw_list.
	Preview string `json:"preview,omitempty"`
}

// InitScriptOptions adds, removes, lists or clears the scripts registered to run
// before a new document loads.
//
// This is a DevTools Protocol session feature: Page.addScriptToEvaluateOnNewDocument
// lives on the debugger session, so an extension-bridge lane that detaches
// between operations would lose it. The caller gets the named capability error
// there rather than a script that silently never runs.
type InitScriptOptions struct {
	// Action is add, remove, list, or clear.
	Action string `json:"action"`
	// Source is the JavaScript to run before the document's own scripts. Required
	// for add.
	Source string `json:"source,omitempty"`
	// ID is the identifier returned by add. Required for remove.
	ID    string `json:"id,omitempty"`
	TabID string `json:"tab_id,omitempty"`
}

// InitScriptResult is the reply for every init-script action.
type InitScriptResult struct {
	OK      bool         `json:"ok"`
	TabID   string       `json:"tab_id,omitempty"`
	Action  string       `json:"action"`
	Added   *InitScript  `json:"added,omitempty"`
	Removed string       `json:"removed,omitempty"`
	Scripts []InitScript `json:"scripts,omitempty"`
	Message string       `json:"message,omitempty"`
}

// maxInitScriptBytes bounds one script so a caller cannot hand the page a
// multi-megabyte preamble that every navigation re-parses.
const maxInitScriptBytes = 256 * 1024

// initScriptPreviewBytes is how much source a listing echoes.
const initScriptPreviewBytes = 120

// NormalizeInitScript validates an init-script request and canonicalizes the
// action name.
func NormalizeInitScript(opts InitScriptOptions) (InitScriptOptions, error) {
	action := strings.ToLower(strings.TrimSpace(opts.Action))
	if action == "" {
		action = "list"
	}
	out := opts
	out.Action = action
	switch action {
	case "add":
		if strings.TrimSpace(opts.Source) == "" {
			return out, errors.New("init script action=add needs source: the JavaScript to run before each new document")
		}
		if len(opts.Source) > maxInitScriptBytes {
			return out, fmt.Errorf("init script source is %d bytes, over the %d-byte limit", len(opts.Source), maxInitScriptBytes)
		}
	case "remove":
		if strings.TrimSpace(opts.ID) == "" {
			return out, errors.New("init script action=remove needs id: the identifier returned by action=add")
		}
	case "list", "clear":
	default:
		return out, fmt.Errorf("unknown init script action %q: use add, remove, list, or clear", opts.Action)
	}
	return out, nil
}

// NewInitScript builds the stored record for a registered script.
func NewInitScript(id, source string) InitScript {
	return InitScript{ID: id, Bytes: len(source), Preview: initScriptPreview(source)}
}

func initScriptPreview(source string) string {
	collapsed := strings.Join(strings.Fields(source), " ")
	if len(collapsed) <= initScriptPreviewBytes {
		return collapsed
	}
	return collapsed[:initScriptPreviewBytes] + "…"
}
