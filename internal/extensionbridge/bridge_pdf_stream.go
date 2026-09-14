package extensionbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Don-Works/brw/internal/browser"
)

// CapturePDFStream renders the current page through the extension's CDP bridge
// and returns the PDF as a stream, so the browser host never holds the whole
// document. Parity with the direct-CDP transport is deliberate: a capability
// present on one transport and silently absent on the other turns a memory
// guarantee into a coin toss, decided by which profile a caller happens to be
// driving.
func (b *Bridge) CapturePDFStream(ctx context.Context) (io.ReadCloser, error) {
	tabID := b.contextTabID(ctx)
	raw, err := b.cdp(ctx, tabID, "Page.printToPDF", map[string]any{
		"printBackground": true,
		"transferMode":    "ReturnAsStream",
	})
	if err != nil {
		return nil, err
	}
	var payload struct {
		Stream string `json:"stream"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Stream == "" {
		return nil, errors.New("Page.printToPDF returned no stream handle")
	}
	return &bridgeStreamReader{bridge: b, ctx: ctx, tabID: tabID, handle: payload.Stream}, nil
}

// bridgeStreamReader holds one decoded chunk at a time. It keeps the request
// context because IO.read is a round trip like any other bridge call and must
// stay cancellable by the caller that started the capture.
type bridgeStreamReader struct {
	bridge *Bridge
	ctx    context.Context
	tabID  string
	handle string

	pending []byte
	eof     bool
	closed  bool
	err     error
}

func (r *bridgeStreamReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, errors.New("PDF stream is closed")
	}
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.eof {
			return 0, io.EOF
		}
		raw, err := r.bridge.cdp(r.ctx, r.tabID, "IO.read", map[string]any{
			"handle": r.handle,
			"size":   browser.PDFStreamChunkBytes,
		})
		if err != nil {
			r.err = err
			return 0, err
		}
		var payload struct {
			Data          string `json:"data"`
			Base64Encoded bool   `json:"base64Encoded"`
			EOF           bool   `json:"eof"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			r.err = err
			return 0, err
		}
		r.eof = payload.EOF
		if payload.Base64Encoded {
			decoded, decodeErr := base64.StdEncoding.DecodeString(payload.Data)
			if decodeErr != nil {
				r.err = fmt.Errorf("decode IO.read chunk: %w", decodeErr)
				return 0, r.err
			}
			r.pending = decoded
		} else {
			r.pending = []byte(payload.Data)
		}
		if len(r.pending) == 0 && payload.EOF {
			return 0, io.EOF
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// Close releases the browser-side handle. A stream left open pins the rendered
// document in the browser process for the life of the tab.
//
// The close deliberately does NOT inherit the caller's cancellation. A capture
// that was cancelled or timed out mid-copy is exactly when the handle is most
// likely to leak, and issuing IO.close on the context that just died would
// guarantee it does. Bridge.call still bounds this with its own timeout.
func (r *bridgeStreamReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.pending = nil
	_, err := r.bridge.cdp(context.WithoutCancel(r.ctx), r.tabID, "IO.close", map[string]any{"handle": r.handle})
	return err
}
