package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Don-Works/brw/internal/browser"
)

// errSessionStateUnavailable is what a transport with no session-state
// capability answers. The upstream HTTP proxy implements the capability and
// forwards it to the browser host, so this is the honest "this controller
// cannot do it at all" case rather than a routing gap.
var errSessionStateUnavailable = errors.New("this browser transport does not support session snapshots: brw_state reads and writes cookies at the browser-context level, which needs a direct-CDP profile — and is refused on a browser you are personally signed into whatever its CDP can do")

func (s *Server) callSessionState(ctx context.Context, args json.RawMessage) (any, *rpcError) {
	state, ok := s.manager.(browser.SessionStateController)
	if !ok {
		return toolError(errSessionStateUnavailable), nil
	}
	var req browser.SessionStateOptions
	if err := unmarshalStrictArgs(args, &req); err != nil {
		return nil, invalid(err)
	}
	return toolJSON(state.SessionState(ctx, req))
}

// sessionStateTool is the catalogue entry. The description states only what the
// implementation enforces: there is no action that returns a stored cookie, the
// origin allowlist is caller-supplied and applied on save AND on restore (see
// SessionStateOptions.Validate, which refuses a restore that names none), and a
// daemon with no at-rest key refuses to save rather than writing a snapshot in
// the clear.
func sessionStateTool() map[string]any {
	return tool("brw_state", "Save the signed-in state of a browser context and put it back into another one, so a throwaway or incognito run does not have to log in again. Actions: save (seal the cookies this browser context holds for the origins you name, returning an opaque snapshot_id plus counts), restore (apply a saved snapshot's cookies into a browser context — navigate afterwards to see the signed-in page), list (snapshot ids, origins, counts and expiry), delete (revoke one snapshot). WHAT YOU GET BACK IS ALWAYS METADATA: no action returns a cookie name or value, so a snapshot cannot be read out through brw, only replayed into a browser on the same host. origins is REQUIRED on save AND on restore, and is an exact allowlist of scheme://host[:port] origins — a cookie is sealed only when its domain matches one of them by the ordinary cookie domain rule, and the same filter runs again on restore against the origins the restoring call names, so a snapshot can never install a cookie for an origin you did not ask for. A restore that names no origins is refused rather than falling back to the snapshot's own, which would check the file against itself; list the snapshot to see which origins it covers. redact drops cookie names matching a glob on top of that. ttl_seconds may shorten the daemon's retention (never lengthen it); an expired snapshot fails by name and is deleted. context_id names an incognito context from brw_open_incognito; omit it for the profile's default context. A snapshot carries COOKIES ONLY — not localStorage, sessionStorage, IndexedDB or cache — so a site that keeps its session in localStorage will not be signed in by a restore. Snapshots are written encrypted on the browser host under an owner-only directory, and a daemon started without --state-key-file refuses to save at all instead of writing one in the clear. NOT ON A TRANSPORT THAT DRIVES A BROWSER YOU ARE SIGNED INTO: on the extension-bridge and chrome-opt-in-cdp transports this tool is not advertised and returns a named refusal, because brw does not seal the cookies of the browser you are personally signed into. It is a policy refusal, not a capability gap — the chrome-opt-in lane has exactly the CDP it would take.", object(map[string]any{
		"action": stringEnumSchema("save (seal this context's cookies for the named origins), restore (apply a snapshot into a context), list (metadata for the live snapshots), delete (revoke one).",
			browser.SessionStateActionSave, browser.SessionStateActionRestore, browser.SessionStateActionList, browser.SessionStateActionDelete),
		"snapshot_id": stringSchema("The opaque id returned by save. Required for restore and delete."),
		"origins": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "Exact origins the snapshot may cover, for example [\"https://app.example.com\"]. Required for save AND for restore, where what is applied is the intersection of these origins and the snapshot's own. Patterns, leading-dot domains and single-label hosts are refused.",
		},
		"redact": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "Glob patterns matched against cookie NAMES; anything matching is left out of the snapshot. Narrowing only.",
		},
		"ttl_seconds": integerSchema("Shorten this snapshot's lifetime. Omit for the daemon's default retention; a value longer than the default is capped to it."),
		"context_id":  stringSchema("Browser context to read from or write to, as returned by brw_open_incognito. Omit for the profile's default context."),
	}, []string{"action"}))
}
