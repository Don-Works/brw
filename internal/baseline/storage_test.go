package baseline

import (
	"errors"
	"runtime"
	"testing"
)

// TestCheckRefusesEveryShapeOfMissingStorage covers the trap the interface
// introduced.
//
// Check took *Store until a baseline gained a second destination. `store ==
// nil` caught a missing store then; against an interface it catches only the
// nil interface, and a nil *Store handed over as a Storage is a NON-nil
// interface value holding a nil pointer. That shape is what a caller with an
// unset field produces, and it used to nil-deref inside Load instead of
// returning the refusal.
func TestCheckRefusesEveryShapeOfMissingStorage(t *testing.T) {
	var typedNil *Store
	tests := []struct {
		name  string
		store Storage
	}{
		{name: "no storage at all", store: nil},
		{name: "a nil *Store handed over as a Storage", store: typedNil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A panic here is the failure: the whole point is that the refusal
			// happens before the implementation is called.
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("Check panicked on %s instead of refusing: %v", tc.name, recovered)
				}
			}()
			key := Key{
				RecipeDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				StepIndex:    0,
				Environment: Environment{
					BrowserBuild: "Chromium/152.0.0.0", ViewportWidth: 1280, ViewportHeight: 800,
					DevicePixelRatio: 1, Locale: "en-gb", OS: runtime.GOOS,
				},
			}
			_, err := Check(tc.store, CheckOptions{Key: key, Screenshot: []byte{1, 2, 3}})
			if !errors.Is(err, ErrNoStorage) {
				t.Fatalf("Check(%s) = %v, want ErrNoStorage", tc.name, err)
			}
		})
	}
}
