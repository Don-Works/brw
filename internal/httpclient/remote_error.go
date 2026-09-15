package httpclient

import (
	"errors"
	"net/http"

	"github.com/Don-Works/brw/internal/usagelog"
)

// RemoteError is a refusal the daemon answered with, carrying the daemon's own
// classification of it alongside the message.
//
// Without it every non-2xx answer arrives here as prose, and a caller deciding
// what to do next — retry, give up, tell a human to grant something — has
// nothing to decide on but the wording of a sentence. The daemon already
// computes a stable class for the operational ledger (usagelog.ClassifyError)
// and sends it on every error response; this keeps that class attached across
// the transport instead of throwing it away one layer short of the caller that
// needs it.
//
// Error() returns the message alone, so anything that only prints the error
// reads exactly as it did before.
type RemoteError struct {
	// Status is the HTTP status the daemon answered with.
	Status int
	// Class is the daemon's error classification, for example "policy_denied",
	// "timeout" or "transport". Empty when the daemon sent none.
	Class string
	// Message is the daemon's own error text, bounded like every other upstream
	// error.
	Message string
	// Body is the response body as far as it was read, so a caller can recover a
	// structured verdict the daemon put beside the message. It is bounded by the
	// same error-response limit, so a very large failure body is truncated and
	// will not parse — callers treat a parse failure as "no verdict", never as a
	// different outcome.
	Body []byte
}

func (e *RemoteError) Error() string { return e.Message }

// RemoteClass returns the daemon's classification of err, or "" when err did
// not come from a daemon that sent one. It is the accessor callers should use:
// the error may be wrapped by the time it is classified.
func RemoteClass(err error) string {
	var remote *RemoteError
	if errors.As(err, &remote) {
		return remote.Class
	}
	return ""
}

// RemoteBody returns the response body of a daemon refusal, or nil.
func RemoteBody(err error) []byte {
	var remote *RemoteError
	if errors.As(err, &remote) {
		return remote.Body
	}
	return nil
}

// RemoteStatus returns the HTTP status of a daemon refusal, or 0.
func RemoteStatus(err error) int {
	var remote *RemoteError
	if errors.As(err, &remote) {
		return remote.Status
	}
	return 0
}

// newRemoteError builds the typed refusal. A response with no class is still
// typed: the status and body are worth keeping even when the daemon is an older
// build that sent no header.
func newRemoteError(resp *http.Response, message string, body []byte) *RemoteError {
	class := ""
	if resp != nil {
		class = resp.Header.Get(usagelog.HeaderErrorClass)
	}
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	return &RemoteError{Status: status, Class: class, Message: message, Body: body}
}
