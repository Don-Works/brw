package httpclient

import (
	"errors"
	"net/http"

	"github.com/Don-Works/brw/internal/usagelog"
)

// RemoteError is a refusal the daemon answered with, carrying the daemon's own classification of it alongside the message.
type RemoteError struct {
	// Status is the HTTP status the daemon answered with.
	Status int
	// Class is the daemon's error classification, for example "policy_denied", "timeout" or "transport".
	Class string
	// Message is the daemon's own error text, bounded like every other upstream error.
	Message string
	// Body is the response body as far as it was read, so a caller can recover a structured verdict the daemon put beside the message.
	Body []byte
}

func (e *RemoteError) Error() string { return e.Message }

// RemoteClass returns the daemon's classification of err, or "" when err did not come from a daemon that sent one.
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

func newRemoteError(resp *http.Response, message string, body []byte) *RemoteError {
	return &RemoteError{Status: resp.StatusCode, Class: resp.Header.Get(usagelog.HeaderErrorClass), Message: message, Body: body}
}
