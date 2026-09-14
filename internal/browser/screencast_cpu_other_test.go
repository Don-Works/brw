//go:build !unix

package browser

import "time"

// processCPU has no portable form outside unix. The comparison test reports the
// unavailability and checks only the transferred bytes there, rather than
// asserting on a number it did not measure.
func processCPU() (time.Duration, bool) { return 0, false }
