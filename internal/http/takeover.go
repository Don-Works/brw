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

// The dashboard's second half: what the agent is DOING, and the ability to take
// the keyboard off it.
//
// The activity feed is the trace stream the daemon already publishes, rendered
// beside the frames. Watching a page change with no idea which action caused it
// is the failure this closes.
//
// Takeover is input forwarding, and it is gated twice over. It needs an explicit
// per-session enable, so a dashboard left open is a viewer and not a driver; and
// it needs brwd to be bound to loopback, because forwarding Input.dispatch* is
// remote control of a signed-in browser and a bind beyond loopback means the
// operator has already decided other machines may reach this daemon. On such a
// bind the control is not rendered at all and the routes do not exist, rather
// than being drawn disabled: a disabled button is a thing to be re-enabled by
// whoever finds the endpoint.
const (
	// takeoverHeartbeat is how often the page renews its hold. A grant lives for
	// twice this, so one missed beat is survivable and a closed tab still frees
	// the browser without anyone pressing release.
	takeoverHeartbeat = 45 * time.Second
	// One event per request, so the body is a handful of numbers.
	takeoverInputBodyBytes = 4 << 10
)

// takeoverController is the optional transport capability for human takeover.
// Direct CDP implements it; the extension bridge and the upstream-HTTP proxy do
// not, and get a named refusal rather than a control that does nothing.
type takeoverController interface {
	AcquireTakeover(string, time.Duration) (browser.TakeoverGrant, error)
	RenewTakeover(string, time.Duration) (browser.TakeoverGrant, error)
	ReleaseTakeover(string) error
	TakeoverState() browser.TakeoverStatus
	DispatchTakeoverInput(context.Context, string, browser.TakeoverInput) error
}

// takeoverAvailable reports whether this daemon may offer takeover at all. It is
// a property of the bind address, not of the request: a loopback peer reaching a
// daemon bound to a tailnet address is still a daemon other machines can reach.
func (s *Server) takeoverAvailable() bool {
	if !s.loopbackBind {
		return false
	}
	_, ok := s.manager.(takeoverController)
	return ok
}

// takeoverGuard applies the dashboard's own conditions and then the bind check.
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

// dashboardTakeover is POST /dashboard/takeover: acquire, renew or release the
// human's exclusive hold.
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

// dashboardInput is POST /dashboard/input: one forwarded mouse or key event.
//
// The token is re-checked inside the controller, so this handler being reachable
// is not the same as input being permitted. That duplication is deliberate: the
// HTTP edge decides whether the surface exists, the browser decides whether the
// event may reach the renderer.
func (s *Server) dashboardInput(w http.ResponseWriter, r *http.Request) {
	controller, ok := s.takeoverGuard(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, takeoverInputBodyBytes)
	var req struct {
		Token string                `json:"token"`
		Event browser.TakeoverInput `json:"event"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := controller.DispatchTakeoverInput(r.Context(), req.Token, req.Event); err != nil {
		takeoverFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// takeoverFailure maps the browser's typed refusals onto status codes. A missing
// grant is 403 rather than 400: the request was well formed and was refused.
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

// dashboardActivity is GET /dashboard/activity: the trace stream as one line per
// step — action, what it acted on, whether it worked, how long it took.
//
// Unlike /api/session/stream this is NOT scoped to a lease. The operator running
// the daemon is already watching the rendered pixels of every tab on this
// screen; withholding the action that produced them would leave the feed
// describing a browser other than the one on display. The dashboard's own two
// gates — opt-in and loopback-only — are what stands in front of it.
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

// ActivityLine is one row of the feed. It is a projection of the trace entry
// rather than the entry itself: the feed is read by a person at a glance, and a
// row that carried the entry's page text would put page content on a surface
// whose job is to say what happened, not what the page said.
//
// Error is the one field whose CONTENT is not chosen here — it is whatever the
// failing action said — so a failed navigate_to or open would otherwise carry
// its target URL into the row. Addresses are replaced before the row is built;
// see scrubActivityError.
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

// activityURL matches an absolute URL anywhere in a failure reason. Deliberately
// greedy about schemes rather than about hosts: the point is that no address
// reaches the row, not that the row explains which one it was.
var activityURL = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"'<>)\]]+`)

// scrubActivityError keeps a failure's reason and drops the address in it. An
// operator needs to know a navigate failed and why; the URL it was aimed at is
// page-derived and belongs to the tab they are already watching, not to a feed
// served beside it.
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
