package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The include parameter is documented as an array of section names OR one
// comma-separated string, and both forms have to reach the same windowed read.
func TestReadToolAcceptsIncludeAsArrayOrCommaString(t *testing.T) {
	for _, args := range []string{
		`{"include":["headings","links"]}`,
		`{"include":"headings,links"}`,
		`{"include":"headings, links"}`,
	} {
		t.Run(args, func(t *testing.T) {
			srv := &Server{manager: longPageController{}, toolProfile: "all"}
			got := callToolJSON(t, srv, "brw_read", args)
			if main, _ := got["main"].(string); main != "" {
				t.Fatalf("main was returned although include named only headings and links: %d chars", len(main))
			}
			headings, _ := got["headings"].([]any)
			if len(headings) != 1 {
				t.Fatalf("headings = %#v, want the page's one heading", got["headings"])
			}
			links, _ := got["links"].([]any)
			if len(links) != 1 {
				t.Fatalf("links = %#v, want the page's one link", got["links"])
			}
		})
	}
}

func TestReadToolRejectsIncludeOfOtherTypesByName(t *testing.T) {
	srv := &Server{manager: longPageController{}, toolProfile: "all"}
	_, rpcErr := srv.callTool(context.Background(), "brw_read", json.RawMessage(`{"include":42}`))
	if rpcErr == nil {
		t.Fatal("a numeric include was accepted")
	}
	if !strings.Contains(rpcErr.Message, "array of section names or a comma-separated string") {
		t.Fatalf("error = %q, want it to name both accepted forms", rpcErr.Message)
	}
}

// The published schema has to say what the decoder accepts, or a client that
// follows the schema sends the array and one that follows the description
// sends the string and one of them is refused.
func TestReadToolSchemaDeclaresBothIncludeForms(t *testing.T) {
	var include map[string]any
	for _, tl := range tools() {
		if name, _ := tl["name"].(string); name != "brw_read" {
			continue
		}
		schema, _ := tl["inputSchema"].(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		include, _ = properties["include"].(map[string]any)
	}
	if include == nil {
		t.Fatal("brw_read schema has no include property")
	}
	kinds, _ := include["type"].([]string)
	if strings.Join(kinds, ",") != "array,string" {
		t.Fatalf("include type = %#v, want [array string]", include["type"])
	}
	description, _ := include["description"].(string)
	if !strings.Contains(description, "comma-separated string") {
		t.Fatalf("include description does not mention the comma-separated form: %q", description)
	}
}
