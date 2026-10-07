package recipe

import (
	"errors"
	"fmt"
)

// ProfileSessionChecker is the surface capability behind RequiresProfileSession: reporting whether the browser on the other side of this surface could be one a human already signed into.
type ProfileSessionChecker interface {
	CheckProfileSession() error
}

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
