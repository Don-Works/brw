package extensionbridge

import (
	"testing"
	"time"
)

func TestAcceptLogLimiterLogsEachOriginHourly(t *testing.T) {
	var limiter acceptLogLimiter
	start := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	const foreign = "chrome-extension://hkomepfdcddgepbdalomhabiphokllkd"
	steps := []struct {
		name        string
		origin      string
		at          time.Duration
		wantLog     bool
		wantSkipped int
	}{
		{name: "first refusal", origin: foreign, wantLog: true},
		{name: "retry eleven seconds later", origin: foreign, at: 11 * time.Second},
		{name: "another retry", origin: foreign, at: 22 * time.Second},
		{name: "another origin is logged on its own", origin: "chrome-extension://other", at: 30 * time.Second, wantLog: true},
		{name: "an hour on, with the count", origin: foreign, at: time.Hour, wantLog: true, wantSkipped: 2},
		{name: "count resets", origin: foreign, at: 2*time.Hour + time.Second, wantLog: true},
	}
	for _, step := range steps {
		logged, skipped := limiter.admit(step.origin, start.Add(step.at))
		if logged != step.wantLog || skipped != step.wantSkipped {
			t.Fatalf("%s: logged=%v skipped=%d, want %v %d", step.name, logged, skipped, step.wantLog, step.wantSkipped)
		}
	}
}
