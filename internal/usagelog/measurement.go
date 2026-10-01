package usagelog

import (
	"context"
	"encoding/json"
	"unicode/utf8"
)

type requestIDContextKey struct{}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDContextKey{}, SafeID(id))
}

func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDContextKey{}).(string)
	return id
}

func Count(value int64) *int64 { return &value }

func safeScope(value string) string {
	switch value {
	case "transport", "tool", "catalogue", "projection":
		return value
	}
	return ""
}

func safeRepresentation(value string) string {
	switch value {
	case "http_body", "mcp_arguments_result", "mcp_catalogue", "cli_stdout":
		return value
	}
	return ""
}

func MeasureJSON(data []byte) (chars, binary int64) {
	chars = int64(utf8.RuneCount(data))
	var value any
	if json.Unmarshal(data, &value) != nil {
		return chars, 0
	}
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			kind, _ := v["type"].(string)
			for key, child := range v {
				text, isText := child.(string)
				if isText && (key == "bytes_base64" || key == "data_base64" || key == "base64" || (key == "data" && (kind == "image" || kind == "audio"))) {
					binary += int64(len(text))
					chars -= int64(utf8.RuneCountInString(text))
				} else {
					walk(child)
				}
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(value)
	return chars, binary
}

func EstimateTokens(chars int64) int64 { return (chars + 3) / 4 }
