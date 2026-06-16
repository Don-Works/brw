package mcp

import "testing"

func TestIsJSONObject(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{`{"a":1}`, true},
		{`  {"a":1}`, true},
		{"\n\t{}", true},
		{`[]`, false},
		{`[1,2,3]`, false},
		{`"Example Domain"`, false},
		{`42`, false},
		{`null`, false},
		{`true`, false},
		{``, false},
	}
	for _, c := range cases {
		if got := isJSONObject([]byte(c.in)); got != c.want {
			t.Errorf("isJSONObject(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// toolJSON must only attach structuredContent when the payload is a JSON object;
// strict MCP clients reject a non-object structuredContent ("expected record"),
// which previously forced agents into wasteful browser_evaluate retries.
func TestToolJSONStructuredContentOnlyForObjects(t *testing.T) {
	hasStructured := func(v any) bool {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("toolJSON returned %T, want map", v)
		}
		_, present := m["structuredContent"]
		return present
	}

	// Object result → structuredContent present.
	res, rpcErr := toolJSON(map[string]any{"h1": "Example Domain"}, nil)
	if rpcErr != nil {
		t.Fatalf("unexpected rpc error: %v", rpcErr)
	}
	if !hasStructured(res) {
		t.Error("object payload should carry structuredContent")
	}

	// Scalar result (browser_evaluate of `document.title`) → omitted.
	res, _ = toolJSON("Example Domain", nil)
	if hasStructured(res) {
		t.Error("string payload must NOT carry structuredContent")
	}

	// Array result (list tools) → omitted.
	res, _ = toolJSON([]string{"a", "b"}, nil)
	if hasStructured(res) {
		t.Error("array payload must NOT carry structuredContent")
	}

	// The text content must always be present regardless.
	m := res.(map[string]any)
	if _, ok := m["content"]; !ok {
		t.Error("text content must always be present")
	}
}
