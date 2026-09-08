package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

// streamHeartbeat keeps the connection and any intermediary alive while the
// browser is idle. An idle session costs one comment frame per interval and
// nothing else — there is no polling here, entries are pushed.
const streamHeartbeat = 15 * time.Second

// traceSubscriber is implemented by controllers that can stream actions live.
// Direct CDP does; the extension bridge and the upstream-HTTP proxy do not,
// and get a plain 501 rather than a silent empty stream.
type traceSubscriberController interface {
	SubscribeTrace() (<-chan browser.TraceEntry, func())
}

// sessionStream is GET /api/session/stream: server-sent events, one per browser
// action, for watching a flow while it runs.
//
// Entries come off Manager.SubscribeTrace, which publishes from the same
// choke point that fills the trace ring buffer — after ref identity is
// resolved and after credential redaction. A password fill arrives here as an
// action with redacted:true and no value, exactly as it appears in
// /api/page/trace.
func (s *Server) sessionStream(w http.ResponseWriter, r *http.Request) {
	sub, ok := s.manager.(traceSubscriberController)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "live session streaming needs the direct-CDP transport; this daemon proxies or bridges to another one, so subscribe on that daemon instead",
		})
		return
	}
	flusher, isFlusher := w.(http.Flusher)
	if !isFlusher {
		writeError(w, errors.New("streaming is unavailable: the response writer cannot flush"))
		return
	}

	entries, cancel := sub.SubscribeTrace()
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	// The stream is page-derived text. Never let an intermediary transform it.
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, ": brw session stream\n\n")
	flusher.Flush()

	ticker := time.NewTicker(streamHeartbeat)
	defer ticker.Stop()

	ctx := r.Context()
	var seq uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case entry, open := <-entries:
			if !open {
				return
			}
			seq++
			payload, err := json.Marshal(struct {
				Seq uint64 `json:"seq"`
				browser.TraceEntry
			}{Seq: seq, TraceEntry: entry})
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: action\ndata: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
