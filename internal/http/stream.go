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

	// Scope exactly as /api/page/trace does. The daemon is shared, so an
	// unscoped stream hands one agent session another's browsing live — on a
	// signed-in profile that means authenticated page titles and URLs. An
	// entry with no tab is daemon-wide and safe; anything else needs a lease
	// this caller holds. A request with no lease identity sees only tab-less
	// entries, because entitlement cannot be established.
	// The operator running the daemon is a different actor from the agent
	// sessions sharing it, and watching your own browser work is the whole
	// point of this endpoint. BRW_STREAM_SCOPE=all is that opt-in, set on the
	// daemon at launch rather than chosen per request, so a lease-holding
	// session cannot widen its own view by asking. Default stays closed.
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
			if !visible(entry) {
				// Count rather than drop silently, so a caller can tell a
				// filtered stream from an idle browser — same contract as the
				// trace endpoint's "withheld".
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
