package siteconsent

import "errors"

// IsRefusal reports whether an error is this gate saying no.
//
// It exists because a caller has to tell "the policy refused this" apart from
// "the browser fell over". They need opposite handling: a refusal is settled
// until a human grants something, and retrying it is pointless, while a
// transport failure is worth another attempt. An unattended run — a launchd job,
// a systemd timer — has nobody to ask and must exit with a code that says which
// of the two happened, and the exit code is all a scheduler ever sees.
//
// The test is the error's TYPE, not its message. Every refusal this package
// produces is one of the named types below, so a reworded message cannot
// silently reclassify a refusal as a generic failure, and
// TestEveryRefusalTypeIsRecognised fails on a type this function does not name.
func IsRefusal(err error) bool {
	if err == nil {
		return false
	}
	var (
		notGranted           *NotGrantedError
		denied               *DeniedError
		categoryBlocked      *CategoryBlockedError
		adminBlocked         *AdminBlockedError
		confirmationRequired *ConfirmationRequiredError
		confirmationDeclined *ConfirmationDeclinedError
		cannotDecide         *CannotDecideError
		localTarget          *LocalTargetError
	)
	return errors.As(err, &notGranted) ||
		errors.As(err, &denied) ||
		errors.As(err, &categoryBlocked) ||
		errors.As(err, &adminBlocked) ||
		errors.As(err, &confirmationRequired) ||
		errors.As(err, &confirmationDeclined) ||
		errors.As(err, &cannotDecide) ||
		errors.As(err, &localTarget) ||
		errors.Is(err, ErrPromptUnanswerable)
}
