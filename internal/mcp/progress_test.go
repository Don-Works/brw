package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestProgressTokenAcceptsOnlyBoundedStringsAndIntegers(t *testing.T) {
	for _, token := range []string{`"opaque"`, `""`, `0`, `-1`, `9007199254740993`} {
		if !validProgressToken(json.RawMessage(token)) {
			t.Errorf("valid progress token refused: %s", token)
		}
	}
	for _, token := range []string{`null`, `false`, `{}`, `[]`, `1.5`, `1e3`, `"` + strings.Repeat("x", 129) + `"`, `"` + strings.Repeat("x", 257) + `"`} {
		if validProgressToken(json.RawMessage(token)) {
			t.Errorf("invalid progress token accepted: %s", token)
		}
	}
}

func TestLongCallProgressPingAndCancellationShareStdio(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	input, sender := io.Pipe()
	receiver, output := io.Pipe()
	defer sender.Close()
	defer receiver.Close()
	ctrl := newStdioCancelController(true)
	done := make(chan error, 1)
	go func() { done <- New(ctrl).Serve(ctx, input, output); _ = output.Close() }()
	packets := make(chan map[string]json.RawMessage, 8)
	go func() {
		reader := bufio.NewReader(receiver)
		for {
			raw, _, err := readMessage(reader, stdioModeLine)
			if err != nil {
				return
			}
			var packet map[string]json.RawMessage
			if json.Unmarshal(raw, &packet) != nil {
				return
			}
			select {
			case packets <- packet:
			case <-ctx.Done():
				return
			}
		}
	}()
	read := func() map[string]json.RawMessage {
		t.Helper()
		select {
		case packet := <-packets:
			return packet
		case <-ctx.Done():
			t.Fatal(ctx.Err())
			return nil
		}
	}
	write := func(value any) {
		t.Helper()
		if err := json.NewEncoder(sender).Encode(value); err != nil {
			t.Fatal(err)
		}
	}
	write(map[string]any{"jsonrpc": "2.0", "id": "pending", "method": "tools/call", "params": map[string]any{"name": "brw_plan", "arguments": map[string]any{"steps": []any{}}, "_meta": map[string]any{"progressToken": json.Number("9007199254740993")}}})
	select {
	case <-ctrl.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	write(map[string]any{"jsonrpc": "2.0", "id": "ping", "method": "ping"})
	if packet := read(); string(packet["id"]) != `"ping"` {
		t.Fatalf("ping blocked behind pending call: %s", packet)
	}
	packet := read()
	var progress map[string]json.RawMessage
	if string(packet["method"]) != `"notifications/progress"` || json.Unmarshal(packet["params"], &progress) != nil || string(progress["progressToken"]) != "9007199254740993" || len(progress) != 3 {
		t.Fatalf("invalid or uncorrelated progress: %s", packet)
	}
	write(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": "pending"}})
	if packet := read(); string(packet["id"]) != `"pending"` {
		t.Fatalf("cancel did not finish the request: %s", packet)
	}
	_ = sender.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("progress retained server resources after completion")
	}
	select {
	case packet := <-packets:
		t.Fatalf("notification after terminal response: %s", packet)
	default:
	}
}
