package baseline

import (
	"errors"
	"reflect"
)

// Storage is the set a baseline check reads and writes.
type Storage interface {
	Load(Key) (Record, bool, error)
	Save(Record) error
	EnvironmentsFor(digest string, step int) ([]Environment, error)
	Delete(Key) error
	Location() string
}

// ErrNoStorage is the refusal when no destination is configured at all.
var ErrNoStorage = errors.New("no baseline store is configured on this daemon")

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

// Location names the local directory root.
func (s *Store) Location() string { return "the local baseline root " + s.root }
