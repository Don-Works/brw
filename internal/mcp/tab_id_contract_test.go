package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

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

			if required, ok := schema["required"].([]string); ok {
				for _, field := range required {
					args[field] = placeholderFor(properties[field])
				}
			}
			payload, err := json.Marshal(args)
			if err != nil {
				t.Fatalf("marshal args: %v", err)
			}

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
