package extensionbridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/coder/websocket"
)

// cdpCall is one CDP method the stub extension was asked for, with the params
// it carried, so a test can assert on both the sequence and the arguments.
type cdpCall struct {
	Method string
	Params map[string]any
}

// cdpStub answers cdp requests by method. reply returns the CDP result for one
// call; returning a nil map with a message answers the call as a failure.
type cdpStub struct {
	mu    sync.Mutex
	calls []cdpCall
	reply func(call cdpCall, index int) (map[string]any, string)
}

func (s *cdpStub) record(call cdpCall) (map[string]any, string) {
	s.mu.Lock()
	index := len(s.calls)
	s.calls = append(s.calls, call)
	s.mu.Unlock()
	return s.reply(call, index)
}

func (s *cdpStub) methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.calls))
	for _, call := range s.calls {
		out = append(out, call.Method)
	}
	return out
}

// serveCDPStub stands in for the extension: it answers every cdp request from
// the supplied reply function and records what was asked for.
func serveCDPStub(t *testing.T, b *Bridge, stub *cdpStub) func() {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/extension"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{testDefaultOrigin}},
	})
	if err != nil {
		srv.Close()
		t.Fatalf("dial bridge: %v", err)
	}
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})
	serveCtx, serveCancel := context.WithCancel(context.Background())
	go func() {
		for {
			_, data, readErr := conn.Read(serveCtx)
			if readErr != nil {
				return
			}
			var msg struct {
				ID     string         `json:"id"`
				Type   string         `json:"type"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			method, _ := msg.Params["method"].(string)
			params, _ := msg.Params["params"].(map[string]any)
			result, failure := stub.record(cdpCall{Method: method, Params: params})
			reply := map[string]any{"id": msg.ID, "ok": failure == ""}
			if failure == "" {
				reply["result"] = result
			} else {
				reply["error"] = failure
			}
			out, _ := json.Marshal(reply)
			_ = conn.Write(serveCtx, websocket.MessageText, out)
		}
	}()
	return func() {
		serveCancel()
		_ = conn.Close(websocket.StatusNormalClosure, "test done")
		srv.Close()
	}
}

// chunkedPDFStub answers printToPDF with a handle and then serves body in
// size-bounded IO.read chunks, so the reader's chunk loop and its EOF handling
// are both exercised rather than short-circuited by a one-chunk document.
func chunkedPDFStub(body []byte, chunk int, encode bool) *cdpStub {
	offset := 0
	stub := &cdpStub{}
	stub.reply = func(call cdpCall, _ int) (map[string]any, string) {
		switch call.Method {
		case "Page.printToPDF":
			return map[string]any{"stream": "pdf-stream-1"}, ""
		case "IO.read":
			end := min(offset+chunk, len(body))
			part := body[offset:end]
			offset = end
			data := string(part)
			if encode {
				data = base64.StdEncoding.EncodeToString(part)
			}
			return map[string]any{"data": data, "base64Encoded": encode, "eof": offset >= len(body)}, ""
		case "IO.close":
			return map[string]any{}, ""
		}
		return nil, "unexpected method " + call.Method
	}
	return stub
}

// TestBridgeCapturePDFStreamReadsInChunksAndReleasesTheHandle is the extension
// half of the streaming capability. Parity with direct CDP is the point: a
// memory guarantee that holds on one transport and not the other is decided by
// which profile a caller happens to be driving.
func TestBridgeCapturePDFStreamReadsInChunksAndReleasesTheHandle(t *testing.T) {
	body := bytes.Repeat([]byte("%PDF-stream-body\n"), 400)
	tests := []struct {
		name   string
		encode bool
	}{
		{name: "base64 chunks", encode: true},
		{name: "raw chunks", encode: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			b := New("", 5*time.Second, "")
			const chunk = 512
			stub := chunkedPDFStub(body, chunk, test.encode)
			cleanup := serveCDPStub(t, b, stub)
			defer cleanup()

			stream, err := b.CapturePDFStream(browser.WithTabID(context.Background(), "42"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(stream)
			if err != nil {
				t.Fatal(err)
			}
			if closeErr := stream.Close(); closeErr != nil {
				t.Fatalf("close stream: %v", closeErr)
			}
			if !bytes.Equal(got, body) {
				t.Fatalf("streamed %d bytes, want %d", len(got), len(body))
			}

			methods := stub.methods()
			wantReads := (len(body) + chunk - 1) / chunk
			if len(methods) != wantReads+2 {
				t.Fatalf("cdp calls = %v, want printToPDF, %d reads and one close", methods, wantReads)
			}
			if methods[0] != "Page.printToPDF" || methods[len(methods)-1] != "IO.close" {
				t.Fatalf("cdp calls = %v", methods)
			}
			for _, method := range methods[1 : len(methods)-1] {
				if method != "IO.read" {
					t.Fatalf("cdp calls = %v", methods)
				}
			}

			// The reader holds one chunk, so it must have asked for a bounded size.
			stub.mu.Lock()
			readParams := stub.calls[1].Params
			stub.mu.Unlock()
			size, _ := readParams["size"].(float64)
			if int64(size) != int64(browser.PDFStreamChunkBytes) {
				t.Fatalf("IO.read size = %v, want the bounded chunk size %d", readParams["size"], browser.PDFStreamChunkBytes)
			}

			// A closed stream stays closed rather than reading a released handle.
			if _, err := stream.Read(make([]byte, 8)); err == nil {
				t.Fatal("read succeeded after the stream was closed")
			}
			if closeErr := stream.Close(); closeErr != nil {
				t.Fatalf("second close = %v, want a no-op", closeErr)
			}
			if extra := len(stub.methods()); extra != len(methods) {
				t.Fatalf("cdp calls after a second close = %d, want %d", extra, len(methods))
			}
		})
	}
}

// TestBridgeCapturePDFStreamClosesTheHandleAfterCancellation is the leak this
// path is most likely to hit. artifact.Service closes the stream in a defer
// after the copy, so a capture whose context died mid-copy is exactly when
// IO.close is issued — and issuing it on the dead context would leave the
// browser holding the whole rendered document for the life of the tab.
func TestBridgeCapturePDFStreamClosesTheHandleAfterCancellation(t *testing.T) {
	b := New("", 5*time.Second, "")
	body := bytes.Repeat([]byte("%PDF-cancelled\n"), 200)
	stub := chunkedPDFStub(body, 128, true)
	cleanup := serveCDPStub(t, b, stub)
	defer cleanup()

	ctx, cancel := context.WithCancel(browser.WithTabID(context.Background(), "42"))
	stream, err := b.CapturePDFStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(io.Discard, stream, 64); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	cancel()

	if _, err := io.ReadAll(stream); err == nil {
		t.Fatal("reads continued after the capture context was cancelled")
	}
	if closeErr := stream.Close(); closeErr != nil {
		t.Fatalf("close after cancellation = %v; the browser-side handle was not released", closeErr)
	}
	methods := stub.methods()
	if methods[len(methods)-1] != "IO.close" {
		t.Fatalf("cdp calls = %v, want the handle released last", methods)
	}
}
