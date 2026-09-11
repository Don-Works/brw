package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

// The dashboard shows a live picture of the browser so a human can watch the
// agent work on their own signed-in profile and step in when it needs them.
//
// It is OFF unless BRW_DASHBOARD=1 and it refuses non-loopback requests even
// when the daemon itself is bound wider. A trace stream leaks URLs and titles;
// this leaks the rendered pixels of a logged-in session — inbox contents, bank
// balances, anything on screen. That is not something to expose by default or
// over a network because the daemon happened to be reachable.
const dashboardEnvVar = "BRW_DASHBOARD"

// screencaster is the direct-CDP fast path: Chrome pushes a frame only when the
// page actually changes, so an idle page costs nothing.
type screencaster interface {
	ScreencastFrames(context.Context, browser.ScreencastOptions) (<-chan browser.ScreencastFrame, func(), error)
}

func dashboardEnabled() bool {
	value := strings.TrimSpace(os.Getenv(dashboardEnvVar))
	return value == "1" || strings.EqualFold(value, "true") || strings.EqualFold(value, "on")
}

// dashboardGuard enforces the two conditions under which live pixels may leave
// the daemon.
func (s *Server) dashboardGuard(w http.ResponseWriter, r *http.Request) bool {
	if !dashboardEnabled() {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "the brw dashboard is off. It streams the rendered pixels of a signed-in browser, so it is opt-in: restart the daemon with " + dashboardEnvVar + "=1 to enable it.",
		})
		return false
	}
	if !requestIsLoopback(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "the brw dashboard only serves loopback clients. To watch a remote browser, forward the port over SSH (ssh -L) so the pixels travel inside the tunnel rather than over the network.",
		})
		return false
	}
	return true
}

// requestIsLoopback reports whether the peer is on this machine. The daemon may
// be bound to a Tailscale or LAN address for the API; that is not consent to
// stream the screen to those clients.
func requestIsLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// dashboardFrame is one rendered frame pushed to a viewer.
type dashboardFrame struct {
	Seq        int    `json:"seq"`
	JPEGBase64 string `json:"jpeg_base64"`
	At         string `json:"at"`
}

// dashboardStream is GET /dashboard/stream: a server-sent event stream of JPEG
// frames.
//
// SSE rather than a WebSocket because the stream is one-directional in phase 1
// (read-only viewing), and SSE reconnects on its own, needs no upgrade
// handshake, and cannot be used to send input at a stage where input is not
// implemented.
//
// Pacing: a viewer sets fps, and frames are dropped rather than queued when the
// viewer cannot keep up, so a slow tab of a browser never becomes back-pressure
// on the browser itself. The newest frame always wins; a stale frame is worth
// nothing to someone watching live.
func (s *Server) dashboardStream(w http.ResponseWriter, r *http.Request) {
	if !s.dashboardGuard(w, r) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, fmt.Errorf("streaming is unavailable: the response writer cannot flush"))
		return
	}

	fps := clampInt(queryInt(r, "fps", 4), 1, 30)
	quality := clampInt(queryInt(r, "quality", 60), 10, 95)
	maxWidth := clampInt(queryInt(r, "width", 1024), 320, 2560)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": brw dashboard stream\n\n")
	flusher.Flush()

	ctx := r.Context()
	interval := time.Second / time.Duration(fps)

	if caster, isCaster := s.manager.(screencaster); isCaster {
		s.streamViaScreencast(ctx, w, flusher, caster, browser.ScreencastOptions{
			Quality: quality, MaxWidth: maxWidth,
		}, interval)
		return
	}
	// Extension-bridge lane: no compositor stream, so poll screenshots while a
	// viewer is watching. This is why the dashboard works against the user's
	// real signed-in Chrome at all.
	s.streamViaScreenshots(ctx, w, flusher, interval)
}

func (s *Server) streamViaScreencast(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, caster screencaster, opts browser.ScreencastOptions, interval time.Duration) {
	frames, stop, err := caster.ScreencastFrames(ctx, opts)
	if err != nil {
		reason, _ := json.Marshal(err.Error())
		fmt.Fprintf(w, "event: error\ndata: {\"error\":%s}\n\n", reason)
		flusher.Flush()
		return
	}
	defer stop()

	seq := 0
	var pending []byte
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case frame, open := <-frames:
			if !open {
				return
			}
			// Keep only the newest: a viewer wants now, not a backlog.
			pending = frame.Data
		case <-ticker.C:
			if pending == nil {
				continue
			}
			seq++
			if !writeFrame(w, flusher, seq, pending) {
				return
			}
			pending = nil
		}
	}
}

func (s *Server) streamViaScreenshots(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	seq := 0
	var lastLen int
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			shotCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			shot, err := s.manager.Screenshot(shotCtx)
			cancel()
			if err != nil {
				// A transient failure (tab navigating, briefly occluded) must not
				// end the stream the human is watching.
				continue
			}
			data, decodeErr := base64.StdEncoding.DecodeString(shot.Base64)
			if decodeErr != nil {
				continue
			}
			// A byte-identical length is a cheap proxy for "nothing changed" and
			// keeps an idle page from burning bandwidth every tick.
			if len(data) == lastLen && seq > 0 {
				continue
			}
			lastLen = len(data)
			seq++
			if !writeFrame(w, flusher, seq, data) {
				return
			}
		}
	}
}

func writeFrame(w http.ResponseWriter, flusher http.Flusher, seq int, data []byte) bool {
	payload, err := json.Marshal(dashboardFrame{
		Seq:        seq,
		At:         time.Now().UTC().Format(time.RFC3339),
		JPEGBase64: base64.StdEncoding.EncodeToString(data),
	})
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "event: frame\ndata: %s\n\n", payload); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

func queryInt(r *http.Request, name string, fallback int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// dashboardPage is GET /dashboard.
func (s *Server) dashboardPage(w http.ResponseWriter, r *http.Request) {
	if !s.dashboardGuard(w, r) {
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	// The page renders frames the browser produced. A strict CSP keeps that
	// content from doing anything but being displayed.
	h.Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	fmt.Fprint(w, dashboardHTML)
}

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>brw dashboard</title>
<style>
  :root { color-scheme: light dark; --bg:#0f1115; --fg:#e6e8ec; --muted:#8b93a1; --line:#262a33; }
  * { box-sizing: border-box; }
  body { margin:0; background:var(--bg); color:var(--fg); font:14px/1.5 ui-sans-serif,system-ui,-apple-system,Segoe UI,Roboto,sans-serif; }
  header { display:flex; gap:12px; align-items:center; flex-wrap:wrap;
           padding:10px 16px; border-bottom:1px solid var(--line); }
  h1 { font-size:14px; font-weight:600; margin:0; letter-spacing:.02em; }
  .dot { width:8px; height:8px; border-radius:50%; background:var(--muted); }
  .dot.live { background:#37d67a; }
  .spacer { flex:1; }
  label { color:var(--muted); display:inline-flex; gap:6px; align-items:center; }
  select { background:#171a21; color:var(--fg); border:1px solid var(--line);
           border-radius:6px; padding:4px 8px; font:inherit; }
  main { padding:16px; display:flex; justify-content:center; }
  figure { margin:0; max-width:100%; }
  img { max-width:100%; height:auto; display:block; border:1px solid var(--line); border-radius:8px; background:#000; }
  figcaption { color:var(--muted); margin-top:8px; font-size:12px; }
  .empty { color:var(--muted); padding:48px 16px; text-align:center; }
</style>
</head>
<body>
<header>
  <span class="dot" id="dot"></span>
  <h1>brw dashboard</h1>
  <span class="spacer"></span>
  <label>fps
    <select id="fps"><option>1</option><option>2</option><option selected>4</option><option>8</option><option>12</option></select>
  </label>
  <label>quality
    <select id="quality"><option>20</option><option>40</option><option selected>60</option><option>80</option></select>
  </label>
  <label>width
    <select id="width"><option>640</option><option selected>1024</option><option>1440</option></select>
  </label>
</header>
<main>
  <figure>
    <img id="frame" alt="Live view of the browser brw is driving">
    <figcaption id="status">connecting…</figcaption>
  </figure>
</main>
<div class="empty" id="empty">Waiting for the first frame. An idle page sends nothing until it changes.</div>
<script>
(function(){
  var img = document.getElementById('frame');
  var status = document.getElementById('status');
  var empty = document.getElementById('empty');
  var dot = document.getElementById('dot');
  var source = null;
  var frames = 0;

  function connect(){
    if (source) source.close();
    var params = new URLSearchParams({
      fps: document.getElementById('fps').value,
      quality: document.getElementById('quality').value,
      width: document.getElementById('width').value
    });
    source = new EventSource('/dashboard/stream?' + params.toString());
    status.textContent = 'connecting…';
    dot.classList.remove('live');
    source.addEventListener('frame', function(event){
      var payload = JSON.parse(event.data);
      img.src = 'data:image/jpeg;base64,' + payload.jpeg_base64;
      frames++;
      empty.style.display = 'none';
      dot.classList.add('live');
      status.textContent = 'frame ' + payload.seq + ' · ' + payload.at;
    });
    source.addEventListener('error', function(event){
      // EventSource retries on its own; say so rather than looking dead.
      dot.classList.remove('live');
      status.textContent = frames ? 'reconnecting…' : 'waiting for the daemon…';
    });
  }

  ['fps','quality','width'].forEach(function(id){
    document.getElementById(id).addEventListener('change', connect);
  });
  connect();
})();
</script>
</body>
</html>
`
