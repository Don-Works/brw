package baseline

import (
	"errors"
	"reflect"
)

// Storage is the set a baseline check reads and writes.
//
// Check took *Store until a baseline gained a second destination. A baseline of
// a signed-in page is a screenshot of private content, and the private recipe
// provider is where the recipe that produced it already lives; a local
// directory is the right home for a public fixture and the wrong one for that.
// The interface is what lets one Check serve both without a second copy of the
// comparison rules — and the rules are the part that must not fork, because
// "check writes nothing" is enforced in Check and nowhere else.
//
// Location names the destination in words a result can carry. A baseline that
// lands somewhere the operator did not expect is the failure this whole area is
// about, so the answer says where it went rather than leaving it to be inferred
// from which flags the daemon was started with.
type Storage interface {
	Load(Key) (Record, bool, error)
	Save(Record) error
	EnvironmentsFor(digest string, step int) ([]Environment, error)
	Delete(Key) error
	Location() string
}

// ErrNoStorage is the refusal when no destination is configured at all.
var ErrNoStorage = errors.New("no baseline store is configured on this daemon")

// noStorage reports whether a Storage is unusable.
//
// `store == nil` is not enough once the parameter is an interface: a nil
// *Store passed as a Storage is a NON-nil interface value holding a nil
// pointer, so the plain comparison stops firing and the call nil-derefs inside
// the implementation instead of refusing. The typed case is what a caller
// actually produces — a field that was never set, handed straight to Check.
func noStorage(store Storage) bool {
	if store == nil {
		return true
	}
	switch value := reflect.ValueOf(store); value.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return value.IsNil()
	default:
		return false
	}
}

// Location names the local directory root. It is deliberately the path: an
// operator reading a result wants to know which directory to look in.
func (s *Store) Location() string { return "the local baseline root " + s.root }
