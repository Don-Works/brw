package browser

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"github.com/chromedp/cdproto/cdp"
	cdpio "github.com/chromedp/cdproto/io"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// PDFStreamChunkBytes is how much of a rendered PDF is pulled across CDP at a
// time. Chunking is the whole point of the streaming path: with ReturnAsBase64
// the daemon holds the entire document as a base64 string AND as decoded bytes
// before one byte reaches the artifact store, so a 50 MiB print costs well over
// 100 MiB of heap. Reading in chunks holds one chunk.
const PDFStreamChunkBytes = 256 << 10

// CapturePDFStream renders the active page and returns the PDF as a stream.
// Page.printToPDF is asked for a stream handle and IO.read pulls it one bounded
// chunk at a time. The caller MUST Close the reader: until then it owns the
// browser-side stream handle, and a handle that is never closed pins the whole
// rendered document in the browser process for the life of the tab.
//
// Every CDP round trip here gets its own deadline derived from the tab context,
// rather than one deadline covering the render and the whole transfer. A 50 MiB
// print is hundreds of IO.read calls plus the store's disk writes; charging all
// of that to the single per-operation timeout would fail exactly the documents
// streaming exists for, and would fail them with the browser-side handle still
// held, because the close would run on the context that had just expired.
func (m *Manager) CapturePDFStream(ctx context.Context) (io.ReadCloser, error) {
	tabCtx, err := m.tabContextFor(ctx)
	if err != nil {
		return nil, err
	}

	var handle cdpio.StreamHandle
	if err := m.runPDFStreamOp(tabCtx, func(runCtx context.Context) error {
		_, stream, printErr := page.PrintToPDF().
			WithPrintBackground(true).
			WithTransferMode(page.PrintToPDFTransferModeReturnAsStream).
			Do(runCtx)
		if printErr != nil {
			return printErr
		}
		handle = stream
		return nil
	}); err != nil {
		return nil, err
	}
	if handle == "" {
		return nil, errors.New("browser returned no PDF stream handle")
	}
	return &cdpStreamReader{
		handle: handle,
		chunk:  m.pdfStreamChunkBytes(),
		read: func(h cdpio.StreamHandle, size int64) (string, bool, bool, error) {
			// IO.read is executed directly rather than through the generated
			// helper, whose Do discards the base64Encoded flag. A PDF stream is
			// always base64, but decoding on the flag the browser actually
			// reported keeps this correct for any stream that is not.
			var result cdpio.ReadReturns
			params := cdpio.Read(h).WithSize(size)
			if err := m.runPDFStreamOp(tabCtx, func(c context.Context) error {
				return cdp.Execute(c, cdpio.CommandRead, params, &result)
			}); err != nil {
				return "", false, false, err
			}
			return result.Data, result.Base64encoded, result.EOF, nil
		},
		closeStream: func() error {
			// A fresh budget on the tab context, never the one the failed read ran
			// under: a capture that timed out or was cancelled must still release
			// the handle, which is precisely when it is most likely to leak.
			return m.runPDFStreamOp(tabCtx, func(runCtx context.Context) error {
				return cdpio.Close(handle).Do(runCtx)
			})
		},
	}, nil
}

// runPDFStreamOp executes one CDP round trip under its own deadline derived
// from the tab context.
func (m *Manager) runPDFStreamOp(tabCtx context.Context, fn func(context.Context) error) error {
	opCtx, cancel := context.WithTimeout(tabCtx, m.timeout)
	defer cancel()
	return chromedp.Run(opCtx, chromedp.ActionFunc(fn))
}

// pdfStreamChunkBytes is the chunk size this manager uses. It is a field rather
// than a package variable so a test can lower it on its own manager and drive
// the multi-chunk path without a quarter-megabyte document, and without racing
// any other capture in the process.
func (m *Manager) pdfStreamChunkBytes() int64 {
	if m.pdfStreamChunk > 0 {
		return m.pdfStreamChunk
	}
	return PDFStreamChunkBytes
}

// cdpStreamReader turns a CDP IO stream handle into an io.ReadCloser holding at
// most one decoded chunk. That bound is what lets the artifact store persist a
// large capture without the payload ever existing whole in the daemon heap.
type cdpStreamReader struct {
	handle      cdpio.StreamHandle
	chunk       int64
	read        func(cdpio.StreamHandle, int64) (string, bool, bool, error)
	closeStream func() error

	pending []byte
	eof     bool
	closed  bool
	err     error
}

func (r *cdpStreamReader) Read(p []byte) (int, error) {
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
		data, encoded, eof, err := r.read(r.handle, r.chunk)
		if err != nil {
			r.err = err
			return 0, err
		}
		r.eof = eof
		if encoded {
			decoded, decodeErr := base64.StdEncoding.DecodeString(data)
			if decodeErr != nil {
				r.err = fmt.Errorf("decode PDF stream chunk: %w", decodeErr)
				return 0, r.err
			}
			r.pending = decoded
		} else {
			r.pending = []byte(data)
		}
		if len(r.pending) == 0 && eof {
			return 0, io.EOF
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *cdpStreamReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.pending = nil
	if r.closeStream == nil {
		return nil
	}
	return r.closeStream()
}
