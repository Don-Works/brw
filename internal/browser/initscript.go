package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	maxInitScriptBytes   = 64 << 10
	maxInitScriptsPerTab = 16
)

// ErrInitScriptUnsupported is returned by transports that cannot hold a
// Page.addScriptToEvaluateOnNewDocument registration across calls. The
// extension bridge attaches and detaches per operation, so a script installed
// through it would vanish before the next navigation.
var ErrInitScriptUnsupported = errors.New("init scripts are not supported on the extension-bridge transport: they are DevTools Protocol session registrations that do not survive the extension's attach/detach cycle; use a direct-CDP profile")

// InitScriptController installs JavaScript that runs at document-start on every
// subsequent navigation of a tab, and optionally on the document that is
// already open. It is a separate capability from EnvironmentController because
// the result shape is a list of identifiers, not an override echo.
type InitScriptController interface {
	AddInitScript(context.Context, InitScriptOptions) (InitScriptResult, error)
	RemoveInitScript(context.Context, InitScriptRemoveOptions) (InitScriptResult, error)
	ListInitScripts(context.Context, string) (InitScriptResult, error)
}

// InitScriptOptions adds one script. Source is required. Origin, when set,
// wraps the source so it is a no-op on any other origin — that is what keeps a
// helper for one app from running on every site the tab later visits.
type InitScriptOptions struct {
	Source string `json:"source"`
	Origin string `json:"origin,omitempty"`
	TabID  string `json:"tab_id,omitempty"`
}

// InitScriptRemoveOptions drops one previously added script by the identifier
// Add returned.
type InitScriptRemoveOptions struct {
	ID    string `json:"id"`
	TabID string `json:"tab_id,omitempty"`
}

// InitScript is the disclosure-safe description of one installed script. The
// source is never echoed: it can contain tokens, and a result that repeats it
// puts them in the agent transcript.
type InitScript struct {
	ID     string `json:"id"`
	Origin string `json:"origin,omitempty"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

// InitScriptResult lists the scripts this tab currently holds after the call.
type InitScriptResult struct {
	OK      bool         `json:"ok"`
	TabID   string       `json:"tab_id,omitempty"`
	Added   *InitScript  `json:"added,omitempty"`
	Removed string       `json:"removed,omitempty"`
	Scripts []InitScript `json:"scripts"`
	Count   int          `json:"count"`
	Message string       `json:"message,omitempty"`
}

// NormalizeInitScript validates a new script and returns the origin to wrap
// with (empty means run everywhere).
func NormalizeInitScript(opts InitScriptOptions) (source, origin string, err error) {
	source = opts.Source
	if strings.TrimSpace(source) == "" {
		return "", "", errors.New("init script source is empty")
	}
	if len(source) > maxInitScriptBytes {
		return "", "", fmt.Errorf("init script is %d bytes; the maximum is %d", len(source), maxInitScriptBytes)
	}
	if opts.Origin == "" {
		return source, "", nil
	}
	origin, err = CanonicalOrigin(opts.Origin)
	if err != nil {
		return "", "", err
	}
	return source, origin, nil
}

// WrapInitScript prefixes the source with an origin guard when origin is set.
// The origin is JSON-encoded so a quote in a host cannot break out of the
// comparison. Scripts with no origin run as written.
func WrapInitScript(source, origin string) string {
	if origin == "" {
		return source
	}
	encoded, _ := json.Marshal(origin)
	return "(function(){if(location.origin!==" + string(encoded) + ")return;\n" + source + "\n})();"
}

func initScriptDigest(source string) string {
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:])
}
