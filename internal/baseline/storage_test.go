package baseline

import (
	"errors"
	"runtime"
	"testing"
)

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
