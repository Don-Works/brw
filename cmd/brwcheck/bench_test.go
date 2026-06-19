package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPayloadBytes(t *testing.T) {
	if got := payloadBytes(nil); got != 0 {
		t.Fatalf("nil payload bytes = %d, want 0", got)
	}
	if got := payloadBytes(map[string]string{"ok": "true"}); got <= 2 {
		t.Fatalf("map payload bytes = %d, want > 2", got)
	}
}

func TestPrintBenchTable(t *testing.T) {
	card := benchScorecard{
		Fixture:   "custom-combobox.html",
		Path:      "file:///tmp/custom-combobox.html",
		Transport: "direct-cdp",
		Rows: []benchMeasure{
			{Action: "open", MS: 12, Bytes: 34, OK: true},
			{Action: "click", MS: 5, Bytes: 90, OK: true},
		},
		TotalMS:    17,
		TotalBytes: 124,
		OK:         true,
	}

	var buf bytes.Buffer
	printBenchTable(&buf, card)

	out := buf.String()
	for _, want := range []string{"brwcheck bench", "ACTION", "open", "TOTAL", "17", "124"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}
