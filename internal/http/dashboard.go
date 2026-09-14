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
//
// Width and Height are the page's own DIP viewport, not the JPEG's pixel size.
// A viewer scales the image into whatever element it has; without the page's
// dimensions it cannot turn a click on that element back into the coordinates
// Input.dispatchMouseEvent wants, so takeover would be aiming blind.
type dashboardFrame struct {
	Seq        int     `json:"seq"`
	JPEGBase64 string  `json:"jpeg_base64"`
	At         string  `json:"at"`
	Width      float64 `json:"width,omitempty"`
	Height     float64 `json:"height,omitempty"`
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
	var pending *browser.ScreencastFrame
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
			newest := frame
			pending = &newest
		case <-ticker.C:
			if pending == nil {
				continue
			}
			seq++
			if !writeFrame(w, flusher, seq, pending.Data, pending.Width, pending.Height) {
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
			// No page geometry here: a screenshot carries none, and this lane is
			// the bridge transport, which offers no takeover to aim anyway.
			if !writeFrame(w, flusher, seq, data, 0, 0) {
				return
			}
		}
	}
}

func writeFrame(w http.ResponseWriter, flusher http.Flusher, seq int, data []byte, width, height float64) bool {
	payload, err := json.Marshal(dashboardFrame{
		Seq:        seq,
		At:         time.Now().UTC().Format(time.RFC3339),
		JPEGBase64: base64.StdEncoding.EncodeToString(data),
		Width:      width,
		Height:     height,
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
//
// The takeover control is assembled into the page only when this daemon may
// offer it. On a daemon bound beyond loopback, or one bridging to another
// browser, the markup and its script are not in the response at all — there is
// no disabled button to re-enable and no handler to call from the console.
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
	fmt.Fprint(w, s.renderDashboard())
}

func (s *Server) renderDashboard() string {
	control, script := "", ""
	if s.takeoverAvailable() {
		control, script = dashboardTakeoverControl, dashboardTakeoverScript
	}
	page := strings.Replace(dashboardHTML, dashboardTakeoverControlSlot, control, 1)
	return strings.Replace(page, dashboardTakeoverScriptSlot, script, 1)
}

const (
	dashboardTakeoverControlSlot = "<!--takeover-control-->"
	dashboardTakeoverScriptSlot  = "<!--takeover-script-->"
)

// dashboardTakeoverControl is the enable switch. Absent, not disabled, on any
// daemon that may not offer takeover.
const dashboardTakeoverControl = `<button id="takeover" type="button" class="takeover">take over</button>`

// dashboardTakeoverScript forwards the human's pointer and keyboard to the tab.
// Coordinates are mapped from the rendered image back onto the page's own DIP
// viewport, which every frame carries; a frame with no geometry (the bridge
// lane) leaves takeover inert rather than guessing.
const dashboardTakeoverScript = `<script>
(function(){
  var button = document.getElementById('takeover');
  var frame = document.getElementById('frame');
  var status = document.getElementById('takeoverStatus');
  var token = null;
  var beat = null;

  function post(path, body){
    return fetch(path, {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify(body)
    }).then(function(response){
      return response.json().then(function(payload){
        if (!response.ok) throw new Error(payload.error || ('HTTP ' + response.status));
        return payload;
      });
    });
  }

  function modifiers(ev){
    return (ev.altKey ? 1 : 0) | (ev.ctrlKey ? 2 : 0) | (ev.metaKey ? 4 : 0) | (ev.shiftKey ? 8 : 0);
  }

  function point(ev){
    var box = frame.getBoundingClientRect();
    var size = window.brwFrameSize;
    if (!size || !size.width || !size.height || !box.width || !box.height) return null;
    return {
      x: (ev.clientX - box.left) / box.width * size.width,
      y: (ev.clientY - box.top) / box.height * size.height
    };
  }

  function send(event){
    if (!token) return;
    post('/dashboard/input', {token: token, event: event}).catch(function(err){
      status.textContent = 'input refused: ' + err.message;
      stop();
    });
  }

  function mouse(type){
    return function(ev){
      var at = point(ev);
      if (!at) return;
      ev.preventDefault();
      send({
        kind: 'mouse', type: type, x: at.x, y: at.y,
        button: ev.button === 2 ? 'right' : (ev.button === 1 ? 'middle' : 'left'),
        buttons: type === 'mousePressed' ? 1 : 0,
        click_count: type === 'mouseMoved' ? 0 : 1,
        modifiers: modifiers(ev)
      });
    };
  }

  function key(type){
    return function(ev){
      ev.preventDefault();
      send({
        kind: 'key', type: type, key: ev.key, code: ev.code,
        text: (type === 'keyDown' && ev.key.length === 1) ? ev.key : '',
        windows_virtual_key_code: ev.keyCode || 0,
        modifiers: modifiers(ev)
      });
    };
  }

  var onDown = mouse('mousePressed');
  var onUp = mouse('mouseReleased');
  var onMove = mouse('mouseMoved');
  var onKeyDown = key('keyDown');
  var onKeyUp = key('keyUp');
  var moveAt = 0;
  function throttledMove(ev){
    var now = Date.now();
    if (now - moveAt < 50) return;
    moveAt = now;
    onMove(ev);
  }

  function listen(on){
    var method = on ? 'addEventListener' : 'removeEventListener';
    frame[method]('mousedown', onDown);
    frame[method]('mouseup', onUp);
    frame[method]('mousemove', throttledMove);
    frame[method]('contextmenu', prevent);
    window[method]('keydown', onKeyDown);
    window[method]('keyup', onKeyUp);
  }
  function prevent(ev){ ev.preventDefault(); }

  function start(){
    post('/dashboard/takeover', {action: 'acquire'}).then(function(grant){
      token = grant.token;
      listen(true);
      frame.classList.add('driving');
      button.textContent = 'release';
      status.textContent = 'you are driving; agent actions are refused until you release';
      beat = setInterval(function(){
        post('/dashboard/takeover', {action: 'renew', token: token}).catch(stop);
      }, 45000);
    }).catch(function(err){
      status.textContent = err.message;
    });
  }

  function stop(){
    var held = token;
    token = null;
    if (beat) { clearInterval(beat); beat = null; }
    listen(false);
    frame.classList.remove('driving');
    button.textContent = 'take over';
    status.textContent = 'the agent has the browser';
    if (held) post('/dashboard/takeover', {action: 'release', token: held}).catch(function(){});
  }

  button.addEventListener('click', function(){ token ? stop() : start(); });
  // A closed tab must not leave the agent locked out until the grant expires.
  window.addEventListener('pagehide', function(){ if (token) stop(); });
})();
</script>`

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>brw dashboard</title>
<style>
  :root { color-scheme: light dark; --bg:#0f1115; --fg:#e6e8ec; --muted:#8b93a1; --line:#262a33;
          --ok:#37d67a; --bad:#e5576b; }
  * { box-sizing: border-box; }
  body { margin:0; background:var(--bg); color:var(--fg); font:14px/1.5 ui-sans-serif,system-ui,-apple-system,Segoe UI,Roboto,sans-serif; }
  header { display:flex; gap:12px; align-items:center; flex-wrap:wrap;
           padding:10px 16px; border-bottom:1px solid var(--line); }
  h1 { font-size:14px; font-weight:600; margin:0; letter-spacing:.02em; }
  .dot { width:8px; height:8px; border-radius:50%; background:var(--muted); }
  .dot.live { background:var(--ok); }
  .spacer { flex:1; }
  label { color:var(--muted); display:inline-flex; gap:6px; align-items:center; }
  select { background:#171a21; color:var(--fg); border:1px solid var(--line);
           border-radius:6px; padding:4px 8px; font:inherit; }
  button.takeover { background:#171a21; color:var(--fg); border:1px solid var(--line);
                    border-radius:6px; padding:4px 10px; font:inherit; cursor:pointer; }
  button.takeover:hover { border-color:var(--ok); }
  main { padding:16px; display:grid; gap:16px; grid-template-columns:minmax(0,1fr) 340px; align-items:start; }
  @media (max-width: 900px) { main { grid-template-columns:minmax(0,1fr); } }
  figure { margin:0; max-width:100%; }
  img { max-width:100%; height:auto; display:block; border:1px solid var(--line); border-radius:8px; background:#000; }
  img.driving { border-color:var(--ok); cursor:crosshair; }
  figcaption { color:var(--muted); margin-top:8px; font-size:12px; }
  aside { border:1px solid var(--line); border-radius:8px; overflow:hidden; }
  aside h2 { font-size:12px; font-weight:600; margin:0; padding:8px 12px; color:var(--muted);
             border-bottom:1px solid var(--line); letter-spacing:.04em; text-transform:uppercase; }
  ol { list-style:none; margin:0; padding:0; max-height:70vh; overflow-y:auto; }
  li { display:grid; grid-template-columns:auto 1fr auto; gap:8px; align-items:baseline;
       padding:6px 12px; border-bottom:1px solid var(--line); font-size:12px; }
  li:last-child { border-bottom:0; }
  .action { font-weight:600; }
  .target { color:var(--muted); overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .latency { color:var(--muted); font-variant-numeric:tabular-nums; }
  li.failed .action { color:var(--bad); }
  li.human .action { color:var(--ok); }
  .empty { color:var(--muted); padding:24px 12px; text-align:center; font-size:12px; }
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
  <!--takeover-control-->
</header>
<main>
  <figure>
    <img id="frame" alt="Live view of the browser brw is driving">
    <figcaption id="status">connecting…</figcaption>
    <figcaption id="takeoverStatus">the agent has the browser</figcaption>
  </figure>
  <aside>
    <h2>activity</h2>
    <ol id="feed"></ol>
    <div class="empty" id="feedEmpty">No actions yet. Every step the agent takes appears here.</div>
  </aside>
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
  window.brwFrameSize = null;

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
      if (payload.width && payload.height) {
        window.brwFrameSize = {width: payload.width, height: payload.height};
      }
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
<script>
(function(){
  var feed = document.getElementById('feed');
  var feedEmpty = document.getElementById('feedEmpty');
  var activity = new EventSource('/dashboard/activity');

  function cell(className, text){
    var node = document.createElement('span');
    node.className = className;
    node.textContent = text;
    return node;
  }

  activity.addEventListener('action', function(event){
    var line = JSON.parse(event.data);
    var row = document.createElement('li');
    if (line.outcome !== 'ok') row.className = 'failed';
    if (line.action === 'human_input') row.className = 'human';
    var target = line.name || line.ref || '';
    if (line.name && line.ref) target = line.name + ' (' + line.ref + ')';
    if (line.outcome !== 'ok' && line.error) target = line.error;
    row.appendChild(cell('action', line.action));
    row.appendChild(cell('target', target));
    row.appendChild(cell('latency', line.duration_ms + 'ms'));
    feed.insertBefore(row, feed.firstChild);
    feedEmpty.style.display = 'none';
    // The feed is a live view, not a log: an unbounded list is a leak on a
    // page that may be left open for hours.
    while (feed.childNodes.length > 200) feed.removeChild(feed.lastChild);
  });
})();
</script>
<!--takeover-script-->
</body>
</html>
`
