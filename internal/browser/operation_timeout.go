package browser

import (
	"fmt"
	"strings"
	"time"
)

// NoOperationTimeout stands for "no fixed per-operation limit". It is a
// deadline far enough away never to fire, so every operation still runs under
// a context and still ends when its caller cancels it or its own step timeout
// passes. Small sums of it stay inside time.Duration.
const NoOperationTimeout = 50 * 365 * 24 * time.Hour

// OperationTimeout normalises a configured per-operation timeout: zero or
// less means no fixed limit.
func OperationTimeout(d time.Duration) time.Duration {
	if d <= 0 || d > NoOperationTimeout {
		return NoOperationTimeout
	}
	return d
}

// ParseOperationTimeout reads a profile's operation_timeout: a Go duration,
// with "0", "none" and "off" meaning no fixed limit.
func ParseOperationTimeout(raw string) (time.Duration, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "none", "off":
		return NoOperationTimeout, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("operation_timeout %q: %w", raw, err)
	}
	return OperationTimeout(d), nil
}
