package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

const (
	takeoverHeartbeat = 45 * time.Second

	takeoverInputBodyBytes = 4 << 10
)

type takeoverController interface {
	AcquireTakeover(string, time.Duration) (browser.TakeoverGrant, error)
	RenewTakeover(string, time.Duration) (browser.TakeoverGrant, error)
	ReleaseTakeover(string) error
	TakeoverState() browser.TakeoverStatus
	DispatchTakeoverInput(context.Context, string, browser.TakeoverInput) error
}

func (s *Server) takeoverAvailable() bool {
	if !s.loopbackBind {
		return false
	}
	_, ok := s.manager.(takeoverController)
	return ok
}

func (s *Server) takeoverGuard(w http.ResponseWriter, r *http.Request) (takeoverController, bool) {
	if !s.dashboardGuard(w, r) {
		return nil, false
	}
	if !s.loopbackBind {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "takeover is not available on this daemon: brwd is bound beyond loopback, and forwarding input to a signed-in browser over a network is not something a bind address can consent to. Restart brwd on 127.0.0.1 and reach it over an SSH tunnel.",
		})
		return nil, false
	}
	controller, ok := s.manager.(takeoverController)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "takeover needs a daemon that drives the browser over CDP itself; this one proxies or bridges to another one, so take over on that daemon instead",
		})
		return nil, false
	}
	return controller, true
}

func (s *Server) dashboardTakeover(w http.ResponseWriter, r *http.Request) {
	controller, ok := s.takeoverGuard(w, r)
	if !ok {
		return
	}
	var req struct {
		Action string `json:"action"`
		Token  string `json:"token"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "acquire":
		grant, err := controller.AcquireTakeover("dashboard", takeoverHeartbeat*2)
		if err != nil {
			takeoverFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, grant)
	case "renew":
		grant, err := controller.RenewTakeover(req.Token, takeoverHeartbeat*2)
		if err != nil {
			takeoverFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, grant)
	case "release":
		if err := controller.ReleaseTakeover(req.Token); err != nil {
			takeoverFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, controller.TakeoverState())
	case "status":
		writeJSON(w, http.StatusOK, controller.TakeoverState())
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "action must be one of acquire, renew, release, status",
		})
	}
}

func (s *Server) dashboardInput(w http.ResponseWriter, r *http.Request) {
	controller, ok := s.takeoverGuard(w, r)
	if !ok {
		return
	}
	var req struct {
		Token string                `json:"token"`
		Event browser.TakeoverInput `json:"event"`
	}
	if err := decodeBody(w, r, &req, takeoverInputBodyBytes, true); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := controller.DispatchTakeoverInput(r.Context(), req.Token, req.Event); err != nil {
		takeoverFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func takeoverFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, browser.ErrTakeoverNotHeld):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error()})
	case errors.Is(err, browser.ErrTakeoverBusy):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
	default:
		writeError(w, err)
	}
}

func (s *Server) dashboardActivity(w http.ResponseWriter, r *http.Request) {
	if !s.dashboardGuard(w, r) {
		return
	}
	sub, ok := s.manager.(traceSubscriberController)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "the activity feed needs a daemon that drives the browser over CDP itself; this one proxies or bridges to another one, so watch it on that daemon instead",
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
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": brw activity feed\n\n")
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
			payload, err := json.Marshal(activityLine(seq, entry))
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

// ActivityLine is one row of the feed.
type ActivityLine struct {
	Seq        uint64 `json:"seq"`
	Action     string `json:"action"`
	Ref        string `json:"ref,omitempty"`
	Name       string `json:"name,omitempty"`
	Role       string `json:"role,omitempty"`
	Outcome    string `json:"outcome"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Redacted   bool   `json:"redacted,omitempty"`
	At         string `json:"at"`
}

var activityURL = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"'<>)\]]+`)

func scrubActivityError(message string) string {
	return activityURL.ReplaceAllString(message, "<url>")
}

func activityLine(seq uint64, entry browser.TraceEntry) ActivityLine {
	outcome := "ok"
	if !entry.OK {
		outcome = "failed"
	}
	at := entry.Timestamp
	if strings.TrimSpace(at) == "" {
		at = time.Now().UTC().Format(time.RFC3339)
	}
	return ActivityLine{
		Seq:        seq,
		Action:     entry.Action,
		Ref:        entry.Ref,
		Name:       entry.Name,
		Role:       entry.Role,
		Outcome:    outcome,
		Error:      scrubActivityError(entry.Error),
		DurationMS: entry.DurationMS,
		Redacted:   entry.Redacted,
		At:         at,
	}
}
