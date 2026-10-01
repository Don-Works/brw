package usageview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadSeparatesBoundariesAndUnknownCounters(t *testing.T) {
	dir := t.TempDir()
	data := `{"ts":"2026-10-01T10:00:00Z","layer":"http","operation":"brw_read","outcome":"ok","duration_ms":3,"input_bytes":0,"output_bytes":100}
{"ts":"2026-10-01T10:00:01Z","layer":"mcp","operation":"brw_read","outcome":"ok","duration_us":4500,"output_bytes":120,"estimated_output_tokens_chars4":20}
{"ts":"2026-10-01T10:00:02Z","layer":"mcp","operation":"brw_read","outcome":"error","duration_us":10000}
{"ts":"2026-10-01T10:00:03Z","layer":"reader","scope":"model","operation":"answer","outcome":"success","provider_input_tokens":42,"provider_output_tokens":0,"duration_us":100000}
{"ts":"2026-09-01T10:00:00Z","layer":"mcp","operation":"brw_read","output_bytes":9999}
not json
{"ts":`
	if err := os.WriteFile(filepath.Join(dir, "usage.ndjson"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := Read(context.Background(), Options{Path: dir, Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if r.MatchedRecords != 4 || r.InvalidRecords != 1 || r.IncompleteLines != 1 || len(r.Groups) != 3 {
		t.Fatalf("coverage: %+v", r)
	}
	g := r.Groups[0]
	if g.Layer != "mcp" || g.OutputBytes.Total != 120 || g.OutputBytes.Samples != 1 || g.InputBytes.Samples != 0 || g.Errors != 1 || g.P50MS != 4.5 || g.P95MS != 10 {
		t.Fatalf("MCP counters: %+v", g)
	}
	if r.Groups[1].InputBytes.Samples != 1 || r.Groups[1].InputBytes.Total != 0 {
		t.Fatal("known zero lost")
	}
	if r.Groups[2].ProviderInput.Total != 42 || r.Groups[2].ProviderOutput.Samples != 1 {
		t.Fatal("provider count lost")
	}
}

func TestReadRotationBoundsAndDuplicateFiles(t *testing.T) {
	dir := t.TempDir()
	row := []byte("{\"ts\":\"2026-10-01T10:00:00Z\",\"layer\":\"http\",\"operation\":\"brw_read\",\"outcome\":\"ok\"}\n")
	path := filepath.Join(dir, "usage.ndjson")
	if err := os.WriteFile(path, row, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(dir, "linked.ndjson")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reader.jsonl.1"), row, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.txt"), row, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "long.ndjson"), []byte(strings.Repeat("x", (1<<20)+1)), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := Read(context.Background(), Options{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	if r.MatchedRecords != 2 || r.Files != 3 || !r.Bounded || r.InvalidRecords != 1 {
		t.Fatalf("coverage: %+v", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Read(ctx, Options{Path: dir}); err != context.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
}
