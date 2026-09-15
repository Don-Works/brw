package browser

import "errors"

// ErrAttachOnlyNoEndpoint is returned when a lane that may only attach has no
// endpoint to attach to.
//
// It exists so the "never enable the opt-in for the user" rule is enforced by
// the code that would do the enabling rather than by every caller remembering
// not to. browser.New launches Chrome whenever RemoteURL is empty, and a Chrome
// launched with a debugging flag against the user's own profile is exactly what
// the chrome://inspect opt-in replaced: brw would be granting itself the access
// Chrome asks a human to grant.
var ErrAttachOnlyNoEndpoint = errors.New("this transport attaches to a browser brw did not start, and no debugging endpoint was discovered; brw will not launch a browser with a debugging flag to create one")
