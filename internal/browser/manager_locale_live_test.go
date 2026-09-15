package browser

import (
	"context"
	"testing"
	"time"
)

func TestSetLocaleIsWhatThePageReports(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	emulationTab(t, m, ctx, serveEnvironmentFixture(t))

	result, err := m.SetLocale(ctx, LocaleOptions{Locale: "en-GB", Timezone: "Europe/London"})
	if err != nil {
		t.Fatalf("SetLocale: %v", err)
	}
	if result.Locale == nil || result.Locale.LocaleICU != "en_GB" {
		t.Fatalf("result locale = %+v", result.Locale)
	}
	if result.Locale.ReportedTimezone != "Europe/London" {
		t.Fatalf("reported timezone = %q", result.Locale.ReportedTimezone)
	}
	if evaluateString(t, m, ctx, `Intl.DateTimeFormat().resolvedOptions().timeZone`) != "Europe/London" {
		t.Fatal("page timezone is not Europe/London")
	}
}
