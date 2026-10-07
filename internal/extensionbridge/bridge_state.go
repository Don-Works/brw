package extensionbridge

import (
	"context"
	"errors"

	"github.com/Don-Works/brw/internal/browser"
)

// ErrSessionStateUnsupported is the extension bridge's refusal of brw_state.
var ErrSessionStateUnsupported = errors.New("session snapshots are not supported on the extension-bridge transport; brw_state would seal the cookies of the browser you are signed into, which brw does not do — use a direct-CDP profile, or an incognito context there")

// SessionState is refused on the extension bridge; see ErrSessionStateUnsupported.
func (b *Bridge) SessionState(_ context.Context, _ browser.SessionStateOptions) (browser.SessionStateResult, error) {
	return browser.SessionStateResult{}, ErrSessionStateUnsupported
}

var _ browser.SessionStateController = (*Bridge)(nil)
