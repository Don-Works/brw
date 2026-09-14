package extensionbridge

import (
	"context"
	"errors"

	"github.com/Don-Works/brw/internal/browser"
)

// ErrSessionStateUnsupported is the extension bridge's refusal of brw_state.
// This is a policy refusal, not a missing capability: the bridge drives the
// browser the user is personally signed into, and sealing that profile's
// cookies is the "no cookie extraction" non-goal in docs/auth-model.md. A
// scoped snapshot is only ever taken of a session brw's own browser
// established, so it belongs to the direct-CDP transport.
var ErrSessionStateUnsupported = errors.New("session snapshots are not supported on the extension-bridge transport; brw_state would seal the cookies of the browser you are signed into, which brw does not do — use a direct-CDP profile, or an incognito context there")

// SessionState is refused on the extension bridge; see ErrSessionStateUnsupported.
func (b *Bridge) SessionState(_ context.Context, _ browser.SessionStateOptions) (browser.SessionStateResult, error) {
	return browser.SessionStateResult{}, ErrSessionStateUnsupported
}

var _ browser.SessionStateController = (*Bridge)(nil)
