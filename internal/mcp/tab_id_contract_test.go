package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Every tool that advertises tab_id must also ACCEPT it.
//
// callTool lifts tab_id off any payload and puts it on the context, so the
// targeting works for free — but each case unmarshals its own arguments
// strictly, and a request struct that does not declare the field rejects the
// call outright with "unknown field tab_id". brw_get and brw_storage shipped
// that way in v0.13.0: their schemas offered a tab_id the dispatch then refused,
// so a multi-tab agent could not read a fact from a named tab.
//
// The schema is the contract, so this walks the real catalogue rather than a
// hand-maintained list: a new tool that advertises tab_id is covered the moment
// it is added.
func TestEveryToolAdvertisingTabIDAcceptsIt(t *testing.T) {
	server := New(fakeController{})
	ctx := context.Background()

	for _, tl := range tools() {
		name, _ := tl["name"].(string)
		schema, _ := tl["inputSchema"].(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		if properties == nil {
			continue
		}
		if _, advertises := properties["tab_id"]; !advertises {
			continue
		}

		t.Run(name, func(t *testing.T) {
			args := map[string]any{"tab_id": "SOME-TAB-ID"}
			// Fill required fields so the call reaches argument unmarshalling
			// rather than stopping at validation.
			if required, ok := schema["required"].([]string); ok {
				for _, field := range required {
					args[field] = placeholderFor(properties[field])
				}
			}
			payload, err := json.Marshal(args)
			if err != nil {
				t.Fatalf("marshal args: %v", err)
			}

			// The fake controller cannot really drive a browser, so most tools
			// fail here. What matters is HOW: a rejection naming tab_id as an
			// unknown field means the schema and the request struct disagree.
			result, rpcErr := server.callTool(ctx, name, payload)
			for _, text := range []string{rpcErrText(rpcErr), resultText(result)} {
				if strings.Contains(text, "unknown field") && strings.Contains(text, "tab_id") {
					t.Fatalf("%s advertises tab_id but its dispatch rejects it: %s", name, text)
				}
			}
		})
	}
}

func placeholderFor(spec any) any {
	properties, _ := spec.(map[string]any)
	if properties == nil {
		return "x"
	}
	if enum, ok := properties["enum"].([]any); ok && len(enum) > 0 {
		return enum[0]
	}
	switch properties["type"] {
	case "integer", "number":
		return 1
	case "boolean":
		return true
	case "array":
		return []any{}
	case "object":
		return map[string]any{}
	}
	return "x"
}

func rpcErrText(err *rpcError) string {
	if err == nil {
		return ""
	}
	return err.Message
}

func resultText(result any) string {
	encoded, err := json.Marshal(result)
	if err != nil {
		return ""
	}
	return string(encoded)
}
