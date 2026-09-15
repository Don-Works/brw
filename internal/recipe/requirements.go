package recipe

import (
	"errors"
	"fmt"
)

// ProfileSessionChecker is the surface capability behind RequiresProfileSession:
// reporting whether the browser on the other side of this surface could be one
// a human already signed into.
type ProfileSessionChecker interface {
	CheckProfileSession() error
}

// checkRequirements refuses a recipe whose declared requirements this runner
// cannot honour, before any step executes.
//
// It is a switch over the closed Requirements domain with no default that
// passes: a name the schema accepts and this function does not recognise is
// refused, so a requirement can never be added to one half and forgotten in the
// other. That is the whole reason the declaration is worth having — a
// requirement that is parsed, stored, shown in a recipe listing and never
// checked is a guarantee brw does not have.
//
// It fails CLOSED when the surface cannot answer. A surface with no
// ProfileSessionChecker is not "probably the signed-in browser"; it is a
// surface that has not said, and running a login-shaped flow on an unknown
// browser is exactly the outcome the declaration exists to prevent.
func (r Runner) checkRequirements(value Recipe) error {
	var problems []error
	for _, name := range value.Requires {
		switch name {
		case RequiresProfileSession:
			checker, ok := r.Surface.(ProfileSessionChecker)
			if !ok {
				problems = append(problems, fmt.Errorf("recipe requires %s, and this browser surface cannot say whether it drives the signed-in profile; brw refuses rather than running a signed-in flow signed out", RequiresProfileSession))
				continue
			}
			if err := checker.CheckProfileSession(); err != nil {
				problems = append(problems, fmt.Errorf("recipe requires %s: %w", RequiresProfileSession, err))
			}
		default:
			problems = append(problems, fmt.Errorf("recipe declares requirement %q, which this runner does not implement", name))
		}
	}
	return errors.Join(problems...)
}
