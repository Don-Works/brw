package mcp

import (
	"strings"
	"testing"
)

func textOf(t *testing.T, result any) string {
	t.Helper()
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", result)
	}
	content, ok := m["content"].([]toolContent)
	if !ok {
		t.Fatalf("content is not []toolContent: %T", m["content"])
	}
	if len(content) == 0 {
		t.Fatal("content is empty")
	}
	return content[0].Text
}

func TestEvaluateResult_SmallObjectUnchanged(t *testing.T) {
	res, rpcErr := evaluateResult(map[string]any{"a": 1}, nil, 0, 0)
	if rpcErr != nil {
		t.Fatalf("unexpected rpc error: %+v", rpcErr)
	}
	if got := textOf(t, res); got != `{"a":1}` {
		t.Fatalf("text = %q, want compact object", got)
	}
	m := res.(map[string]any)
	if _, ok := m["structuredContent"]; !ok {
		t.Fatal("expected structuredContent for an object payload")
	}
}

func TestEvaluateResult_OversizedTruncatedNotEmpty(t *testing.T) {
	// A large string result that previously came back EMPTY past the cap.
	big := strings.Repeat("x", defaultEvaluateMaxBytes*2)
	res, rpcErr := evaluateResult(big, nil, 0, 0)
	if rpcErr != nil {
		t.Fatalf("unexpected rpc error: %+v", rpcErr)
	}
	text := textOf(t, res)
	if text == "" {
		t.Fatal("oversized result must never be empty")
	}
	if !strings.Contains(text, "truncated") {
		t.Fatalf("expected truncation marker, got tail: %q", text[len(text)-80:])
	}
	// The serialized value is the JSON-quoted string, so total = len+2 quotes.
	if !strings.Contains(text, "of "+itoa(len(big)+2)+" bytes") {
		t.Fatalf("expected total byte count in marker, got tail: %q", text[len(text)-120:])
	}
}

func TestEvaluateResult_OffsetPagination(t *testing.T) {
	big := strings.Repeat("y", 100)
	// JSON serializes to "yyy...y" (102 bytes incl quotes). Page from offset 50.
	res, rpcErr := evaluateResult(big, nil, 50, 10)
	if rpcErr != nil {
		t.Fatalf("unexpected rpc error: %+v", rpcErr)
	}
	text := textOf(t, res)
	if !strings.HasPrefix(text, "yyyyyyyyyy") {
		t.Fatalf("expected 10-byte window of y's, got prefix: %q", text[:12])
	}
	if !strings.Contains(text, "offset 50") {
		t.Fatalf("expected offset in marker, got: %q", text)
	}
}

func TestEvaluateResult_OffsetBeyondEnd(t *testing.T) {
	res, rpcErr := evaluateResult("short", nil, 9999, 0)
	if rpcErr != nil {
		t.Fatalf("unexpected rpc error: %+v", rpcErr)
	}
	text := textOf(t, res)
	if !strings.Contains(text, "returned 0 of") {
		t.Fatalf("expected explicit zero-window marker, got: %q", text)
	}
}

func TestEvaluateResult_MaxBytesOverflowNoPanic(t *testing.T) {
	// Regression (review #1): max_bytes near MaxInt with a non-zero offset must
	// not overflow offset+maxBytes (which would wrap negative and panic the
	// data[offset:end] slice). It should clamp to the end and return the rest.
	big := strings.Repeat("z", 100)
	const huge = int(^uint(0) >> 1) // platform max int
	res, rpcErr := evaluateResult(big, nil, 1, huge)
	if rpcErr != nil {
		t.Fatalf("unexpected rpc error: %+v", rpcErr)
	}
	text := textOf(t, res)
	if text == "" {
		t.Fatal("must not be empty")
	}
	if !strings.HasPrefix(text, "zzz") {
		t.Fatalf("expected remainder from offset 1, got prefix: %q", text[:10])
	}
}

// itoa avoids importing strconv just for the test's marker assertions.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
