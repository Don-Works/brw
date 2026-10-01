package usageview

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzUsageAggregationShapes(f *testing.F) {
	for _, seed := range []string{`null`, `{}`, `[]`, `{"output_bytes":9223372036854775807,"duration_us":0}`, `{"output_bytes":-1,"duration_ms":-1}`, `{"output_bytes":"12","provider_input_tokens":null}`, `{"output_bytes":1.5,"duration_us":true}`, `{"output_bytes":9223372036854775808}`, `{"provider_output_tokens":0,"duration_ms":9223372036854775807}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64<<10 {
			t.Skip()
		}
		var event map[string]json.RawMessage
		if json.Unmarshal(data, &event) != nil {
			return
		}
		group := &Group{}
		for i := 0; i < 3; i++ {
			accumulate(group, event)
		}
		if group.Records != 3 || group.Errors < 0 || group.Errors > group.Records || len(group.durations) > group.Records || group.TotalMS < 0 || math.IsInf(group.TotalMS, 0) || math.IsNaN(group.TotalMS) {
			t.Fatal("invalid aggregate record or duration counts")
		}
		for _, counter := range []Counter{group.InputBytes, group.OutputBytes, group.InputEstimate, group.OutputEstimate, group.ProviderInput, group.ProviderOutput, group.ProviderCached, group.ProviderReasoning} {
			if counter.Total < 0 || counter.Samples < 0 || counter.Samples > group.Records || (counter.Samples == 0 && counter.Total != 0) {
				t.Fatal("invalid aggregate counter")
			}
		}
	})
}

func FuzzBoundedLogLines(f *testing.F) {
	for _, seed := range [][]byte{nil, []byte("\n"), []byte("synthetic\nnext"), bytes.Repeat([]byte{'x'}, 64<<10), bytes.Repeat([]byte{'x'}, 1<<20), bytes.Repeat([]byte{'x'}, (1<<20)+1)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 2<<20 {
			t.Skip()
		}
		reader := bufio.NewReaderSize(bytes.NewReader(data), 64<<10)
		line, err := boundedLine(reader)
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			end = len(data)
		} else {
			end++
		}
		if end > 1<<20 {
			if !errors.Is(err, errLongLine) || len(line) != 0 {
				t.Fatal("oversized line escaped bound")
			}
			return
		}
		if !bytes.Equal(line, data[:end]) || (err != nil && !errors.Is(err, io.EOF)) {
			t.Fatal("line boundary changed input")
		}
	})
}

func FuzzReadSyntheticLogShapes(f *testing.F) {
	for _, seed := range []string{`null`, `{}`, `{"output_bytes":null}`, `{"ts":"2026-10-01T10:00:00Z","layer":"mcp","operation":"brw_read","output_bytes":9223372036854775807}`, "synthetic\n{", `{"ts":true,"layer":[]}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64<<10 {
			t.Skip()
		}
		row := []byte("{\"ts\":\"2026-10-01T10:00:00Z\",\"layer\":\"mcp\",\"operation\":\"brw_read\",\"outcome\":\"ok\",\"output_bytes\":1}\n")
		content := append(append(append([]byte{}, row...), data...), '\n')
		content = append(content, row...)
		path := filepath.Join(t.TempDir(), "synthetic.jsonl")
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		report, err := Read(context.Background(), Options{Path: path})
		if err != nil || report.MatchedRecords < 2 || report.IncompleteLines != 0 || report.ScannedRecords < report.MatchedRecords+report.InvalidRecords {
			t.Fatalf("coverage matched=%d scanned=%d invalid=%d incomplete=%d err=%v", report.MatchedRecords, report.ScannedRecords, report.InvalidRecords, report.IncompleteLines, err)
		}
		for _, group := range report.Groups {
			if group.OutputBytes.Total < 0 || group.OutputBytes.Samples > group.Records || strings.ContainsAny(group.Operation, "\n\r") {
				t.Fatal("invalid group metadata")
			}
		}
	})
}
