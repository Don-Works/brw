package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Don-Works/brw/internal/usageview"
)

func TestUsageCommandWithoutDaemon(t *testing.T) {
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"usage", "--path", filepath.Join(t.TempDir(), "missing"), "--json", "--since", "all"}, &out, &stderr)
	if code != ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	var report usageview.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.MatchedRecords != 0 || report.Schema != "brw.usage/1" {
		t.Fatalf("report=%+v", report)
	}
}

func TestUsageCommandRejectsInvalidWindow(t *testing.T) {
	for _, args := range [][]string{{"--since", "-2h"}, {"--watch", "1ms"}, {"--limit", "-1"}} {
		var out, stderr bytes.Buffer
		if code := usageCommand(context.Background(), args, &out, &stderr); code != ExitUsage {
			t.Fatalf("%v exit=%d", args, code)
		}
	}
}

func TestUsageDurationCoverage(t *testing.T) {
	if usageDuration(0, 0, 3) != "?" || usageDuration(0, 1, 3) != "0.000*" || usageDuration(0, 3, 3) != "0.000" {
		t.Fatal("duration coverage must distinguish unknown, partial and measured zero")
	}
}
