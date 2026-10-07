package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

const streamHeartbeat = 15 * time.Second

type traceSubscriberController interface {
	SubscribeTrace() (<-chan browser.TraceEntry, func())
}

func (s *Server) sessionStream(w http.ResponseWriter, r *http.Request) {
	sub, ok := s.manager.(traceSubscriberController)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "live session streaming needs a daemon that drives the browser over CDP itself; this one proxies or bridges to another one, so subscribe on that daemon instead",
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

	watchAll := strings.EqualFold(strings.TrimSpace(os.Getenv("BRW_STREAM_SCOPE")), "all")
	owner := leaseOwner(r.Context())
	visible := func(entry browser.TraceEntry) bool {
		if watchAll || entry.TabID == "" {
			return true
		}
		return owner != "" && s.leases.ownsTab(owner, entry.TabID)
	}
	var withheld uint64

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")

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
			if !visible(entry) {

				withheld++
				if _, err := fmt.Fprintf(w, "event: withheld\ndata: {\"withheld\":%d}\n\n", withheld); err != nil {
					return
				}
				flusher.Flush()
				continue
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
