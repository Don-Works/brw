package usageview

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadRotatedMalformedAndOversizedCoverage(t *testing.T) {
	dir := t.TempDir()
	row := "{\"ts\":\"2026-10-01T10:00:00Z\",\"layer\":\"mcp\",\"operation\":\"brw_read\",\"outcome\":\"ok\",\"output_bytes\":1}\n"
	files := map[string]string{
		"usage.jsonl":   row + "{\"ts\":",
		"usage.jsonl.1": row + "not json\n" + row,
		"usage.jsonl.2": row + strings.Repeat("x", (1<<20)+1) + "\n" + row,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	report, err := Read(context.Background(), Options{Path: dir})
	if err != nil || report.Files != 3 || report.InvalidRecords != 2 || report.IncompleteLines != 1 || !report.Bounded || report.MatchedRecords != 4 {
		t.Fatalf("unexpected coverage: %+v err=%v", report, err)
	}
	if _, err := json.Marshal(report); err != nil {
		t.Fatal(err)
	}
}

func TestReadEnforcesFileLimit(t *testing.T) {
	dir := t.TempDir()
	row := []byte("{\"ts\":\"2026-10-01T10:00:00Z\",\"layer\":\"mcp\",\"operation\":\"brw_read\",\"outcome\":\"ok\"}\n")
	for i := 0; i < maxFiles+1; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("synthetic-%03d.jsonl", i)), row, 0600); err != nil {
			t.Fatal(err)
		}
	}
	report, err := Read(context.Background(), Options{Path: dir})
	if err != nil || report.Files != maxFiles || report.MatchedRecords != maxFiles || !report.Bounded {
		t.Fatalf("files=%d matched=%d bounded=%v err=%v", report.Files, report.MatchedRecords, report.Bounded, err)
	}
}
