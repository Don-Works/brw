package harness

import (
	"fmt"
	"strings"

	"github.com/Don-Works/brw/internal/snapshot"
)

// ElementQuery names a control the way a harness script refers to it: by role
// and by a fragment of its accessible name.
type ElementQuery struct {
	Role string
	Name string
}

func (q ElementQuery) String() string {
	return fmt.Sprintf("role=%s name~%q", q.Role, q.Name)
}

// FindElement returns the first element matching the query.
func FindElement(elements []snapshot.Element, query ElementQuery) (snapshot.Element, bool) {
	for _, element := range elements {
		if query.Role != "" && !strings.EqualFold(element.Role, query.Role) {
			continue
		}
		if query.Name != "" && !strings.Contains(strings.ToLower(element.Name), strings.ToLower(query.Name)) {
			continue
		}
		return element, true
	}
	return snapshot.Element{}, false
}

// ResolveRefs maps each wanted control to a ref, and fails naming the control
// the page did not offer. A harness that carries on with a missing ref fails
// several steps later as an unexplained action error.
func ResolveRefs(elements []snapshot.Element, wanted map[string]ElementQuery) (map[string]string, error) {
	refs := make(map[string]string, len(wanted))
	for key, query := range wanted {
		element, ok := FindElement(elements, query)
		if !ok {
			return nil, fmt.Errorf("page offers no %s (%s)", key, query)
		}
		refs[key] = element.Ref
	}
	return refs, nil
}
