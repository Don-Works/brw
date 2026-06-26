package mcp

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestClassifyToolError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"canceled", context.Canceled, "canceled"},
		{"wrapped-deadline", fmt.Errorf("screenshot: %w", context.DeadlineExceeded), "timeout"},
		{"no-response", errors.New("no response from downstream"), "timeout"},
		{"busy", errors.New("bridge busy: max inflight reached"), "busy"},
		{"transport", errors.New("extension bridge transport: socket closed"), "transport"},
		{"json-eof", errors.New("unmarshal response: unexpected end of JSON input"), "transport"},
		{"ordinary", errors.New("no element matches ref e17"), ""},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyToolError(tc.err); got != tc.want {
				t.Fatalf("classifyToolError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestToolErrorAttachesCodeForTransportFailures(t *testing.T) {
	res := toolError(context.DeadlineExceeded).(map[string]any)
	if res["isError"] != true {
		t.Fatalf("expected isError=true")
	}
	sc, ok := res["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("expected structuredContent for a transport error, got %#v", res)
	}
	if sc["error"] != "timeout" || sc["retryable"] != true {
		t.Fatalf("expected timeout/retryable, got %#v", sc)
	}
}

func TestToolErrorNoCodeForOrdinaryError(t *testing.T) {
	res := toolError(errors.New("no element matches ref e17")).(map[string]any)
	if _, ok := res["structuredContent"]; ok {
		t.Fatalf("ordinary tool errors must not carry a transport code: %#v", res)
	}
}
