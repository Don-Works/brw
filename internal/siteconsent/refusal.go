package siteconsent

import "errors"

// IsRefusal reports whether an error is this gate saying no.
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
