package extensionbridge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Don-Works/brw/internal/actions"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/accessibility"
	"github.com/coder/websocket"
)

type Bridge struct {
	addr               string
	timeout            time.Duration
	allowedExtensionID string
	identity           brwidentity.Identity
	server             *http.Server

	// authToken, when non-empty, is a per-launch shared secret the extension may
	// present in its hello. The daemon serves it over the loopback /status
	// endpoint (which a browser web page cannot read cross-origin), so the real
	// 0.2.0+ extension can prove itself. A WRONG token is always rejected; a
	// MISSING token is accepted unless requireToken is set (graceful: upgrading
	// the daemon never bricks an already-installed pre-0.2.0 extension). Empty
	// disables the check entirely (library/test/embedder use); the empty-Origin
	// rejection still applies in all cases.
	authToken string
	// requireToken, when true, rejects a hello that carries no token (strict
	// mode). Default false keeps the bridge backward-compatible with an extension
	// that has not yet been reloaded to 0.2.0.
	requireToken bool
	// compatWarnOnce logs the "no token, accepting for compatibility" notice at
	// most once per daemon lifetime instead of on every MV3 reconnect.
	compatWarnOnce sync.Once

	mu      sync.RWMutex
	conn    *websocket.Conn
	hello   hello
	active  string
	pending map[string]chan response
	writeMu sync.Mutex
	nextID  atomic.Uint64

	// connReady is closed when a connection goes live and replaced with a fresh
	// open channel on every (re)connect (guarded by mu). A call that arrives
	// during the brief MV3 service-worker reconnect gap parks on it and proceeds
	// the instant the socket comes back, instead of failing with "not connected".
	connReady chan struct{}

	// sema bounds how many RPCs are on the shared socket at once. The extension
	// drives a single MV3 worker thread; without a cap, N agents firing together
	// flood it until responses stop arriving within b.timeout (the "10 heavy calls
	// wedge the bridge" failure). Excess calls queue (ctx-aware) and, past the
	// deadline, fail fast with ErrBridgeBusy so a caller backs off. nil == no cap.
	sema        chan struct{}
	maxInflight int

	// tabLocks serialize RPCs per target tab so two operations on the SAME tab
	// never interleave their CDP frames / in-page ref state (a stale-ref source).
	// Different tabs still run in parallel up to the sema cap. Keyed by tab id.
	tabLocksMu sync.Mutex
	tabLocks   map[string]chan struct{}

	// Backpressure / resilience counters, surfaced over /status for operators.
	inflight  atomic.Int64  // RPCs currently on the wire
	queued    atomic.Int64  // callers blocked waiting for an in-flight slot
	busyDrops atomic.Uint64 // calls rejected with ErrBridgeBusy (cap saturated)
	retries   atomic.Uint64 // idempotent calls retried after a transient drop

	connectedAt      time.Time
	lastSeenAt       time.Time
	disconnectedAt   time.Time
	disconnectReason string

	// cancels tracks in-flight long-running operations (plan / batch / wait
	// loops) keyed by an operation token so Cancel can stop a specific run
	// cooperatively. Mirrors the browser.Manager mechanism so cancellation
	// behaves identically across the CDP and extension transports.
	cancels *cancelRegistry

	// emulationStates tracks per-tab DevTools device emulation so clear can
	// restore UA/platform overrides that CDP itself has no clear command for.
	emulationMu     sync.Mutex
	emulationStates map[string]bridgeDeviceEmulationState

	// defaultGroup, when non-empty, is the tab-group title brw_open uses when the
	// caller did not specify a group, so the agent's tabs are corralled into one
	// labelled group in the user's window instead of scattered loose — the tidy,
	// "act like a person" default. The daemon sets it (see cmd/brwd); the zero
	// value keeps Open ungrouped for embedders/tests. Set once before serving.
	defaultGroup string
	// raiseWindowOnFocus controls whether focus_tab raises the Chrome WINDOW to
	// the OS foreground (chrome.windows.update{focused:true}). The library default
	// is true for back-compat, but the daemon defaults it to FALSE so automation
	// never steals the user's OS focus while they work in another app/window. Tab
	// activation within the window still happens regardless, so no-tab_id tools
	// resolve the right tab in the common single-window case without a focus grab.
	// Set once before serving.
	raiseWindowOnFocus bool
	// followFocus controls how a no-tab_id action resolves its target tab.
	//
	// false ("isolation", the daemon default): brw acts only on the tab it OWNS
	// — the one it opened, or one named explicitly via tab_id — and never on the
	// user's genuinely-focused Chrome tab. When brw owns no tab yet, the first
	// page-acting tool opens a fresh tab (in the default group, in the
	// background) instead of hijacking whatever the user is looking at. This is
	// what stops a worker from stomping the user's existing tabs.
	//
	// true (the library default, and restorable on the daemon with
	// --bridge-follow-focus / BRW_BRIDGE_FOLLOW_FOCUS): the legacy behavior — a
	// no-tab_id action follows the user's live-focused tab. Suits an interactive
	// single-operator session that wants brw to act on whatever tab is selected.
	//
	// Set once before serving.
	followFocus bool
	// autoOpenFailedAt records when an isolation auto-open last failed (guarded by
	// b.mu). It powers a cooldown so a wedged browser — one whose extension stops
	// answering open_tab — yields ONE bounded failure instead of every no-tab_id
	// action re-triggering a full-timeout open. Without it a single stuck open
	// cascades into "brw_evaluate 20003ms x N", one 20s hang per call.
	autoOpenFailedAt time.Time
}

// isolationSeedURL is the placeholder brw opens to claim its own working tab when
// a worker's first page action arrives before any explicit brw_open (isolation
// mode). The tool that triggered the open then navigates/reads this tab.
const isolationSeedURL = "about:blank"

const (
	// autoOpenTimeout bounds a single isolation auto-open so an unresponsive
	// extension fails in seconds instead of holding the caller for the full bridge
	// timeout (the 20s that surfaced as the brw_evaluate latency spike).
	autoOpenTimeout = 8 * time.Second
	// autoOpenCooldown suppresses re-opening for a window after a failure, so a
	// burst of no-tab_id calls against a wedged browser fast-fails instead of each
	// paying autoOpenTimeout — this is what breaks the per-call hang cascade.
	autoOpenCooldown = 15 * time.Second
)

type bridgeDeviceIdentity struct {
	UserAgent string `json:"userAgent"`
	Platform  string `json:"platform"`
}

type bridgeDeviceEmulationState struct {
	Baseline    bridgeDeviceIdentity
	HasBaseline bool
	Config      browser.DeviceEmulationConfig
}

type hello struct {
	Source    string `json:"source,omitempty"`
	Version   string `json:"version,omitempty"`
	Chrome    string `json:"chrome,omitempty"`
	Platform  string `json:"platform,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Profile   string `json:"profile,omitempty"`
	Label     string `json:"label,omitempty"`
	// Token is the per-launch handshake secret. It is read off the hello for
	// verification and then ZEROED before the hello is stored or echoed in
	// /status, so the secret is never reflected back over an endpoint a web page
	// could observe.
	Token string `json:"token,omitempty"`
}

type request struct {
	ID     string         `json:"id"`
	Type   string         `json:"type"`
	Params map[string]any `json:"params,omitempty"`
}

type response struct {
	ID     string          `json:"id,omitempty"`
	Type   string          `json:"type,omitempty"`
	TabID  int             `json:"tabId,omitempty"`
	OK     bool            `json:"ok,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Hello  hello           `json:"hello,omitempty"`
}

func New(addr string, timeout time.Duration, allowedExtensionID string) *Bridge {
	return NewWithIdentity(addr, timeout, allowedExtensionID, brwidentity.Identity{})
}

func NewWithIdentity(addr string, timeout time.Duration, allowedExtensionID string, identity brwidentity.Identity) *Bridge {
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	b := &Bridge{
		addr:               addr,
		timeout:            timeout,
		allowedExtensionID: strings.TrimSpace(allowedExtensionID),
		identity:           identity,
		pending:            map[string]chan response{},
		cancels:            newCancelRegistry(),
		emulationStates:    map[string]bridgeDeviceEmulationState{},
		connReady:          make(chan struct{}),
		tabLocks:           map[string]chan struct{}{},
		maxInflight:        defaultBridgeMaxInflight,
		sema:               make(chan struct{}, defaultBridgeMaxInflight),
		// Library default preserves historical behaviour (focus raises the
		// window); the daemon flips this to false for the seamless experience.
		raiseWindowOnFocus: true,
		// Library default preserves historical behaviour (no-tab_id actions follow
		// the user's focused tab); the daemon flips this to false (isolation) so a
		// worker works in its own tabs and never stomps the user's existing ones.
		followFocus: true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/extension", b.handleExtension)
	mux.HandleFunc("/status", b.handleStatus)
	// Bound the websocket-upgrade handshake against slow-header clients; the
	// connection is hijacked into a long-lived WS afterward, so no read/write
	// timeout that would sever the live bridge.
	b.server = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return b
}

// handshakeTimeout bounds how long the bridge waits for the extension's
// authenticated hello after the WS upgrade before giving up on a connection.
const handshakeTimeout = 5 * time.Second

// SetAuthToken installs the per-launch handshake secret the extension may
// present in its hello. Call once before ListenAndServe. An empty token leaves
// the handshake check disabled (the empty-Origin rejection still applies).
func (b *Bridge) SetAuthToken(token string) { b.authToken = strings.TrimSpace(token) }

// SetRequireToken switches the bridge to strict mode, where a hello with no
// token is rejected rather than accepted for backward-compatibility. Call once
// before ListenAndServe. Default (false) keeps a not-yet-reloaded extension
// working.
func (b *Bridge) SetRequireToken(v bool) { b.requireToken = v }

// NewAuthToken returns a fresh 256-bit URL-safe random token suitable for
// SetAuthToken. The daemon generates one per launch.
func NewAuthToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// defaultBridgeMaxInflight caps concurrent RPCs on the shared extension socket
// by default. The extension processes commands on a single MV3 service-worker
// thread, so beyond a handful of simultaneous heavy operations (a big snapshot,
// an upload) responses stop returning within the op deadline and every in-flight
// call times out together. Six keeps a healthy pipeline full without flooding
// the worker; tune via SetMaxInflight / --bridge-max-inflight.
const defaultBridgeMaxInflight = 6

// bridgeReconnectGrace bounds how long a call parks waiting for the MV3 service
// worker to reconnect after finding the socket down, before failing. The worker
// reconnects in ~1s; a few seconds of grace rides out the gap without hanging a
// caller indefinitely (the overall op deadline still applies on top).
const bridgeReconnectGrace = 3 * time.Second

// disconnectDrainReason is the error releaseConn stamps on pending RPCs when the
// socket drops. It is recognised as a transient transport failure so an
// idempotent read can be retried after the worker reconnects.
const disconnectDrainReason = "extension disconnected"

// ErrBridgeBusy signals that the bridge's in-flight cap is saturated and a call
// could not get a slot before its deadline. It is backpressure, not a fault: a
// caller should retry with backoff or reduce its concurrency rather than treat
// it as a hard failure.
var ErrBridgeBusy = errors.New("extension bridge busy: too many concurrent operations in flight, retry with backoff")

// errBridgeNotConnected is the no-live-socket condition. Transient: the MV3
// worker reconnects shortly, so an idempotent op may be retried.
var errBridgeNotConnected = errors.New("extension bridge is not connected; load/click the Chrome extension first")

// errBridgeTransport wraps a transient transport failure (write failed mid-frame,
// socket dropped while a call was pending). Safe to retry for idempotent ops.
var errBridgeTransport = errors.New("extension bridge transport error")

// SetMaxInflight sets the cap on concurrent RPCs over the shared socket. n<=0
// disables the cap (unbounded). Call once before serving; it rebuilds the
// semaphore and is not safe to race with live calls.
func (b *Bridge) SetMaxInflight(n int) {
	if n <= 0 {
		b.maxInflight = 0
		b.sema = nil
		return
	}
	b.maxInflight = n
	b.sema = make(chan struct{}, n)
}

// idempotentBridgeTypes are read-only bridge RPCs that are safe to re-issue after
// a transient transport drop. Mutating ops (open_tab, type, navigate, upload,
// group/ungroup, and the generic "cdp" passthrough, which carries both reads and
// writes) are deliberately excluded so a retry can never double-apply an action.
var idempotentBridgeTypes = map[string]bool{
	"list_tabs":         true,
	"list_tab_groups":   true,
	"get_active_tab_id": true,
	"cached_snapshot":   true,
	"get_downloads":     true,
}

func isIdempotentType(typ string) bool { return idempotentBridgeTypes[typ] }

func isTransientTransportErr(err error) bool {
	return errors.Is(err, errBridgeTransport) || errors.Is(err, errBridgeNotConnected)
}

// isUnknownMessageTypeErr reports whether err is the extension's "unknown message
// type" rejection, surfaced when the connected extension predates a bridge RPC
// (e.g. an old build that lacks get_downloads). Callers use it to degrade
// gracefully to an unsupported result rather than erroring.
func isUnknownMessageTypeErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown message type")
}

func (b *Bridge) ListenAndServe() error {
	return b.server.ListenAndServe()
}

func (b *Bridge) Shutdown(ctx context.Context) error {
	b.mu.Lock()
	if b.conn != nil {
		_ = b.conn.Close(websocket.StatusNormalClosure, "shutdown")
		b.conn = nil
	}
	b.mu.Unlock()
	return b.server.Shutdown(ctx)
}

func (b *Bridge) handleStatus(w http.ResponseWriter, r *http.Request) {
	b.mu.RLock()
	connected := b.conn != nil
	hello := b.hello
	active := b.active
	connectedAt := b.connectedAt
	lastSeenAt := b.lastSeenAt
	disconnectedAt := b.disconnectedAt
	disconnectReason := b.disconnectReason
	pending := len(b.pending)
	identity := b.identity
	token := b.authToken
	b.mu.RUnlock()
	status := map[string]any{
		"connected":         connected,
		"hello":             hello,
		"active_tab_id":     active,
		"connected_at":      formatStatusTime(connectedAt),
		"last_seen_at":      formatStatusTime(lastSeenAt),
		"disconnected_at":   formatStatusTime(disconnectedAt),
		"disconnect_reason": disconnectReason,
		"pending":           pending,
		// Backpressure / contention signal so operators can see saturation
		// (inflight near max_inflight, queued > 0, busy_drops climbing) before it
		// shows up as timeouts, and tune --bridge-max-inflight accordingly.
		"max_inflight": b.maxInflight,
		"inflight":     b.inflight.Load(),
		"queued":       b.queued.Load(),
		"busy_drops":   b.busyDrops.Load(),
		"retries":      b.retries.Load(),
	}
	if !identity.Empty() {
		status["identity"] = identity
	}
	// Hand the handshake token to the extension over loopback only, and never
	// to a browser web origin. The extension's same-origin GET carries no Origin
	// header and reads the body (it has host_permissions); a malicious page's
	// cross-origin fetch gets an opaque response AND is omitted here, and a
	// DNS-rebinding page (no Origin, attacker Host) is excluded by the loopback
	// Host check.
	if token != "" && tokenServable(r) {
		status["token"] = token
	}
	writeJSON(w, http.StatusOK, status)
}

// tokenServable reports whether the handshake token may be included in a /status
// response: only over a loopback Host and never to an http(s) browser Origin.
func tokenServable(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" && !strings.HasPrefix(o, "chrome-extension://") {
		return false
	}
	return isLoopbackHostname(r.Host)
}

// isLoopbackHostname reports whether the Host header (with optional port) refers
// to a loopback name/IP.
func isLoopbackHostname(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSpace(strings.Trim(host, "[]")))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// extensionOriginOK reports whether origin is a usable chrome-extension:// origin.
// An empty Origin (a non-browser local client such as curl or a rogue script) or
// any non-extension scheme is rejected here, closing the coder/websocket gap
// where an absent Origin is treated as same-origin and allowed.
func extensionOriginOK(origin string) bool {
	const prefix = "chrome-extension://"
	return strings.HasPrefix(origin, prefix) && len(origin) > len(prefix)
}

// effectiveExtensionID returns the extension id whose chrome-extension:// origin
// the bridge will accept. An explicit profile bridge_extension_id always wins;
// otherwise we fall back to the published default id (profilepolicy.
// DefaultBridgeExtensionID) rather than the chrome-extension://* wildcard, so an
// unconfigured bridge still pins to the real extension instead of accepting ANY
// installed extension. Returns "" only when neither is set, which is the sole
// case that falls back to the wildcard (with a loud warning).
func (b *Bridge) effectiveExtensionID() string {
	if b.allowedExtensionID != "" {
		return b.allowedExtensionID
	}
	return strings.TrimSpace(profilepolicy.DefaultBridgeExtensionID)
}

func (b *Bridge) handleExtension(w http.ResponseWriter, r *http.Request) {
	// Reject any connection that does not present a chrome-extension Origin. This
	// closes the coder/websocket gap where an ABSENT Origin (a non-browser local
	// client — curl, a rogue script) is treated as same-origin and accepted; only
	// a real extension carries a chrome-extension:// Origin, and a browser web
	// page cannot forge one.
	if !extensionOriginOK(r.Header.Get("Origin")) {
		http.Error(w, "forbidden: a chrome-extension origin is required", http.StatusForbidden)
		return
	}
	allowedID := b.effectiveExtensionID()
	originPatterns := []string{"chrome-extension://*"}
	if allowedID != "" {
		originPatterns = []string{"chrome-extension://" + allowedID}
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: originPatterns,
	})
	if err != nil {
		log.Printf("extension websocket accept: %v", err)
		return
	}
	conn.SetReadLimit(4 << 20)

	if allowedID == "" {
		log.Printf("WARNING: extension bridge accepting connections from any Chrome extension (chrome-extension://*); set a profile policy with bridge_extension_id to restrict")
	}

	// When a per-launch token is configured, authenticate the hello BEFORE this
	// connection becomes the live bridge — so an unverified client can neither
	// displace the real extension nor receive a single command. With no token
	// (library/embedder/test) the connection goes live immediately, as before.
	verifiedHello := hello{}
	if b.authToken != "" {
		h, herr := b.verifyHandshake(r.Context(), conn)
		if herr != nil {
			log.Printf("extension bridge handshake rejected: %v", herr)
			_ = conn.Close(websocket.StatusPolicyViolation, "handshake failed")
			return
		}
		verifiedHello = h
	}

	b.mu.Lock()
	if b.conn != nil {
		_ = b.conn.Close(websocket.StatusNormalClosure, "replaced by new extension connection")
	}
	now := time.Now().UTC()
	b.conn = conn
	b.hello = verifiedHello
	b.connectedAt = now
	b.lastSeenAt = now
	b.disconnectedAt = time.Time{}
	b.disconnectReason = ""
	// Wake any calls parked in getConn waiting for the socket to come back, and
	// arm a fresh gate for the next disconnect→reconnect cycle. Always
	// close-then-replace under mu so the channel is closed exactly once.
	close(b.connReady)
	b.connReady = make(chan struct{})
	b.mu.Unlock()

	log.Printf("extension bridge connected")

	// Keepalive: ping the extension periodically so a half-open link (laptop
	// sleep, NAT timeout, dropped Wi-Fi) is detected promptly instead of hanging
	// until a request times out. A failed ping closes the conn, which unblocks
	// readLoop's conn.Read and drains b.pending. The pinger exits cleanly when the
	// read loop returns (pingCancel) so it never leaks.
	pingCtx, pingCancel := context.WithCancel(r.Context())
	go b.keepAlive(pingCtx, conn, pingKeepaliveInterval)

	readErr := b.readLoop(r.Context(), conn)
	pingCancel()
	reason := "closed"
	if readErr != nil {
		reason = readErr.Error()
	}
	log.Printf("extension bridge disconnected: %s", reason)

	b.releaseConn(conn, reason)
}

// releaseConn tears down a connection that has stopped reading. It only acts
// when conn is still the bridge's ACTIVE connection: an MV3 service worker
// reconnects constantly, and handleExtension deliberately replaces an old conn
// with a new one (b.conn = newConn). When the displaced (stale) conn's readLoop
// finally returns it must NOT drain pending RPCs that now belong to the live
// connection, nor stamp the bridge "disconnected" while b.conn points at a
// healthy socket — doing so spuriously fails in-flight calls and reports a
// disconnect reason alongside connected:true.
func (b *Bridge) releaseConn(conn *websocket.Conn, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != conn {
		return
	}
	b.conn = nil
	b.disconnectedAt = time.Now().UTC()
	b.disconnectReason = reason
	for id, ch := range b.pending {
		delete(b.pending, id)
		ch <- response{ID: id, Error: disconnectDrainReason}
		close(ch)
	}
}

// pingKeepaliveInterval is how often the bridge pings the connected extension to
// detect a half-open link. Each ping is bounded by its own short deadline so a
// dead link is surfaced well within the interval.
const (
	pingKeepaliveInterval = 30 * time.Second
	pingTimeout           = 10 * time.Second
)

// keepAlive pings the extension every interval. A ping that fails (or times out)
// means the link is dead/half-open: the conn is closed, which unblocks readLoop
// and drains b.pending. The goroutine exits when ctx is cancelled (the read loop
// returned) — no leak. It pings only while this conn is still the bridge's
// active conn, so a replaced connection's pinger goes quiet on its next tick.
// interval is a parameter (not the const directly) so tests can drive it fast.
func (b *Bridge) keepAlive(ctx context.Context, conn *websocket.Conn, interval time.Duration) {
	if interval <= 0 {
		interval = pingKeepaliveInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.mu.RLock()
			current := b.conn == conn
			b.mu.RUnlock()
			if !current {
				return
			}
			pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("extension bridge keepalive ping failed: %v", err)
				_ = conn.Close(websocket.StatusGoingAway, "keepalive ping failed")
				return
			}
		}
	}
}

// verifyHandshake reads the first frame of a freshly-accepted connection. It
// must be a hello (any other first frame is refused). A token that is PRESENT
// must match the configured one — a wrong token is always rejected. A MISSING
// token is accepted by default (graceful: a pre-0.2.0 extension that hasn't been
// reloaded still works, so upgrading the daemon never bricks it) unless
// requireToken is set, which makes the token mandatory. Bounded by
// handshakeTimeout so a silent client cannot hold the slot open. Only called
// when b.authToken != "".
func (b *Bridge) verifyHandshake(ctx context.Context, conn *websocket.Conn) (hello, error) {
	verifyCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	_, data, err := conn.Read(verifyCtx)
	if err != nil {
		return hello{}, fmt.Errorf("read hello: %w", err)
	}
	var resp response
	if err := json.Unmarshal(data, &resp); err != nil {
		return hello{}, fmt.Errorf("invalid hello frame: %w", err)
	}
	if resp.Type != "hello" {
		return hello{}, fmt.Errorf("expected hello as first frame, got %q", resp.Type)
	}
	switch {
	case resp.Hello.Token == "":
		// No token: a pre-0.2.0 extension (or one that could not read /status).
		// Accept for compatibility unless strict mode requires it. The empty-Origin
		// rejection already blocks browser web pages regardless of the token.
		if b.requireToken {
			return hello{}, errors.New("missing handshake token (BRW_BRIDGE_REQUIRE_TOKEN is set)")
		}
		b.compatWarnOnce.Do(func() {
			log.Printf("NOTE: extension connected without a handshake token (pre-0.2.0 extension) — accepting for compatibility. Reload the brw extension to enable bridge authentication; set BRW_BRIDGE_REQUIRE_TOKEN=1 to require it.")
		})
	case subtle.ConstantTimeCompare([]byte(resp.Hello.Token), []byte(b.authToken)) != 1:
		// A token was presented but does not match — tampering or a stale token.
		return hello{}, errors.New("invalid handshake token")
	}
	h := resp.Hello
	h.Token = "" // never store or echo the secret
	return h, nil
}

func (b *Bridge) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			log.Printf("extension bridge read: %v", err)
			return err
		}
		var resp response
		if err := json.Unmarshal(data, &resp); err != nil {
			log.Printf("extension bridge invalid message: %v", err)
			continue
		}
		b.mu.Lock()
		if b.conn == conn {
			b.lastSeenAt = time.Now().UTC()
		}
		b.mu.Unlock()
		if resp.Type == "hello" {
			h := resp.Hello
			h.Token = "" // never store or echo the handshake secret
			b.mu.Lock()
			b.hello = h
			b.mu.Unlock()
			continue
		}
		if resp.Type == "active_tab" {
			// active_tab is the USER's live-focus hint, pushed when they switch
			// tabs. Honor it only in follow-focus mode; in isolation the cached
			// active id must track the tab brw OWNS, so a user tab-switch must not
			// repoint it onto the user's tab (that is the stomping we prevent).
			if resp.TabID != 0 && b.followFocus {
				b.setActiveTabID(strconv.Itoa(resp.TabID))
			}
			continue
		}
		if resp.ID == "" {
			continue
		}
		b.mu.Lock()
		ch := b.pending[resp.ID]
		delete(b.pending, resp.ID)
		b.mu.Unlock()
		if ch != nil {
			ch <- resp
			close(ch)
		}
	}
}

func formatStatusTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

// call is the single chokepoint every bridge RPC funnels through. It layers
// three protections over the shared single-socket transport so many concurrent
// agents cannot wedge it:
//
//  1. per-tab serialization — ops on the same tab never interleave their frames;
//  2. backpressure — a bounded number of RPCs are on the wire at once, excess
//     queues, and a call that cannot get a slot before the deadline fails fast
//     with ErrBridgeBusy instead of hanging;
//  3. reconnect resilience — a call arriving during the MV3 reconnect gap waits
//     for the socket to return, and idempotent reads retry once on a transient
//     transport drop.
//
// Acquisition order is per-tab lock THEN in-flight slot: a same-tab call queues
// on the (cheap) tab lock before it consumes a scarce slot, so a burst on one
// tab cannot starve other tabs of slots, and the consistent ordering keeps the
// two gates deadlock-free.
func (b *Bridge) call(ctx context.Context, typ string, params map[string]any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	unlockTab, err := b.lockTab(timeoutCtx, params)
	if err != nil {
		return nil, err
	}
	if unlockTab != nil {
		defer unlockTab()
	}

	releaseSlot, err := b.acquireSlot(timeoutCtx)
	if err != nil {
		return nil, err
	}
	defer releaseSlot()

	raw, err := b.dispatch(timeoutCtx, typ, params)
	if err == nil {
		return raw, nil
	}
	// Retry once for an idempotent read whose only failure was a transient
	// transport drop: the socket fell over mid-flight but the MV3 worker
	// reconnects in ~1s, and re-issuing a side-effect-free read is safe. dispatch
	// itself waits for the reconnect inside getConn. Mutating ops never retry.
	if isIdempotentType(typ) && isTransientTransportErr(err) {
		b.retries.Add(1)
		if raw2, err2 := b.dispatch(timeoutCtx, typ, params); err2 == nil {
			return raw2, nil
		}
	}
	return raw, err
}

// dispatch performs one RPC round-trip: resolve a live connection (waiting out a
// reconnect gap), register the pending response, write the frame, and wait for
// the reply within ctx. A write failure or a disconnect-drained reply is wrapped
// as errBridgeTransport so call() can decide whether to retry.
func (b *Bridge) dispatch(ctx context.Context, typ string, params map[string]any) (json.RawMessage, error) {
	conn, err := b.getConn(ctx)
	if err != nil {
		return nil, err
	}

	id := strconv.FormatUint(b.nextID.Add(1), 10)
	ch := make(chan response, 1)
	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()

	msg, err := json.Marshal(request{ID: id, Type: typ, Params: params})
	if err != nil {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, err
	}
	// Write under writeMu with an INDEPENDENT short deadline (not ctx): request
	// cancellation must never tear down the shared socket mid-write. The response
	// is still bounded by ctx in the select below.
	writeCtx, writeCancel := context.WithTimeout(context.Background(), bridgeWriteTimeout)
	b.writeMu.Lock()
	err = conn.Write(writeCtx, websocket.MessageText, msg)
	b.writeMu.Unlock()
	writeCancel()
	if err != nil {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		// A failed write means the socket is broken (closed / broken pipe); mark
		// it transient so an idempotent caller can retry after the worker returns.
		return nil, fmt.Errorf("%w: %v", errBridgeTransport, err)
	}

	select {
	case resp := <-ch:
		if !resp.OK {
			if resp.Error == "" {
				resp.Error = "extension bridge request failed"
			}
			if resp.Error == disconnectDrainReason {
				// releaseConn drained this pending call because the socket dropped:
				// transient, retryable for idempotent ops.
				return nil, fmt.Errorf("%w: %s", errBridgeTransport, resp.Error)
			}
			return nil, fmt.Errorf("extension bridge: %s", resp.Error)
		}
		return resp.Result, nil
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, ctx.Err()
	}
}

// getConn returns the live socket, parking briefly for the MV3 service worker to
// reconnect if it is momentarily down rather than failing the call outright.
// MV3 workers sleep and respawn constantly, so a transient conn==nil is normal,
// not a real disconnect.
func (b *Bridge) getConn(ctx context.Context) (*websocket.Conn, error) {
	b.mu.RLock()
	conn := b.conn
	b.mu.RUnlock()
	if conn != nil {
		return conn, nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, bridgeReconnectGrace)
	defer cancel()
	for {
		b.mu.RLock()
		conn := b.conn
		ready := b.connReady
		b.mu.RUnlock()
		if conn != nil {
			return conn, nil
		}
		select {
		case <-ready:
			// A connection went live; re-read b.conn on the next loop iteration.
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errBridgeNotConnected
		}
	}
}

// acquireSlot takes one in-flight slot from the backpressure semaphore, blocking
// (ctx-aware) when the cap is saturated. If the deadline elapses before a slot
// frees, it returns ErrBridgeBusy so the caller backs off instead of piling onto
// a flooded socket. The returned release MUST be called. A nil sema disables the
// cap and returns a no-op release immediately.
func (b *Bridge) acquireSlot(ctx context.Context) (func(), error) {
	sema := b.sema
	if sema == nil {
		return func() {}, nil
	}
	release := func() { <-sema; b.inflight.Add(-1) }
	select {
	case sema <- struct{}{}:
		b.inflight.Add(1)
		return release, nil
	default:
	}
	// Saturated: queue for a slot, bounded by the op deadline.
	b.queued.Add(1)
	defer b.queued.Add(-1)
	select {
	case sema <- struct{}{}:
		b.inflight.Add(1)
		return release, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			b.busyDrops.Add(1)
			return nil, ErrBridgeBusy
		}
		return nil, ctx.Err()
	}
}

// ctxKeySkipTabLock marks a context whose RPC must NOT take the per-tab lock.
type ctxKeySkipTabLock struct{}

// withoutTabLock marks ctx so call() skips per-tab serialization for this RPC.
// It is for the abandonable settle fingerprint probe only: a read-only,
// fire-and-forget background read that the settle watchdog may give up on while
// it keeps running (cancelling it mid-write would drop the shared socket). If
// such an orphan held the tab lock it would block the next foreground op on that
// tab; exempting it keeps settle's "abandon and move on" semantics. The in-flight
// cap still bounds it.
func withoutTabLock(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeySkipTabLock{}, true)
}

func tabLockSkipped(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeySkipTabLock{}).(bool)
	return v
}

// lockTab acquires the per-tab serialization lock for the tab named in params
// (if any), so two RPCs targeting the same tab never overlap. Returns a nil
// unlock when the op is not tab-scoped (e.g. list_tabs, open_tab) or is marked
// skip via withoutTabLock. Ctx-aware: a call that cannot get the tab's turn
// before the deadline returns ErrBridgeBusy.
func (b *Bridge) lockTab(ctx context.Context, params map[string]any) (func(), error) {
	if tabLockSkipped(ctx) {
		return nil, nil
	}
	key := tabKeyFromParams(params)
	if key == "" {
		return nil, nil
	}
	lock := b.tabLock(key)
	select {
	case lock <- struct{}{}:
		return func() { <-lock }, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrBridgeBusy
		}
		return nil, ctx.Err()
	}
}

// tabLock returns the (lazily created) 1-buffered channel used as the lock for a
// tab id. A held lock == a token sitting in the channel.
func (b *Bridge) tabLock(key string) chan struct{} {
	b.tabLocksMu.Lock()
	defer b.tabLocksMu.Unlock()
	lock := b.tabLocks[key]
	if lock == nil {
		lock = make(chan struct{}, 1)
		b.tabLocks[key] = lock
	}
	return lock
}

// tabKeyFromParams extracts a stable string lock key from a call's tabId param,
// which is an int on the cdp/file-chooser paths and a string on focus/close.
// A zero/absent id means "not tab-scoped" (no lock).
func tabKeyFromParams(params map[string]any) string {
	if params == nil {
		return ""
	}
	v, ok := params["tabId"]
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case int:
		if t == 0 {
			return ""
		}
		return strconv.Itoa(t)
	case int64:
		if t == 0 {
			return ""
		}
		return strconv.FormatInt(t, 10)
	case float64:
		if t == 0 {
			return ""
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

// SetDefaultGroup configures the tab-group title brw_open lands in when the
// caller passes no group of its own (empty disables default grouping). Call
// before serving.
func (b *Bridge) SetDefaultGroup(name string) { b.defaultGroup = strings.TrimSpace(name) }

// SetRaiseWindowOnFocus configures whether focus_tab raises the Chrome window to
// the OS foreground. The daemon sets this to false so automation never steals
// the user's focus. Call before serving.
func (b *Bridge) SetRaiseWindowOnFocus(v bool) { b.raiseWindowOnFocus = v }

// SetFollowFocus selects how a no-tab_id action resolves its target tab. Pass
// false (the daemon default) for isolation — brw works on tabs it owns and opens
// a fresh background tab rather than touching the user's focused tab. Pass true
// for the legacy behavior where brw follows the user's manually-focused tab. Call
// before serving. See the followFocus field for the full contract.
func (b *Bridge) SetFollowFocus(v bool) { b.followFocus = v }

// openTabParams stamps the foreground/background intent onto an open_tab request.
// In isolation (followFocus=false) brw opens its tab in the BACKGROUND
// (active:false) so it never switches the tab the user is looking at; the
// extension still pins the new tab as brw's working target and the daemon
// resolves it by id. In follow-focus mode the tab opens active so the user's view
// follows the agent — the legacy behavior. An extension that predates the active
// flag treats a missing/true value as active, so this is backward compatible.
func (b *Bridge) openTabParams(params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	params["active"] = b.followFocus
	return params
}

func (b *Bridge) Open(ctx context.Context, url string) (browser.OpenResult, error) {
	// Corral the agent's tabs into one labelled group by default so they don't
	// scatter loose across the user's window. An explicit group from the caller
	// (OpenInGroup) always wins; this only fills the no-group case.
	if group := b.defaultGroup; group != "" {
		return b.OpenInGroup(ctx, url, browser.TabGroupOptions{Name: group})
	}
	if strings.TrimSpace(url) == "" {
		url = "about:blank"
	}
	if !strings.Contains(url, "://") && url != "about:blank" {
		url = "https://" + url
	}
	raw, err := b.call(ctx, "open_tab", b.openTabParams(map[string]any{"url": url}))
	if err != nil {
		return browser.OpenResult{}, err
	}
	var tab extTab
	if err := json.Unmarshal(raw, &tab); err != nil {
		return browser.OpenResult{}, err
	}
	out := tab.toBrowserTab()
	if out.ID == "" {
		return browser.OpenResult{}, errors.New("open_tab returned no tab id")
	}
	b.setActiveTabID(out.ID)
	ready := b.waitOpenReady(ctx, url, out.ID)
	// In isolation we resolve no-tab_id actions by the owned id (b.active), so the
	// opened tab need not be foregrounded — keeping it in the background means the
	// open never disturbs the tab the user is on. Only follow-focus mode, where
	// later actions chase the live foreground tab, must make the new tab current.
	if b.followFocus {
		if err := b.ensureForegroundTab(ctx, out.ID); err != nil {
			return browser.OpenResult{Tab: out, Ready: ready}, err
		}
	}
	return browser.OpenResult{Tab: out, Ready: ready}, nil
}

// waitOpenReady blocks until the freshly opened tab is usable, matching the
// direct-CDP Open() contract so an immediate brw_evaluate / brw_read on
// the new tab doesn't race the transient about:blank Chrome reports before the
// real navigation commits. Returns whether readiness was confirmed; a wait
// timeout is not fatal (the tab still exists), it just reports ready=false. The
// wait targets the specific new tab id (not the resolved active tab) because
// open_tab creates the tab inactive, so the focused tab is still the old one.
func (b *Bridge) waitOpenReady(ctx context.Context, url, tabID string) bool {
	if tabID == "" {
		return false
	}
	waitCtx := browser.WithTabID(ctx, tabID)
	if url == "about:blank" {
		return b.WaitFor(waitCtx, "ready", 5*time.Second) == nil
	}
	return b.WaitFor(waitCtx, "committed", 10*time.Second) == nil
}

func (b *Bridge) ListTabs(ctx context.Context) ([]browser.Tab, error) {
	raw, err := b.call(ctx, "list_tabs", nil)
	if err != nil {
		return nil, err
	}
	var tabs []extTab
	if err := json.Unmarshal(raw, &tabs); err != nil {
		return nil, err
	}
	out := make([]browser.Tab, 0, len(tabs))
	activeID := ""
	fallbackActiveID := ""
	hasFocusedWindow := false
	for _, tab := range tabs {
		outTab := tab.toBrowserTab()
		out = append(out, outTab)
		if outTab.WindowFocused {
			hasFocusedWindow = true
		}
		if outTab.Active && outTab.WindowFocused {
			activeID = outTab.ID
		}
		if fallbackActiveID == "" && outTab.Active {
			fallbackActiveID = outTab.ID
		}
	}
	if activeID == "" && !hasFocusedWindow && b.activeTabID() == "" {
		activeID = fallbackActiveID
	}
	// Only let a list refresh repoint the cached active id in follow-focus mode.
	// In isolation that cache is brw's OWNED tab; the user's focused tab (what
	// activeID reflects here) must not overwrite it, or the next no-tab_id action
	// would land on the user's tab.
	if activeID != "" && b.followFocus {
		b.setActiveTabID(activeID)
	}
	return out, nil
}

func (b *Bridge) ListTabGroups(ctx context.Context) ([]browser.TabGroup, error) {
	raw, err := b.call(ctx, "list_tab_groups", nil)
	if err != nil {
		return nil, err
	}
	var groups []extTabGroup
	if err := json.Unmarshal(raw, &groups); err != nil {
		return nil, err
	}
	out := make([]browser.TabGroup, 0, len(groups))
	for _, group := range groups {
		out = append(out, group.toBrowserTabGroup())
	}
	return out, nil
}

func (b *Bridge) FocusTab(ctx context.Context, id string) error {
	tabID, err := requireTabID(id)
	if err != nil {
		return err
	}
	raw, err := b.call(ctx, "focus_tab", map[string]any{"tabId": tabID, "raiseWindow": b.raiseWindowOnFocus})
	if err != nil {
		return err
	}
	var tab extTab
	if err := json.Unmarshal(raw, &tab); err == nil && tab.ID != 0 {
		b.setActiveTabID(strconv.Itoa(tab.ID))
		return nil
	}
	// Unmarshal failed or returned zero ID; fall through to use the original
	// id. The focus_tab call succeeded, so the focus did happen — we just
	// cannot confirm the tab metadata.
	if strings.TrimSpace(id) != "" {
		b.setActiveTabID(id)
	}
	return nil
}

func (b *Bridge) CloseTab(ctx context.Context, id string) error {
	tabID, err := requireTabID(id)
	if err != nil {
		return err
	}
	_, err = b.call(ctx, "close_tab", map[string]any{"tabId": tabID})
	if err == nil && strings.TrimSpace(id) == b.activeTabID() {
		b.setActiveTabID("")
	}
	return err
}

func (b *Bridge) GroupTabs(ctx context.Context, tabIDs []string, opts browser.TabGroupOptions) error {
	ids := make([]int, 0, len(tabIDs))
	for _, id := range tabIDs {
		ids = append(ids, parseTabID(id))
	}
	params := map[string]any{
		"tabIds": ids,
		"name":   opts.Name,
		"color":  opts.Color,
	}
	if opts.GroupID != "" {
		groupID, err := requireGroupID(opts.GroupID)
		if err != nil {
			return err
		}
		params["groupId"] = groupID
	}
	_, err := b.call(ctx, "group_tabs", params)
	return err
}

func (b *Bridge) UngroupTabs(ctx context.Context, tabIDs []string) error {
	ids := make([]int, 0, len(tabIDs))
	for _, id := range tabIDs {
		ids = append(ids, parseTabID(id))
	}
	_, err := b.call(ctx, "ungroup_tabs", map[string]any{"tabIds": ids})
	return err
}

func (b *Bridge) OpenInGroup(ctx context.Context, url string, opts browser.TabGroupOptions) (browser.OpenResult, error) {
	if strings.TrimSpace(url) == "" {
		url = "about:blank"
	}
	if !strings.Contains(url, "://") && url != "about:blank" {
		url = "https://" + url
	}
	params := map[string]any{"url": url}
	if opts.GroupID != "" {
		groupID, err := requireGroupID(opts.GroupID)
		if err != nil {
			return browser.OpenResult{}, err
		}
		params["groupId"] = groupID
	}
	if opts.Name != "" {
		params["groupName"] = opts.Name
	}
	if opts.Color != "" {
		params["groupColor"] = opts.Color
	}
	raw, err := b.call(ctx, "open_tab", b.openTabParams(params))
	if err != nil {
		return browser.OpenResult{}, err
	}
	var tab extTab
	if err := json.Unmarshal(raw, &tab); err != nil {
		return browser.OpenResult{}, err
	}
	out := tab.toBrowserTab()
	if out.ID == "" {
		return browser.OpenResult{}, errors.New("open_tab returned no tab id")
	}
	b.setActiveTabID(out.ID)
	ready := b.waitOpenReady(ctx, url, out.ID)
	// See Open: in isolation the owned id drives resolution, so we leave the tab in
	// the background and never steal the user's current tab; only follow-focus mode
	// foregrounds it.
	if b.followFocus {
		if err := b.ensureForegroundTab(ctx, out.ID); err != nil {
			return browser.OpenResult{Tab: out, Ready: ready}, err
		}
	}
	return browser.OpenResult{Tab: out, Ready: ready}, nil
}

func (b *Bridge) Snapshot(ctx context.Context, opts snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	var snap snapshot.PageSnapshot
	opts.IncludeAX = false
	// A since-delta request must reach the in-page walker (which derives the delta
	// from live per-document state) and must bypass the outer snapshot cache in
	// BOTH directions: a cached full snapshot would defeat the delta, and caching a
	// delta-shaped (partial) result under the key would corrupt later full reads.
	// include_frames also bypasses the cache so the dynamic cross-origin-frame read
	// is never served stale.
	sinceDelta := opts.Since > 0
	bypassCache := sinceDelta || opts.IncludeFrames
	if !bypassCache {
		if cached, ok := b.tryCachedSnapshot(ctx, opts); ok {
			return cached, nil
		}
	}
	optsJSON, _ := json.Marshal(opts)
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.SnapshotFunctionScript, optsJSON), "", &snap); err != nil {
		return snap, err
	}
	snap.Accessibility = snapshot.AccessibilitySummary{
		Available: false,
		Error:     "accessibility tree is unavailable through the Chrome extension bridge; use direct CDP attach for AX enrichment",
	}
	if opts.IncludeFrames {
		// Best-effort: read interactive controls INSIDE cross-origin iframes and
		// merge them with frame-qualified refs (f<i>:e<j>) + top-level click coords.
		// This succeeds only when the frame's debugger sub-target is reachable; for
		// out-of-process iframes over the extension bridge it usually is not (see
		// readCrossOriginFrames / PromoteCrossOriginFrames).
		read := 0
		if frames, err := b.readCrossOriginFrames(ctx); err == nil && len(frames) > 0 {
			read = snapshot.MergeCrossOriginFrames(&snap, frames)
		}
		if read == 0 {
			// Inner-DOM read unavailable — surface each cross-origin frame as a
			// CLICKABLE element (ref f<i>, cx/cy at its center) so the agent can act
			// on it via brw_click_xy instead of being blind to it.
			snapshot.PromoteCrossOriginFrames(&snap, nil)
		}
	}
	if !bypassCache {
		b.storeCachedSnapshot(ctx, opts, snap)
	}
	return snap, nil
}

// readCrossOriginFrames asks the extension to read the interactive controls of
// each cross-origin (out-of-process) iframe in the active tab. The extension
// matches the tab's frame tree against the debugger target list, briefly attaches
// to each frame target to extract its controls, and detaches. Returns an empty
// slice (no error) when the extension predates the command so callers degrade to
// the same-origin-only snapshot.
func (b *Bridge) readCrossOriginFrames(ctx context.Context) ([]snapshot.CrossOriginFrame, error) {
	tabID := b.contextTabID(ctx)
	raw, err := b.call(ctx, "read_cross_origin_frames", map[string]any{"tabId": parseTabID(tabID)})
	if err != nil {
		if isUnknownMessageTypeErr(err) {
			return nil, nil
		}
		return nil, err
	}
	var payload struct {
		Frames []snapshot.CrossOriginFrame `json:"frames"`
	}
	if len(raw) > 0 {
		if jsonErr := json.Unmarshal(raw, &payload); jsonErr != nil {
			return nil, fmt.Errorf("parse cross-origin frames: %w", jsonErr)
		}
	}
	return payload.Frames, nil
}

func (b *Bridge) tryCachedSnapshot(ctx context.Context, opts snapshot.SnapshotOptions) (snapshot.PageSnapshot, bool) {
	if opts.Mode == "all" || opts.IncludeHidden {
		return snapshot.PageSnapshot{}, false
	}
	tabID := b.contextTabID(ctx)
	raw, err := b.call(ctx, "cached_snapshot", map[string]any{
		"tabId":    parseTabID(tabID),
		"cacheKey": snapshotCacheKey(opts),
	})
	if err != nil {
		return snapshot.PageSnapshot{}, false
	}
	var resp struct {
		Cached   bool                  `json:"cached"`
		Snapshot snapshot.PageSnapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || !resp.Cached {
		return snapshot.PageSnapshot{}, false
	}
	return resp.Snapshot, true
}

func (b *Bridge) storeCachedSnapshot(ctx context.Context, opts snapshot.SnapshotOptions, snap snapshot.PageSnapshot) {
	tabID := b.contextTabID(ctx)
	if tabID == "" {
		return
	}
	_, _ = b.call(ctx, "snapshot_result", map[string]any{
		"tabId":    parseTabID(tabID),
		"cacheKey": snapshotCacheKey(opts),
		"snapshot": snap,
	})
}

func snapshotCacheKey(opts snapshot.SnapshotOptions) string {
	opts.IncludeAX = false
	data, _ := json.Marshal(opts)
	return string(data)
}

func (b *Bridge) Find(ctx context.Context, opts snapshot.FindOptions) (snapshot.FindResult, error) {
	snap, err := b.Snapshot(ctx, snapshot.SnapshotOptions{
		Query:         opts.Query,
		Text:          opts.Text,
		Role:          opts.Role,
		Limit:         opts.Limit,
		ViewportOnly:  opts.ViewportOnly,
		IncludeHidden: opts.IncludeHidden,
	})
	if err != nil {
		return snapshot.FindResult{}, err
	}
	return snapshot.FindResult{
		URL:      snap.URL,
		Title:    snap.Title,
		Elements: snap.Elements,
		Metadata: snap.Metadata,
	}, nil
}

func (b *Bridge) Read(ctx context.Context) (readability.PageRead, error) {
	var read readability.PageRead
	err := b.evaluate(ctx, readability.ReadExpr(), "", &read)
	if err != nil {
		return readability.PageRead{}, err
	}
	return readability.Normalize(read), nil
}

func (b *Bridge) ReadData(ctx context.Context) (snapshot.StructuredData, error) {
	var data snapshot.StructuredData
	err := b.evaluate(ctx, snapshot.StructuredDataScript, "", &data)
	return data, err
}

const (
	// observedActionSettle / batchActionSettle are the CAPS for the adaptive
	// settle (b.settle): the page-change poll never blocks longer than this, so
	// settling is never slower than the previous blind time.Sleep, only faster
	// when the page stabilises early.
	observedActionSettle = 75 * time.Millisecond
	batchActionSettle    = 25 * time.Millisecond
	waitForPollInterval  = 250 * time.Millisecond
	// settlePollStart / settlePollMax bound the adaptive settle poll cadence:
	// start tight so a quiescent page returns in ~one short interval, then back
	// off so a busy page does not spin. settleStableReads is how many consecutive
	// equal fingerprints count as "settled".
	settlePollStart   = 12 * time.Millisecond
	settlePollMax     = 40 * time.Millisecond
	settleStableReads = 2
	// settleMinFloor is the minimum settle duration: we never return "settled"
	// before it even when the page already looks stable, so a handler's
	// setTimeout(0) / framework render / rAF that lands a few ms after the action
	// is still observed (preserving the debounce the old fixed sleep gave). Capped
	// by capDur, so a batch step's 25ms cap is honoured.
	settleMinFloor = 24 * time.Millisecond
	// waitConditionChunk bounds a single in-page wait-promise await so one held
	// Runtime.evaluate resolves (returning false at the chunk timeout) before the
	// bridge request timeout (b.timeout) would cancel it; WaitFor re-arms the promise
	// until its own deadline. waitForErrBackoff paces re-arming after a navigation
	// destroys the in-page execution context mid-await, so WaitFor never hot-loops.
	waitConditionChunk = 6 * time.Second
	waitForErrBackoff  = 100 * time.Millisecond
	// activeTabResolveAttempts/Backoff bound how hard contextTabID retries live
	// active-tab resolution before falling back to the last-known cached tab. The
	// MV3 service worker can be mid-reconnect when a call lands; a couple of quick
	// retries ride that out so we don't act on a stale tab.
	activeTabResolveAttempts = 3
	activeTabResolveBackoff  = 150 * time.Millisecond
	// fileChooserPollTimeout/Interval bound how long file-chooser-interception
	// upload mode waits for the Page.fileChooserOpened event after clicking the
	// trigger before giving up, and how often it polls the extension for it.
	fileChooserPollTimeout  = 5 * time.Second
	fileChooserPollInterval = 200 * time.Millisecond
	// bridgeWriteTimeout bounds a single WS frame write with a deadline INDEPENDENT of
	// the request context. coder/websocket registers context.AfterFunc(writeCtx, close)
	// for the duration of a write, so binding the write to the request ctx means a
	// request that cancels (a long wait hitting b.timeout, the upstream 20s HTTP cap, or
	// a caller giving up) while a frame is queued behind a busy extension tears down the
	// WHOLE shared socket — every in-flight RPC then drains "extension disconnected" and
	// the extension reconnects (~1s). That is the observed "10 concurrent heavy calls
	// wedge the bridge, then it auto-recovers". A write completes in well under this cap;
	// decoupling it stops one slow/cancelled request from wedging every concurrent call.
	bridgeWriteTimeout = 10 * time.Second
)

// settleFingerprintExpr is a cheap in-page snapshot of "has the page changed?"
// signals: readyState, the DOM node count, the body text length, the active
// element tag, and the current URL. It is intentionally O(1)-ish (no full DOM
// serialization) so polling it a few times is far cheaper than a real snapshot.
const settleFingerprintExpr = `(function(){try{
  var ae=document.activeElement;
  return document.readyState+'|'+(document.getElementsByTagName('*').length)+'|'+((document.body&&document.body.innerText)?document.body.innerText.length:0)+'|'+(ae?ae.tagName+'#'+(ae.id||''):'')+'|'+location.href;
}catch(e){return 'err';}})()`

// settle replaces the previous blind time.Sleep(capDur) before observing an
// action. It polls a cheap in-page fingerprint and returns as soon as the page
// is stable (two consecutive equal reads) and ready, or when capDur elapses — so
// it is NEVER slower than the old fixed sleep, only faster on a quiescent page.
// It is cancellation-aware (returns immediately if ctx is done) and degrades to
// honouring the remaining cap if the fingerprint cannot be read (disconnected /
// mid-navigation).
func (b *Bridge) settle(ctx context.Context, capDur time.Duration) {
	if capDur <= 0 {
		return
	}
	start := time.Now()
	deadline := start.Add(capDur)
	floor := settleMinFloor
	if floor > capDur {
		floor = capDur
	}
	floorTime := start.Add(floor)
	interval := settlePollStart
	if interval > capDur {
		interval = capDur
	}
	prev := ""
	stable := 0
	// Each fingerprint read is bounded by the settle deadline (not b.timeout), so a
	// connected-but-unresponsive extension cannot turn a 25/75ms settle into a
	// multi-second b.call wait. We bound the WAIT with a watchdog rather than
	// passing a cap-short context to b.evaluate: a context cancelled mid-write
	// makes coder/websocket drop the whole connection, so a slow-but-healthy write
	// must not be force-cancelled. An abandoned read completes harmlessly in the
	// background via the normal b.timeout.
	read := func() (string, bool) {
		type fpRes struct {
			fp string
			ok bool
		}
		resCh := make(chan fpRes, 1)
		go func() {
			var fp string
			// withoutTabLock: this probe can be abandoned by the watchdog below
			// while it keeps running, so it must not hold the tab's serialization
			// lock and stall the next foreground action on that tab.
			err := b.evaluate(withoutTabLock(ctx), settleFingerprintExpr, "", &fp)
			resCh <- fpRes{fp: fp, ok: err == nil && fp != "" && fp != "err"}
		}()
		select {
		case r := <-resCh:
			return r.fp, r.ok
		case <-ctx.Done():
			return "", false
		case <-time.After(time.Until(deadline)):
			return "", false
		}
	}
	if fp, ok := read(); ok {
		prev = fp
		stable = 1
	}
	for time.Now().Before(deadline) {
		wait := interval
		if remaining := time.Until(deadline); wait > remaining {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		fp, ok := read()
		if !ok {
			// Cannot read the page (navigating / disconnected): keep honouring
			// the remaining cap as a plain wait, matching the old behaviour.
			continue
		}
		if fp == prev {
			stable++
			if stable >= settleStableReads && time.Now().After(floorTime) && (strings.HasPrefix(fp, "complete") || strings.HasPrefix(fp, "interactive")) {
				return
			}
		} else {
			prev = fp
			stable = 1
		}
		if interval < settlePollMax {
			interval += interval / 2 // mild geometric backoff (12,18,27,40…)
			if interval > settlePollMax {
				interval = settlePollMax
			}
		}
	}
}

func (b *Bridge) Click(ctx context.Context, ref string) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	beforeTabs := b.captureTabIDs(ctx)
	if err := b.clickRef(ctx, ref); err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBeforeAndTabs(ctx, "clicked "+ref, before, beforeTabs), nil
}

func (b *Bridge) ClickText(ctx context.Context, opts snapshot.ClickTextOptions) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	beforeTabs := b.captureTabIDs(ctx)
	optsJSON, _ := json.Marshal(opts)
	var clicked snapshot.ClickXYResult
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.ClickTextScript, optsJSON), "", &clicked); err != nil {
		return browser.ActionResult{}, err
	}
	if !clicked.OK {
		if clicked.Error == "" {
			clicked.Error = "click text failed"
		}
		return browser.ActionResult{}, fmt.Errorf("click text: %s", clicked.Error)
	}
	b.settle(ctx, observedActionSettle)
	label := opts.Text
	if clicked.Name != "" {
		label = clicked.Name
	}
	return b.observeActionWithBeforeAndTabs(ctx, "clicked text "+strconv.Quote(label), before, beforeTabs), nil
}

func (b *Bridge) Hover(ctx context.Context, ref string) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	if err := b.hoverRef(ctx, ref); err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBefore(ctx, "hovered "+ref, before), nil
}

func (b *Bridge) hoverRef(ctx context.Context, ref string) error {
	box, err := b.resolveBox(ctx, ref)
	if err != nil {
		return err
	}
	if _, err := b.cdp(ctx, "", "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseMoved",
		"x":    box.ViewportX,
		"y":    box.ViewportY,
	}); err != nil {
		return err
	}
	return nil
}

func (b *Bridge) Evaluate(ctx context.Context, expression string) (any, error) {
	var result json.RawMessage
	if err := b.evaluate(ctx, expression, "", &result); err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(result, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func (b *Bridge) NetworkRequests(ctx context.Context, filter string) ([]browser.NetworkRequest, error) {
	filterJSON, _ := json.Marshal(filter)
	expr := fmt.Sprintf(`(function(filter) {
	  var entries = performance.getEntriesByType('resource');
	  if (filter) {
	    var lower = filter.toLowerCase();
	    entries = entries.filter(function(e) { return e.name.toLowerCase().indexOf(lower) !== -1; });
	  }
	  return entries.map(function(e) {
	    return {
	      url: e.name,
	      initiator_type: e.initiatorType || '',
	      start_time: Math.round(e.startTime),
	      duration: Math.round(e.duration),
	      transfer_size: e.transferSize || 0,
	      status: 0
	    };
	  });
	})(%s)`, filterJSON)
	var requests []browser.NetworkRequest
	if err := b.evaluate(ctx, expr, "", &requests); err != nil {
		return nil, err
	}
	return requests, nil
}

func (b *Bridge) clickRef(ctx context.Context, ref string) error {
	box, err := b.resolveBox(ctx, ref)
	if err != nil {
		if fallbackErr := b.activate(ctx, ref); fallbackErr == nil {
			return nil
		}
		return err
	}
	// Fast path: actuate the click with a single in-page round-trip. CDP
	// Input.dispatchMouseEvent blocks on a renderer ack that can cost ~1.5s per
	// call on heavy pages (≈5s for the three-event sequence below); the in-page
	// pointer/mouse/click sequence fires the same handlers in one Runtime.evaluate
	// (~tens of ms). Trusted CDP dispatch stays as the fallback when the point is
	// not hit-testable in-page (e.g. element scrolled out of the layout viewport).
	xJSON, _ := json.Marshal(box.ViewportX)
	yJSON, _ := json.Marshal(box.ViewportY)
	var inPage snapshot.ClickXYResult
	if evalErr := b.evaluate(ctx, fmt.Sprintf("%s(%s,%s)", snapshot.ClickXYScript, xJSON, yJSON), "", &inPage); evalErr == nil && inPage.OK {
		return nil
	}
	for _, typ := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		if _, err := b.cdp(ctx, "", "Input.dispatchMouseEvent", map[string]any{
			"type":   typ,
			"x":      box.ViewportX,
			"y":      box.ViewportY,
			"button": "left",
			"buttons": func() int {
				if typ == "mousePressed" {
					return 1
				}
				return 0
			}(),
			"clickCount": 1,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bridge) activate(ctx context.Context, ref string) error {
	refJSON, _ := json.Marshal(ref)
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	expr := fmt.Sprintf(`(function(ref) {
	  function roots() {
	    const out = [document];
	    for (let i = 0; i < out.length; i++) {
	      const root = out[i];
	      if (!root.querySelectorAll) continue;
	      for (const el of Array.from(root.querySelectorAll('*'))) {
	        if (el.shadowRoot) out.push(el.shadowRoot);
	      }
	    }
	    return out;
	  }
	  function findByRef(ref) {
	    const selector = '[data-brw-ref="' + CSS.escape(ref) + '"]';
	    for (const root of roots()) {
	      const el = root.querySelector && root.querySelector(selector);
	      if (el) return el;
	    }
	    return null;
	  }
	  const el = findByRef(ref);
	  if (!el) return { ok: false, error: 'ref not found' };
	  if (el.closest('[hidden],[aria-hidden="true"]')) return { ok: false, error: 'ref hidden' };
	  el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
	  if (typeof el.focus === 'function') el.focus({ preventScroll: true });
	  el.dispatchEvent(new MouseEvent('mouseover', { bubbles: true, cancelable: true, view: window }));
	  el.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true, view: window }));
	  el.dispatchEvent(new MouseEvent('mouseup', { bubbles: true, cancelable: true, view: window }));
	  if (typeof el.click === 'function') el.click();
	  else el.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, view: window }));
	  return { ok: true };
	})(%s)`, refJSON)
	if err := b.evaluate(ctx, expr, "", &result); err != nil {
		return err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "ref activation failed"
		}
		return fmt.Errorf("activate: %s", result.Error)
	}
	return nil
}

func (b *Bridge) Type(ctx context.Context, ref, text string) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	if err := b.typeRef(ctx, ref, text); err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBefore(ctx, "typed into "+ref, before), nil
}

func (b *Bridge) typeRef(ctx context.Context, ref, text string) error {
	if err := b.focus(ctx, ref); err != nil {
		return err
	}
	_, err := b.cdp(ctx, "", "Input.insertText", map[string]any{"text": text})
	return err
}

func (b *Bridge) Fill(ctx context.Context, opts snapshot.FillOptions) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	ref, err := b.fillOptions(ctx, opts)
	if err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBefore(ctx, "filled "+ref, before), nil
}

func (b *Bridge) fillOptions(ctx context.Context, opts snapshot.FillOptions) (string, error) {
	ref, err := b.resolveFillRef(ctx, opts)
	if err != nil {
		return "", err
	}
	refJSON, _ := json.Marshal(ref)
	textJSON, _ := json.Marshal(opts.Text)
	replaceJSON, _ := json.Marshal(opts.Replace)
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s,%s,%s)", snapshot.FillElementScript, refJSON, textJSON, replaceJSON), "", &result); err != nil {
		return "", err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "fill failed"
		}
		return "", fmt.Errorf("fill: %s", result.Error)
	}
	return ref, nil
}

func (b *Bridge) resolveFillRef(ctx context.Context, opts snapshot.FillOptions) (string, error) {
	if opts.Ref != "" {
		return opts.Ref, nil
	}
	result, err := b.Find(ctx, snapshot.FindOptions{
		Query: opts.Query,
		Role:  opts.Role,
		Limit: 1,
	})
	if err != nil {
		return "", err
	}
	if len(result.Elements) == 0 {
		return "", fmt.Errorf("no fill target found for query %q", opts.Query)
	}
	return result.Elements[0].Ref, nil
}

func (b *Bridge) UploadFile(ctx context.Context, opts snapshot.UploadOptions) (browser.ActionResult, error) {
	// Resolve the upload source (local path(s), inline bytes_base64, or remote
	// URL). bytes/url sources are materialized to temp files on the daemon host
	// and removed once DOM.setFileInputFiles has consumed them.
	paths, cleanup, err := browser.ResolveUploadPaths(ctx, opts)
	if err != nil {
		return browser.ActionResult{}, err
	}
	defer cleanup()

	// File-chooser-interception mode: when a trigger is named, click it with the
	// native chooser intercepted and set the file on whatever input the chooser
	// reports. Handles SPAs that create the input on click (which would otherwise
	// freeze the CDP session behind a native OS dialog) and inputs in cross-origin
	// iframes (backendNodeId is frame-agnostic).
	if opts.ClickRef != "" || opts.ClickText != "" {
		return b.uploadViaFileChooser(ctx, opts, paths)
	}

	ref := opts.Ref
	if ref == "" {
		query := opts.Query
		if strings.TrimSpace(query) == "" {
			query = "file"
		}
		result, err := b.Find(ctx, snapshot.FindOptions{
			Query: query,
			Role:  opts.Role,
			Limit: 20,
		})
		if err != nil {
			return browser.ActionResult{}, err
		}
		for _, el := range result.Elements {
			if el.Tag == "input" && el.Type == "file" {
				ref = el.Ref
				break
			}
		}
		if ref == "" {
			return browser.ActionResult{}, fmt.Errorf("no file input found for query %q", query)
		}
	}

	refJSON, _ := json.Marshal(ref)
	raw, err := b.cdp(ctx, "", "Runtime.evaluate", map[string]any{
		"expression":    fmt.Sprintf("%s(%s)", snapshot.FileInputElementScript, refJSON),
		"returnByValue": false,
		"awaitPromise":  true,
		"objectGroup":   "brw-upload",
	})
	if err != nil {
		return browser.ActionResult{}, err
	}
	var eval struct {
		Result struct {
			ObjectID string `json:"objectId"`
		} `json:"result"`
		ExceptionDetails any `json:"exceptionDetails,omitempty"`
	}
	if err := json.Unmarshal(raw, &eval); err != nil {
		return browser.ActionResult{}, err
	}
	if eval.ExceptionDetails != nil {
		details, _ := json.Marshal(eval.ExceptionDetails)
		return browser.ActionResult{}, fmt.Errorf("file input resolution failed: %s", details)
	}
	if eval.Result.ObjectID == "" {
		return browser.ActionResult{}, errors.New("file input resolution returned no object id")
	}
	defer func() {
		_, _ = b.cdp(ctx, "", "Runtime.releaseObject", map[string]any{"objectId": eval.Result.ObjectID})
	}()
	before := b.captureSemanticState(ctx)
	if _, err := b.cdp(ctx, "", "DOM.setFileInputFiles", map[string]any{
		"files":    paths,
		"objectId": eval.Result.ObjectID,
	}); err != nil {
		return browser.ActionResult{}, err
	}
	var ignored any
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.FileInputEventsScript, refJSON), "", &ignored); err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBefore(ctx, "uploaded file to "+ref, before), nil
}

// uploadViaFileChooser drives the file-chooser-interception upload path: enable
// native-dialog interception, click the trigger, capture the chooser's
// backendNodeId from the Page.fileChooserOpened event the extension stashes, and
// set the file with DOM.setFileInputFiles. Interception is ALWAYS disabled on
// exit so the user's manual uploads in this Chrome are unaffected.
func (b *Bridge) uploadViaFileChooser(ctx context.Context, opts snapshot.UploadOptions, paths []string) (browser.ActionResult, error) {
	// Pin the tab for the whole sequence so interception, click, poll, and set all
	// target the same tab even if the user switches tabs mid-upload.
	tabID := b.contextTabID(ctx)

	if _, err := b.call(ctx, "set_intercept_file_chooser", map[string]any{
		"tabId":   parseTabID(tabID),
		"enabled": true,
	}); err != nil {
		return browser.ActionResult{}, fmt.Errorf("enable file chooser interception: %w", err)
	}
	defer func() {
		// Always restore manual uploads, even on error. Use a fresh context so a
		// cancelled/expired ctx cannot leave interception stuck on.
		disableCtx, cancel := context.WithTimeout(context.Background(), b.timeout)
		defer cancel()
		_, _ = b.call(disableCtx, "set_intercept_file_chooser", map[string]any{
			"tabId":   parseTabID(tabID),
			"enabled": false,
		})
	}()

	before := b.captureSemanticState(ctx)

	// Click the trigger that opens the (now intercepted) native chooser.
	if opts.ClickRef != "" {
		if err := b.clickRef(ctx, opts.ClickRef); err != nil {
			return browser.ActionResult{}, fmt.Errorf("click upload trigger %s: %w", opts.ClickRef, err)
		}
	} else {
		optsJSON, _ := json.Marshal(snapshot.ClickTextOptions{Text: opts.ClickText, Role: opts.Role})
		var clicked snapshot.ClickXYResult
		if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.ClickTextScript, optsJSON), tabID, &clicked); err != nil {
			return browser.ActionResult{}, fmt.Errorf("click upload trigger %q: %w", opts.ClickText, err)
		}
		if !clicked.OK {
			msg := clicked.Error
			if msg == "" {
				msg = "click text failed"
			}
			return browser.ActionResult{}, fmt.Errorf("click upload trigger %q: %s", opts.ClickText, msg)
		}
	}

	// Poll for the captured Page.fileChooserOpened event (up to ~5s).
	var backendNodeID int64
	deadline := time.Now().Add(fileChooserPollTimeout)
	for {
		var ev struct {
			Captured      bool  `json:"captured"`
			BackendNodeID int64 `json:"backendNodeId"`
		}
		raw, err := b.call(ctx, "get_file_chooser_event", map[string]any{"tabId": parseTabID(tabID)})
		if err == nil {
			if jsonErr := json.Unmarshal(raw, &ev); jsonErr == nil && ev.Captured {
				if ev.BackendNodeID == 0 {
					return browser.ActionResult{}, errors.New("file chooser opened but reported no backendNodeId")
				}
				backendNodeID = ev.BackendNodeID
				break
			}
		}
		if time.Now().After(deadline) {
			return browser.ActionResult{}, fmt.Errorf("no file chooser opened within %s after clicking the trigger — confirm the trigger opens a file picker", fileChooserPollTimeout)
		}
		select {
		case <-ctx.Done():
			return browser.ActionResult{}, ctx.Err()
		case <-time.After(fileChooserPollInterval):
		}
	}

	if _, err := b.cdp(ctx, tabID, "DOM.setFileInputFiles", map[string]any{
		"files":         paths,
		"backendNodeId": backendNodeID,
	}); err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBefore(ctx, "uploaded file via intercepted file chooser", before), nil
}

func (b *Bridge) Select(ctx context.Context, ref, value string) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	message, err := b.selectValue(ctx, ref, value)
	if err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBefore(ctx, message, before), nil
}

func (b *Bridge) selectValue(ctx context.Context, ref, value string) (string, error) {
	refJSON, _ := json.Marshal(ref)
	valueJSON, _ := json.Marshal(value)
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s,%s)", snapshot.SelectElementScript, refJSON, valueJSON), "", &result); err != nil {
		return "", err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "select failed"
		}
		if !strings.Contains(result.Error, "ref is not a select element") {
			return "", fmt.Errorf("select: %s", result.Error)
		}
		return b.selectCustomOption(ctx, ref, value)
	}
	return "selected " + ref, nil
}

func (b *Bridge) selectCustomOption(ctx context.Context, ref, value string) (string, error) {
	if b.elementValueMatches(ctx, ref, value) {
		return "selected " + ref + " already " + value, nil
	}
	option, err := b.findOptionCandidate(ctx, value)
	if err != nil {
		if err := b.clickRef(ctx, ref); err != nil {
			return "", fmt.Errorf("open custom select %s: %w", ref, err)
		}
		b.settle(ctx, observedActionSettle)
		option, err = b.findOptionCandidate(ctx, value)
		if err != nil {
			return "", err
		}
	}
	if err := b.clickRef(ctx, option.Ref); err != nil {
		return "", fmt.Errorf("select option %s: %w", option.Ref, err)
	}
	return "selected " + ref + " via option " + option.Ref, nil
}

func (b *Bridge) elementValueMatches(ctx context.Context, ref, value string) bool {
	snap, err := b.Snapshot(ctx, snapshot.SnapshotOptions{Limit: 0, ViewportOnly: false})
	if err != nil {
		return false
	}
	for _, el := range snap.Elements {
		if el.Ref == ref && browser.ElementMatchesOptionValue(el, value) {
			return true
		}
	}
	return false
}

func (b *Bridge) findOptionCandidate(ctx context.Context, value string) (snapshot.Element, error) {
	for _, opts := range []snapshot.SnapshotOptions{
		{Role: "option", Query: value, Limit: 100, ViewportOnly: false},
		{Role: "option", Limit: 200, ViewportOnly: false},
	} {
		snap, err := b.Snapshot(ctx, opts)
		if err != nil {
			return snapshot.Element{}, err
		}
		if option, ok := browser.SelectOptionCandidate(snap.Elements, value); ok {
			return option, nil
		}
	}
	return snapshot.Element{}, fmt.Errorf("no visible option found for %q", value)
}

func (b *Bridge) Press(ctx context.Context, key string) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	if err := b.pressKey(ctx, key); err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBefore(ctx, "pressed "+key, before), nil
}

func (b *Bridge) pressKey(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("key is required")
	}
	desc := actions.DescribeKey(key)
	if desc.Key == "" {
		return errors.New("key is required")
	}
	for _, typ := range []string{"keyDown", "keyUp"} {
		params := map[string]any{
			"type":                  typ,
			"modifiers":             desc.Modifiers,
			"key":                   desc.Key,
			"code":                  desc.Code,
			"windowsVirtualKeyCode": desc.WindowsVirtualKeyCode,
			"nativeVirtualKeyCode":  desc.WindowsVirtualKeyCode,
		}
		if typ == "keyDown" && desc.Text != "" {
			params["text"] = desc.Text
			params["unmodifiedText"] = desc.Text
		}
		if _, err := b.cdp(ctx, "", "Input.dispatchKeyEvent", params); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bridge) Scroll(ctx context.Context, direction string) (browser.ActionResult, error) {
	before := b.captureSemanticState(ctx)
	message, err := b.scrollDirection(ctx, direction)
	if err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	return b.observeActionWithBefore(ctx, message, before), nil
}

// Navigate moves through the active tab's session history (back/forward) or
// reloads the current document via the in-page History/Location web APIs, then
// returns a post-navigation observation. Standards-only: history.back(),
// history.forward(), location.reload().
func (b *Bridge) Navigate(ctx context.Context, direction string) (browser.ActionResult, error) {
	dir, err := normalizeNavigateDirection(direction)
	if err != nil {
		return browser.ActionResult{}, err
	}
	before := b.captureSemanticState(ctx)
	if err := b.navigateDirection(ctx, dir); err != nil {
		return browser.ActionResult{}, err
	}
	// A history move / reload may tear down and rebuild the document; give it a
	// moment to settle, then wait for readiness before observing.
	b.settle(ctx, observedActionSettle)
	_ = b.WaitFor(ctx, "load", 10*time.Second)
	return b.observeActionWithBefore(ctx, "navigated "+dir, before), nil
}

// NavigateTo navigates the active tab to a URL, waits for the page to load,
// and returns a post-navigation observation. Unlike Open, this does NOT create
// a new tab — it navigates the existing active tab.
func (b *Bridge) NavigateTo(ctx context.Context, url string) (browser.ActionResult, error) {
	if strings.TrimSpace(url) == "" {
		return browser.ActionResult{}, fmt.Errorf("navigate_to: url is required")
	}
	if !strings.Contains(url, "://") {
		url = "https://" + url
	}
	before := b.captureSemanticState(ctx)
	beforeTabs := b.captureTabIDs(ctx)
	if err := b.navigateToURL(ctx, url); err != nil {
		return browser.ActionResult{}, err
	}
	b.settle(ctx, observedActionSettle)
	_ = b.WaitFor(ctx, "load", 10*time.Second)
	return b.observeActionWithBeforeAndTabs(ctx, "navigated to "+url, before, beforeTabs), nil
}

func (b *Bridge) navigateToURL(ctx context.Context, url string) error {
	urlJSON, _ := json.Marshal(url)
	expr := fmt.Sprintf("(function(){location.href=%s;return true;})()", urlJSON)
	var ok bool
	if err := b.evaluate(ctx, expr, "", &ok); err != nil {
		if isNavigationTeardownError(err) {
			return nil
		}
		return err
	}
	return nil
}

func (b *Bridge) navigateDirection(ctx context.Context, dir string) error {
	var expr string
	switch dir {
	case navigateBack:
		expr = "(function(){history.back();return true;})()"
	case navigateForward:
		expr = "(function(){history.forward();return true;})()"
	case navigateReload:
		expr = "(function(){location.reload();return true;})()"
	default:
		return fmt.Errorf("direction must be one of back, forward, reload; got %q", dir)
	}
	var ok bool
	if err := b.evaluate(ctx, expr, "", &ok); err != nil {
		// A reload/history move can destroy the execution context mid-evaluate;
		// that is the expected outcome of navigation, not a failure.
		if isNavigationTeardownError(err) {
			return nil
		}
		return err
	}
	return nil
}

const (
	navigateBack    = "back"
	navigateForward = "forward"
	navigateReload  = "reload"
)

func normalizeNavigateDirection(direction string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(direction))
	switch d {
	case navigateBack, navigateForward, navigateReload:
		return d, nil
	default:
		return "", fmt.Errorf("direction must be one of back, forward, reload; got %q", direction)
	}
}

func isNavigationTeardownError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "execution context was destroyed") ||
		strings.Contains(msg, "cannot find context with specified id") ||
		strings.Contains(msg, "frame was detached") ||
		strings.Contains(msg, "no by-value result")
}

func (b *Bridge) scrollDirection(ctx context.Context, direction string) (string, error) {
	direction = strings.ToLower(strings.TrimSpace(direction))
	if direction == "" {
		direction = "down"
	}
	directionJSON, _ := json.Marshal(direction)
	var scroll snapshot.ScrollResult
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.ScrollPageScript, directionJSON), "", &scroll); err != nil {
		return "", err
	}
	if !scroll.OK {
		if scroll.Error == "" {
			scroll.Error = "scroll failed"
		}
		return "", fmt.Errorf("scroll: %s", scroll.Error)
	}
	message := fmt.Sprintf("scrolled %s target:%s", direction, scroll.Target)
	if scroll.Name != "" {
		message += " " + strconv.Quote(scroll.Name)
	}
	return message, nil
}

func (b *Bridge) ExecutePlan(ctx context.Context, steps []browser.PlanStep) (browser.PlanResult, error) {
	entry, release := b.cancels.register(ctx, cancelToken(ctx, ""))
	defer release()
	ctx = entry.ctx

	// Resolve the active tab once for the whole plan and pin it into the step
	// context (re-pinned after focus_tab / open steps that move focus) so each
	// step's contextTabID() short-circuits instead of re-resolving per step.
	stepCtx := b.pinActiveTab(ctx)

	result := browser.PlanResult{OK: true, Steps: make([]browser.PlanStepResult, 0, len(steps))}
	for i, step := range steps {
		if entry.Cancelled() {
			result.Cancelled = true
			result.OK = false
			result.Error = "cancelled"
			result.StepsCompleted = len(result.Steps)
			return result, nil
		}
		stepResult, retargetTo := b.executePlanStep(stepCtx, i, step)
		stepCtx = b.retargetPinnedTab(ctx, stepCtx, retargetTo)
		result.Steps = append(result.Steps, stepResult)
		if !stepResult.OK {
			if entry.Cancelled() {
				result.Cancelled = true
				result.OK = false
				result.Error = "cancelled"
				result.StepsCompleted = i
				return result, nil
			}
			result.OK = false
			failedAt := i
			result.FailedAt = &failedAt
			result.Error = stepResult.Error
			result.StepsCompleted = i
			return result, nil
		}
	}
	result.StepsCompleted = len(result.Steps)
	return result, nil
}

// executePlanStep runs one plan step and returns its result plus the retarget
// target tab id — the KNOWN id a successful focus_tab/open moved focus to ("" for
// any other step or on failure) — so the loop re-pins without the active cache.
func (b *Bridge) executePlanStep(ctx context.Context, index int, step browser.PlanStep) (browser.PlanStepResult, string) {
	sr := browser.PlanStepResult{Index: index, Action: step.Action, OK: true}
	retargetTo := ""

	if step.ExpectRef != "" {
		findResult, err := b.Find(ctx, snapshot.FindOptions{Query: step.ExpectRef, Limit: 1})
		if err != nil {
			sr.OK = false
			sr.Error = fmt.Sprintf("expect_ref %q lookup failed: %v", step.ExpectRef, err)
			return sr, retargetTo
		}
		if len(findResult.Elements) == 0 {
			sr.OK = false
			sr.Error = fmt.Sprintf("expect_ref %q not found", step.ExpectRef)
			return sr, retargetTo
		}
		if step.ExpectRole != "" && findResult.Elements[0].Role != step.ExpectRole {
			sr.OK = false
			sr.Error = fmt.Sprintf("expect_ref %q has role %q, expected %q", step.ExpectRef, findResult.Elements[0].Role, step.ExpectRole)
			return sr, retargetTo
		}
	}

	var actionErr error
	switch step.Action {
	case "click":
		if step.Ref == "" {
			actionErr = errors.New("click requires ref")
			break
		}
		actionErr = b.clickRef(ctx, step.Ref)
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": "clicked " + step.Ref, "ref": step.Ref}
		}
		b.settle(ctx, batchActionSettle)
	case "type":
		if step.Ref == "" || step.Text == "" {
			actionErr = errors.New("type requires ref and text")
			break
		}
		actionErr = b.typeRef(ctx, step.Ref, step.Text)
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": "typed into " + step.Ref, "ref": step.Ref}
		}
		b.settle(ctx, batchActionSettle)
	case "fill":
		var ref string
		ref, actionErr = b.fillOptions(ctx, snapshot.FillOptions{Ref: step.Ref, Text: step.Text, Replace: true})
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": "filled " + ref, "ref": ref}
		}
		b.settle(ctx, batchActionSettle)
	case "select":
		if step.Ref == "" || step.Value == "" {
			actionErr = errors.New("select requires ref and value")
			break
		}
		var message string
		message, actionErr = b.selectValue(ctx, step.Ref, step.Value)
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": message, "ref": step.Ref, "value": step.Value}
		}
		b.settle(ctx, batchActionSettle)
	case "press":
		if step.Key == "" {
			actionErr = errors.New("press requires key")
			break
		}
		actionErr = b.pressKey(ctx, step.Key)
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": "pressed " + step.Key, "key": step.Key}
		}
		b.settle(ctx, batchActionSettle)
	case "scroll":
		var message string
		message, actionErr = b.scrollDirection(ctx, step.Direction)
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": message, "direction": step.Direction}
		}
		b.settle(ctx, batchActionSettle)
	case "hover":
		if step.Ref == "" {
			actionErr = errors.New("hover requires ref")
			break
		}
		actionErr = b.hoverRef(ctx, step.Ref)
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": "hovered " + step.Ref, "ref": step.Ref}
		}
		b.settle(ctx, batchActionSettle)
	case "wait":
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = b.timeout
		}
		actionErr = b.WaitFor(ctx, step.Condition, timeout)
		if actionErr == nil {
			sr.Result = map[string]any{"ok": true, "message": "wait matched " + step.Condition, "condition": step.Condition}
		}
	case "read":
		var read readability.PageRead
		read, actionErr = b.Read(ctx)
		sr.Result = read
		if actionErr == nil {
			sr.Message = "read captured"
		}
	case "snapshot":
		snap, err := b.Snapshot(ctx, snapshot.SnapshotOptions{ViewportOnly: true})
		if err != nil {
			actionErr = err
			break
		}
		sr.Snapshot = &snap
		sr.Result = snap
		sr.Message = "snapshot captured"
	case "open":
		if step.URL == "" {
			actionErr = errors.New("open requires url")
			break
		}
		var openRes browser.OpenResult
		openRes, actionErr = b.Open(ctx, step.URL)
		if actionErr == nil {
			retargetTo = openRes.Tab.ID
			sr.Result = openRes
		}
	case "focus_tab":
		if step.ID == "" {
			actionErr = errors.New("focus_tab requires id")
			break
		}
		actionErr = b.FocusTab(ctx, step.ID)
		if actionErr == nil {
			retargetTo = step.ID
			sr.Result = map[string]any{"ok": true, "message": "focused tab " + step.ID, "tab_id": step.ID}
		}
	default:
		actionErr = fmt.Errorf("unknown action %q", step.Action)
	}

	if actionErr != nil {
		sr.OK = false
		sr.Error = actionErr.Error()
	}
	if sr.Message == "" && sr.OK {
		sr.Message = step.Action + " ok"
	}
	return sr, retargetTo
}

func (b *Bridge) WaitFor(ctx context.Context, condition string, timeout time.Duration) error {
	if timeout == 0 {
		timeout = b.timeout
	}
	deadline := time.Now().Add(timeout)
	// Wait via a SINGLE in-page promise (WaitConditionScript) that resolves the
	// instant the condition holds — a MutationObserver/history-driven check running
	// inside the renderer — instead of re-evaluating a heavy condition script across
	// the CDP boundary every 25-250ms. The old cross-process poll made each tick a
	// full document.body.innerText / shadow-DOM walk; ten concurrent waits against a
	// large (10k-row) page flooded the extension's debugger with hundreds of heavy
	// evaluates a second and wedged the whole bridge until the waits expired. One
	// awaited in-page promise per wait keeps bridge load flat no matter how many
	// waits run concurrently. The await is chunked under b.timeout and re-armed, so a
	// navigation that destroys the execution context simply continues against the new
	// document.
	for {
		// Cooperative cancellation: a Cancel on the surrounding plan/batch (or this
		// tab) cancels ctx, unblocking a long wait promptly.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait for %q cancelled", condition)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("timed out waiting for %q after %s; the condition was never met — check that the page is loaded and the condition is correct (valid: ready, committed, text:..., url:..., title:..., ref:..., page_ready)", condition, timeout)
		}
		chunk := remaining
		if limit := b.waitChunkLimit(); chunk > limit {
			chunk = limit
		}
		matched, err := b.waitConditionOnce(ctx, condition, chunk)
		if err == nil && matched {
			return nil
		}
		if err != nil {
			// A navigation can destroy the in-page execution context mid-await; pause
			// briefly, then re-arm the promise against the new document rather than
			// hot-looping on the transient "context was destroyed" error.
			select {
			case <-ctx.Done():
				return fmt.Errorf("wait for %q cancelled", condition)
			case <-time.After(waitForErrBackoff):
			}
		}
	}
}

// waitChunkLimit bounds a single in-page wait await so the held Runtime.evaluate
// resolves (false at the chunk timeout) before the bridge request timeout would
// cancel it, leaving headroom for the WS round-trip.
func (b *Bridge) waitChunkLimit() time.Duration {
	limit := waitConditionChunk
	if budget := b.timeout - 2*time.Second; budget > 0 && budget < limit {
		limit = budget
	}
	if limit <= 0 {
		limit = time.Second
	}
	return limit
}

// waitConditionOnce arms the in-page WaitConditionScript promise once and awaits
// its resolution: true the instant the condition holds, false at chunk. The heavy
// condition check (innerText / shadow-DOM walk for text:/ref:) runs inside the
// renderer on DOM mutations, NOT as repeated cross-process evaluates — so N
// concurrent waits cost N held evaluates, not N*(rate) heavy round-trips.
func (b *Bridge) waitConditionOnce(ctx context.Context, condition string, chunk time.Duration) (bool, error) {
	chunkMs := chunk.Milliseconds()
	if chunkMs < 0 {
		chunkMs = 0
	}
	condJSON, _ := json.Marshal(condition)
	expr := fmt.Sprintf("%s(%s,%d)", snapshot.WaitConditionScript, condJSON, chunkMs)
	var matched bool
	if err := b.evaluate(ctx, expr, "", &matched); err != nil {
		return false, err
	}
	return matched, nil
}

const (
	bridgeScreenshotMaxWidth       = 800
	bridgeScreenshotJPEGQuality    = 50
	bridgeScreenshotAnnotateMaxDim = 900
)

func (b *Bridge) Screenshot(ctx context.Context) (browser.Screenshot, error) {
	tabID := b.contextTabID(ctx)
	params := map[string]any{"format": "jpeg", "quality": bridgeScreenshotJPEGQuality}
	if vw, vh := b.viewportDimensions(ctx, tabID); vw > 0 && vh > 0 {
		scale := 1.0
		if vw > bridgeScreenshotMaxWidth {
			scale = bridgeScreenshotMaxWidth / vw
		}
		params["clip"] = map[string]any{"x": 0, "y": 0, "width": vw, "height": vh, "scale": scale}
	}
	raw, err := b.cdp(ctx, tabID, "Page.captureScreenshot", params)
	if err != nil {
		return browser.Screenshot{}, err
	}
	return screenshotFromRawMIME(raw, "image/jpeg")
}

func (b *Bridge) ScreenshotElement(ctx context.Context, ref string) (browser.Screenshot, error) {
	box, err := b.resolveBox(ctx, ref)
	if err != nil {
		return browser.Screenshot{}, err
	}
	raw, err := b.cdp(ctx, "", "Page.captureScreenshot", map[string]any{
		"format": "png",
		"clip": map[string]any{
			"x":      box.X,
			"y":      box.Y,
			"width":  box.Width,
			"height": box.Height,
			"scale":  1,
		},
	})
	if err != nil {
		return browser.Screenshot{}, err
	}
	return screenshotFromRaw(raw)
}

func (b *Bridge) viewportDimensions(ctx context.Context, tabID string) (float64, float64) {
	var dims []float64
	if err := b.evaluate(ctx, `[Math.round(window.innerWidth), Math.round(window.innerHeight)]`, tabID, &dims); err != nil {
		return 0, 0
	}
	if len(dims) != 2 || dims[0] <= 0 || dims[1] <= 0 {
		return 0, 0
	}
	return dims[0], dims[1]
}

// ScreenshotAnnotated draws a Set-of-Marks overlay (ref-labelled boxes over the
// in-viewport frontier elements), captures the page, removes the overlay, and
// returns the PNG plus a ref->box legend. It mirrors the direct-CDP manager path
// but runs the overlay JS over the bridge's own Runtime.evaluate channel. The
// overlay is removed in every path so the page the agent acts on is unmutated.
func (b *Bridge) ScreenshotAnnotated(ctx context.Context, aopts browser.AnnotatedScreenshotOptions) (browser.AnnotatedScreenshot, error) {
	mode := aopts.Mode
	if strings.TrimSpace(mode) == "" {
		mode = snapshot.DefaultSnapshotMode
	}
	opts := snapshot.NormalizeOptions(snapshot.SnapshotOptions{Mode: mode})
	snap, err := b.Snapshot(ctx, opts)
	if err != nil {
		return browser.AnnotatedScreenshot{}, err
	}

	tabID := b.contextTabID(ctx)

	// Resolve the optional crop clip (ref -> element box, or explicit region),
	// clamped to the viewport, in top-level viewport space — the same space the
	// overlay labels are painted at. nil means a full-viewport capture.
	clip, clipErr := b.resolveAnnotationClip(ctx, tabID, aopts)
	if clipErr != nil {
		return browser.AnnotatedScreenshot{}, clipErr
	}

	marks := make([]snapshot.AnnotationMark, 0, len(snap.Elements))
	meta := make(map[string]snapshot.Element, len(snap.Elements))
	for _, el := range snap.Elements {
		if !el.InViewport {
			continue
		}
		marks = append(marks, snapshot.AnnotationMark{Ref: el.Ref, Name: el.Name, Role: el.Role})
		meta[el.Ref] = el
	}

	injectExpr, err := snapshot.InjectAnnotationOverlayExpr(marks)
	if err != nil {
		return browser.AnnotatedScreenshot{}, err
	}
	var overlay snapshot.AnnotationOverlayResult
	err = b.evaluate(ctx, injectExpr, tabID, &overlay)
	// Always remove the overlay, even when injection errored partway.
	defer func() {
		var discard json.RawMessage
		_ = b.evaluate(ctx, snapshot.RemoveAnnotationOverlayExpr(), tabID, &discard)
	}()
	if err != nil {
		return browser.AnnotatedScreenshot{}, err
	}

	capParams := map[string]any{"format": "png"}
	if clip != nil {
		scale := 1.0
		longest := clip.Width
		if clip.Height > longest {
			longest = clip.Height
		}
		if longest > bridgeScreenshotAnnotateMaxDim {
			scale = bridgeScreenshotAnnotateMaxDim / longest
		}
		capParams["clip"] = map[string]any{"x": clip.X, "y": clip.Y, "width": clip.Width, "height": clip.Height, "scale": scale}
	} else if vw, vh := b.viewportDimensions(ctx, tabID); vw > 0 && vh > 0 {
		scale := 1.0
		longest := vw
		if vh > longest {
			longest = vh
		}
		if longest > bridgeScreenshotAnnotateMaxDim {
			scale = bridgeScreenshotAnnotateMaxDim / longest
		}
		capParams["clip"] = map[string]any{"x": 0, "y": 0, "width": vw, "height": vh, "scale": scale}
	}
	raw, err := b.cdp(ctx, tabID, "Page.captureScreenshot", capParams)
	if err != nil {
		return browser.AnnotatedScreenshot{}, err
	}
	shot, err := screenshotFromRaw(raw)
	if err != nil {
		return browser.AnnotatedScreenshot{}, err
	}

	legend := make(map[string]browser.LegendEntry, len(overlay.Legend))
	for _, box := range overlay.Legend {
		if !box.OK {
			continue
		}
		if clip != nil && !annotationBoxIntersects(box, clip) {
			continue
		}
		el := meta[box.Ref]
		legend[box.Ref] = browser.LegendEntry{
			Ref:    box.Ref,
			Name:   el.Name,
			Role:   el.Role,
			X:      box.X,
			Y:      box.Y,
			Width:  box.Width,
			Height: box.Height,
		}
	}

	return browser.AnnotatedScreenshot{
		MIMEType: shot.MIMEType,
		Data:     shot.Data,
		Base64:   shot.Base64,
		Legend:   legend,
	}, nil
}

// annotationClipMargin pads a ref-derived crop so the label badge and border are
// not sliced off the edge of the crop.
const annotationClipMargin = 18.0

// annotationClip is the bridge's resolved viewport clip for an annotated crop.
type annotationClip struct {
	X, Y, Width, Height float64
}

// resolveAnnotationClip turns the requested ref/region into a viewport clip,
// clamped to the page viewport. Returns nil for a full-viewport capture. Box and
// viewport resolution run over the bridge's own evaluate channel.
func (b *Bridge) resolveAnnotationClip(ctx context.Context, tabID string, aopts browser.AnnotatedScreenshotOptions) (*annotationClip, error) {
	var x, y, w, h float64
	switch {
	case strings.TrimSpace(aopts.Ref) != "":
		box, err := b.resolveBox(browser.WithTabID(ctx, tabID), aopts.Ref)
		if err != nil {
			return nil, err
		}
		x = box.ViewportX - box.Width/2 - annotationClipMargin
		y = box.ViewportY - box.Height/2 - annotationClipMargin
		w = box.Width + 2*annotationClipMargin
		h = box.Height + 2*annotationClipMargin
	case !aopts.Region.IsZero():
		x = aopts.Region.X - annotationClipMargin
		y = aopts.Region.Y - annotationClipMargin
		w = aopts.Region.Width + 2*annotationClipMargin
		h = aopts.Region.Height + 2*annotationClipMargin
	default:
		return nil, nil
	}
	var dims struct {
		W float64 `json:"w"`
		H float64 `json:"h"`
	}
	_ = b.evaluate(ctx, `({w: window.innerWidth||document.documentElement.clientWidth||0, h: window.innerHeight||document.documentElement.clientHeight||0})`, tabID, &dims)
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if dims.W > 0 && x+w > dims.W {
		w = dims.W - x
	}
	if dims.H > 0 && y+h > dims.H {
		h = dims.H - y
	}
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("screenshot clip resolves to an empty region")
	}
	return &annotationClip{X: x, Y: y, Width: w, Height: h}, nil
}

// annotationBoxIntersects reports whether an overlay box overlaps the clip
// rectangle (both in top-level viewport space), used to prune the legend.
func annotationBoxIntersects(box snapshot.AnnotationBox, clip *annotationClip) bool {
	return box.X < clip.X+clip.Width && box.X+box.Width > clip.X &&
		box.Y < clip.Y+clip.Height && box.Y+box.Height > clip.Y
}

func (b *Bridge) observeAction(ctx context.Context, message string) browser.ActionResult {
	return b.observeActionWithBefore(ctx, message, nil)
}

func (b *Bridge) observeActionWithBefore(ctx context.Context, message string, before *browser.SemanticState) browser.ActionResult {
	return b.observeActionWithBeforeAndTabs(ctx, message, before, nil)
}

func (b *Bridge) observeActionWithBeforeAndTabs(ctx context.Context, message string, before *browser.SemanticState, beforeTabIDs map[string]bool) browser.ActionResult {
	result := browser.ActionResult{OK: true, Message: message, TabID: b.contextTabID(ctx)}
	snap, err := b.Snapshot(ctx, snapshot.SnapshotOptions{ViewportOnly: true})
	if err != nil {
		result.OK = false
		result.Message = message + "; observation failed: " + err.Error()
		return result
	}
	result.URL = snap.URL
	result.Title = snap.Title
	if snap.Metadata != nil {
		result.Version = browser.MetadataInt64(snap.Metadata["version"])
		if focus, ok := snap.Metadata["focused_ref"].(string); ok {
			result.Focus = focus
		}
	}
	after := browser.NewSemanticState(snap)
	browser.ApplyStateDiff(&result, before, after)
	frontier := browser.SelectFrontierElements(snap.Elements, result.Focus, 12)
	result.Elements = frontier
	result.Changed = browser.SummarizeElements(frontier, 12)
	if tabs, err := b.ListTabs(ctx); err == nil {
		result.Targets = actionTargets(tabs, b.activeTabID(), 8)
		// Detect if a new tab was opened by this action.
		if beforeTabIDs != nil {
			for _, t := range tabs {
				if !beforeTabIDs[t.ID] && t.ID != result.TabID {
					result.NewTabID = t.ID
					break
				}
			}
		}
	}
	if browser.WantSnapshotFromCtx(ctx) {
		result.Snapshot = &snap
	}
	return result
}

func (b *Bridge) captureSemanticState(ctx context.Context) *browser.SemanticState {
	snap, err := b.Snapshot(ctx, snapshot.SnapshotOptions{ViewportOnly: true})
	if err != nil {
		return nil
	}
	state := browser.NewSemanticState(snap)
	return &state
}

// captureTabIDs returns the set of current tab IDs, used to detect new tabs
// opened by an action. Returns nil on error (detection is best-effort).
func (b *Bridge) captureTabIDs(ctx context.Context) map[string]bool {
	tabs, err := b.ListTabs(ctx)
	if err != nil {
		return nil
	}
	ids := make(map[string]bool, len(tabs))
	for _, t := range tabs {
		ids[t.ID] = true
	}
	return ids
}

func (b *Bridge) evaluate(ctx context.Context, expression, tabID string, dst any) error {
	raw, err := b.cdp(ctx, tabID, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		return err
	}
	var payload struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails any `json:"exceptionDetails,omitempty"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if payload.ExceptionDetails != nil {
		details, _ := json.Marshal(payload.ExceptionDetails)
		return fmt.Errorf("runtime exception: %s", details)
	}
	if len(payload.Result.Value) == 0 {
		// Void/undefined results (e.g. location.reload(), assignments, calls that
		// return nothing) are a successful evaluation, not an error. Surface them
		// as JSON null rather than failing the whole tool call.
		return json.Unmarshal([]byte("null"), dst)
	}
	return json.Unmarshal(payload.Result.Value, dst)
}

func (b *Bridge) cdp(ctx context.Context, tabID, method string, params map[string]any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	req := map[string]any{"method": method, "params": params}
	if strings.TrimSpace(tabID) == "" {
		tabID = b.contextTabID(ctx)
	}
	if tabID != "" {
		req["tabId"] = parseTabID(tabID)
	}
	raw, err := b.call(ctx, "cdp", req)
	if err != nil && tabID != "" && isBridgeTabLostError(err) {
		b.setActiveTabID("")
		delete(req, "tabId")
		return b.call(ctx, "cdp", req)
	}
	if err != nil && tabID != "" && isBridgeDebuggerDetachedError(err) {
		retryRaw, retryErr := b.call(ctx, "cdp", req)
		if retryErr == nil {
			return retryRaw, nil
		}
	}
	return raw, err
}

// contextTabID resolves the tab a page action targets.
//
// Latency profile: when no explicit tab_id is in the context, this makes one
// synchronous get_active_tab_id RPC to the extension per call (every Snapshot,
// Read, Click, etc.). That is a deliberate correctness-over-latency trade: the
// cached b.active reference drifts when the user switches tabs manually in
// Chrome, and acting on the wrong tab is worse than a sub-millisecond local-WS
// round-trip. Callers issuing rapid-fire actions should pass an explicit tab_id
// (which skips the query entirely) or use brw_batch / brw_plan, which
// resolve the tab once for the whole sequence.
func (b *Bridge) contextTabID(ctx context.Context) string {
	if tabID := browser.TabIDFromContext(ctx); tabID != "" {
		return tabID
	}
	if !b.followFocus {
		// Isolation: target the tab brw OWNS. b.active is set only by brw's own
		// Open/FocusTab (never by user-focus signals — those writes are guarded),
		// so this never chases the user's manual tab switches. Returns "" when brw
		// owns no tab yet; the top-level entry (ensureOwnedTabID) opens one then.
		return b.activeTabID()
	}
	// No explicit tab in context: resolve the browser's genuinely focused tab
	// from the extension rather than trusting the cached b.active reference,
	// which drifts when the user switches tabs manually in Chrome (the daemon
	// only updates b.active on explicit FocusTab/ListTabs/Open).
	//
	// Retry briefly before trusting the cache. The MV3 service worker sleeps when
	// Chrome is idle and may be mid-reconnect when a tool call arrives; a single
	// transient resolution failure must NOT silently drop us onto a stale cached
	// tab, because that is exactly how read/observe/snapshot end up resolving three
	// different tabs (one re-resolves live, another serves the stale cache). Three
	// quick attempts ride out a reconnect hiccup; only a genuinely unreachable
	// extension falls through to the last-known tab.
	for attempt := 0; attempt < activeTabResolveAttempts; attempt++ {
		if live := b.resolveActiveTabID(ctx); live != "" {
			return live
		}
		if attempt < activeTabResolveAttempts-1 {
			select {
			case <-ctx.Done():
				return b.activeTabID()
			case <-time.After(activeTabResolveBackoff):
			}
		}
	}
	return b.activeTabID()
}

// ensureOwnedTabID resolves the tab a top-level no-tab_id tool call must target,
// opening one if necessary. An explicit tab_id in ctx always wins (the agent
// chose to work with that existing tab). In isolation (the daemon default) it
// returns the tab brw owns, opening a fresh tab in the default group when brw
// owns none yet — so a worker's first action lands on its own new tab instead of
// the user's focused tab. In follow-focus mode it defers to the live resolver.
func (b *Bridge) ensureOwnedTabID(ctx context.Context) string {
	if tabID := browser.TabIDFromContext(ctx); tabID != "" {
		return tabID
	}
	if b.followFocus {
		return b.contextTabID(ctx)
	}
	if owned := b.activeTabID(); owned != "" {
		return owned
	}
	// No owned tab yet: open one (default group, background) so the call never
	// acts on the user's existing tab. Two guards keep a wedged browser from
	// turning one slow open into a per-call 20s hang (the brw_evaluate cascade):
	//   1. cooldown — after a recent failure, fast-fail instead of re-opening.
	//   2. bounded timeout — a single attempt cannot exceed autoOpenTimeout.
	b.mu.RLock()
	failedAt := b.autoOpenFailedAt
	b.mu.RUnlock()
	if !failedAt.IsZero() && time.Since(failedAt) < autoOpenCooldown {
		return b.activeTabID()
	}

	openCtx, cancel := context.WithTimeout(ctx, autoOpenTimeout)
	defer cancel()
	res, err := b.Open(openCtx, isolationSeedURL)
	if err != nil || res.Tab.ID == "" {
		b.mu.Lock()
		b.autoOpenFailedAt = time.Now()
		b.mu.Unlock()
		log.Printf("brw: isolation auto-open failed (%v); no-tab_id actions fast-fail for %s — reload the brw extension in chrome://extensions, or pass an explicit tab_id", err, autoOpenCooldown)
		return b.activeTabID()
	}
	// Success clears any prior failure so normal isolation resumes immediately.
	b.mu.Lock()
	b.autoOpenFailedAt = time.Time{}
	b.mu.Unlock()
	return res.Tab.ID
}

// ResolveActiveTabID resolves the tab a top-level no-tab_id tool call targets and
// returns it (or "" when it cannot be determined / opened). The MCP / HTTP entry
// points call this when no explicit tab_id is supplied and pin the result into the
// request context via browser.WithTabID, so every downstream contextTabID()
// short-circuits instead of re-resolving per sub-call. In isolation it returns
// (and, on first use, opens) brw's owned tab; in follow-focus mode it runs the
// same bounded retry as contextTabID so a mid-reconnect MV3 service worker does
// not drop the call onto a stale tab.
func (b *Bridge) ResolveActiveTabID(ctx context.Context) string {
	return b.ensureOwnedTabID(ctx)
}

// pinActiveTab resolves the active tab once and returns a context with that tab
// pinned via browser.WithTabID. If an explicit tab is already in the context it
// is left untouched (the caller asked for a specific tab). If resolution fails
// (extension disconnected, mid-reconnect) the original context is returned so
// downstream calls keep their existing per-call resolution behaviour rather than
// pinning an empty tab.
func (b *Bridge) pinActiveTab(ctx context.Context) context.Context {
	if browser.TabIDFromContext(ctx) != "" {
		return ctx
	}
	// ensureOwnedTabID (not contextTabID) so a batch/plan that starts before any
	// brw_open also opens its own tab in isolation instead of resolving the user's.
	if tabID := b.ensureOwnedTabID(ctx); tabID != "" {
		return browser.WithTabID(ctx, tabID)
	}
	return ctx
}

// retargetPinnedTab re-pins the sequence onto the tab a step just moved focus to
// (focus_tab, open). Without it, a focus_tab/open step mid-batch/plan would leave
// subsequent steps pinned to the STALE pre-focus tab. targetTabID is the KNOWN id
// from the just-completed step (focus_tab's target / open's new tab) — never the
// mutable b.active cache, which async active_tab pushes or concurrent operations
// can change out from under the sequence. base is the caller's context: it
// carries a tab ONLY when the caller passed an explicit tab_id (the MCP/HTTP
// entry excludes batch/plan from one-shot active-tab pinning). stepCtx is the
// currently-pinned context, returned unchanged for non-retargeting steps.
func (b *Bridge) retargetPinnedTab(base, stepCtx context.Context, targetTabID string) context.Context {
	// Non-retargeting step (not focus_tab/open, or it failed): keep the pin.
	if targetTabID == "" {
		return stepCtx
	}
	// An explicitly-supplied tab_id stays sticky for the whole sequence (matching
	// the pre-pin behaviour where contextTabID short-circuits on the caller's tab
	// regardless of focus_tab side effects): never let a focus_tab/open override it.
	if browser.TabIDFromContext(base) != "" {
		return stepCtx
	}
	return browser.WithTabID(base, targetTabID)
}

// ensureForegroundTab makes the just-opened tab id the genuine foreground tab —
// the one resolveActiveTabID/get_active_tab_id returns — so that every
// SUBSEQUENT no-tab_id page tool (brw_find / brw_read / brw_click / snapshot)
// targets the tab brw_open just opened rather than whatever tab Chrome left
// focused.
//
// Why this is needed: open_tab creates the tab active WITHIN its window, but the
// daemon's contextTabID deliberately distrusts the cached b.active and
// re-resolves the live foreground tab every call (correct, because the user can
// switch tabs manually in a shared Chrome). Two real-Chrome cases leave a
// DIFFERENT tab foreground right after an open, so b.active and the live
// resolver diverge — exactly the reported "brw_open then brw_find hit an
// unrelated Google Chat tab" bug:
//  1. The new tab lands in a window that is not the OS-focused one, so
//     resolveForegroundTabId returns the focused window's active tab.
//  2. Grouping the new tab into a COLLAPSED group deactivates it (a collapsed
//     group cannot hold the active tab), so Chrome activates an adjacent tab.
//
// We detect the divergence and heal it with an explicit focus_tab (which focuses
// the window, activates the tab, and expands its group), then re-verify. A tab
// that still cannot be made foreground is a HARD error rather than a silent
// fall-through onto a stale tab.
func (b *Bridge) ensureForegroundTab(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("open returned no tab id; refusing to fall back to a stale active tab")
	}
	if live := b.resolveActiveTabID(ctx); live == id {
		b.setActiveTabID(id)
		return nil
	}
	// Diverged: the opened tab is not the live foreground tab. Focus it
	// explicitly (window focus + activate + group expand) and re-verify.
	if err := b.FocusTab(ctx, id); err != nil {
		return fmt.Errorf("open: could not focus the opened tab %s: %w", id, err)
	}
	if live := b.resolveActiveTabID(ctx); live != id {
		return fmt.Errorf("open: opened tab %s did not become the active tab (resolver reported %q); the open may have been blocked or the tab was immediately replaced", id, live)
	}
	b.setActiveTabID(id)
	return nil
}

// resolveActiveTabID asks the extension for the truly active/focused tab and
// updates the cached reference to match, healing drift. Returns "" when the
// bridge is disconnected or the query fails so the caller can fall back to the
// last-known cached value.
func (b *Bridge) resolveActiveTabID(ctx context.Context) string {
	b.mu.RLock()
	connected := b.conn != nil
	b.mu.RUnlock()
	if !connected {
		return ""
	}
	raw, err := b.call(ctx, "get_active_tab_id", nil)
	if err != nil {
		return ""
	}
	var resp struct {
		TabID int `json:"tabId"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.TabID == 0 {
		return ""
	}
	id := strconv.Itoa(resp.TabID)
	// This is the user's live-focused tab. Only heal the cache toward it in
	// follow-focus mode; in isolation the cache is brw's owned tab and must not be
	// repointed at the user's tab. (In isolation this resolver isn't used for
	// targeting, but guard the write so no path can clobber the owned id.)
	if id != b.activeTabID() && b.followFocus {
		b.setActiveTabID(id)
	}
	return id
}

func isBridgeTabLostError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "no tab")
}

func isBridgeDebuggerDetachedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "detached while handling command") ||
		strings.Contains(msg, "debugger is not attached") ||
		strings.Contains(msg, "target closed")
}

func (b *Bridge) axNodes(ctx context.Context, tabID string) ([]*accessibility.Node, error) {
	raw, err := b.cdp(ctx, tabID, "Accessibility.getFullAXTree", nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Nodes []*accessibility.Node `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	return payload.Nodes, nil
}

func (b *Bridge) resolveBox(ctx context.Context, ref string) (snapshot.ElementBox, error) {
	refJSON, _ := json.Marshal(ref)
	var box snapshot.RecoveredBox
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.ResolveOrRecoverBoxScript, refJSON), "", &box); err != nil {
		return snapshot.ElementBox{}, err
	}
	if !box.OK {
		reason := box.Reason
		if reason == "" {
			reason = "not_visible"
		}
		return snapshot.ElementBox{}, fmt.Errorf("element ref %q not recoverable: %s", ref, reason)
	}
	return box.ElementBox, nil
}

func (b *Bridge) focus(ctx context.Context, ref string) error {
	refJSON, _ := json.Marshal(ref)
	var ok bool
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.FocusElementScript, refJSON), "", &ok); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("element ref %q not found or could not be focused", ref)
	}
	return nil
}

func (b *Bridge) condition(ctx context.Context, condition string) (bool, error) {
	condition = strings.TrimSpace(condition)
	if condition == "" || condition == "load" || condition == "page_ready" {
		condition = "ready"
	}
	condJSON, _ := json.Marshal(condition)
	expr := fmt.Sprintf(`(function(condition) {
	  function roots() {
	    const out = [document];
	    for (let i = 0; i < out.length; i++) {
	      const root = out[i];
	      if (!root.querySelectorAll) continue;
	      for (const el of Array.from(root.querySelectorAll('*'))) {
	        if (el.shadowRoot) out.push(el.shadowRoot);
	      }
	    }
	    return out;
	  }
	  function hasRef(ref) {
	    const selector = '[data-brw-ref="' + CSS.escape(ref) + '"]';
	    return roots().some(root => root.querySelector && root.querySelector(selector));
	  }
	  if (condition === "ready" || condition === "load") return document.readyState === "complete" || document.readyState === "interactive";
	  // "committed" contract: BOTH the document must be interactive/complete AND the
	  // URL must be a real navigation target — not the transient "about:blank" nor the
	  // empty href that can appear during very early frame init. The && short-circuits,
	  // so order is immaterial: an empty/blank href fails the condition regardless of
	  // readyState.
	  if (condition === "committed") return (document.readyState === "complete" || document.readyState === "interactive") && location.href !== "about:blank" && location.href !== "";
	  if (condition.startsWith("url:")) return location.href.includes(condition.slice(4));
	  if (condition.startsWith("not_url:")) return !location.href.includes(condition.slice(8));
	  if (condition.startsWith("title:")) return document.title.includes(condition.slice(6));
	  if (condition.startsWith("not_title:")) return !document.title.includes(condition.slice(10));
	  if (condition.startsWith("text:")) return document.body && document.body.innerText.includes(condition.slice(5));
	  if (condition.startsWith("not_text:")) return !document.body || !document.body.innerText.includes(condition.slice(9));
	  if (condition.startsWith("ref:")) return hasRef(condition.slice(4));
	  if (condition.startsWith("not_ref:")) return !hasRef(condition.slice(8));
	  return document.body && document.body.innerText.includes(condition);
	})(%s)`, condJSON)
	var ok bool
	err := b.evaluate(ctx, expr, "", &ok)
	return ok, err
}

type extTab struct {
	ID             int    `json:"id"`
	URL            string `json:"url"`
	Title          string `json:"title"`
	Active         bool   `json:"active"`
	Highlighted    bool   `json:"highlighted"`
	WindowID       int    `json:"windowId"`
	WindowFocused  bool   `json:"windowFocused"`
	WindowType     string `json:"windowType"`
	GroupID        *int   `json:"groupId"`
	GroupTitle     string `json:"groupTitle"`
	GroupColor     string `json:"groupColor"`
	GroupCollapsed bool   `json:"groupCollapsed"`
	OpenerTabID    int    `json:"openerTabId"`
}

func (t extTab) toBrowserTab() browser.Tab {
	windowType := strings.TrimSpace(t.WindowType)
	tabType := "page"
	if windowType == "popup" {
		tabType = "popup"
	}
	openerID := ""
	if t.OpenerTabID != 0 {
		openerID = strconv.Itoa(t.OpenerTabID)
	}
	return browser.Tab{
		ID:             strconv.Itoa(t.ID),
		URL:            t.URL,
		Title:          t.Title,
		Type:           tabType,
		WindowID:       t.WindowID,
		WindowType:     windowType,
		GroupID:        groupIDString(t.GroupID),
		GroupTitle:     t.GroupTitle,
		GroupColor:     t.GroupColor,
		GroupCollapsed: t.GroupCollapsed,
		Active:         t.Active,
		Highlighted:    t.Highlighted,
		WindowFocused:  t.WindowFocused,
		OpenerTabID:    openerID,
		Popup:          windowType == "popup" || t.OpenerTabID != 0,
	}
}

type extTabGroup struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Color     string `json:"color"`
	Collapsed bool   `json:"collapsed"`
	WindowID  int    `json:"windowId"`
	TabIDs    []int  `json:"tabIds"`
	TabCount  int    `json:"tabCount"`
}

func (g extTabGroup) toBrowserTabGroup() browser.TabGroup {
	tabIDs := make([]string, 0, len(g.TabIDs))
	for _, id := range g.TabIDs {
		tabIDs = append(tabIDs, strconv.Itoa(id))
	}
	return browser.TabGroup{
		ID:        strconv.Itoa(g.ID),
		Title:     g.Title,
		Color:     g.Color,
		Collapsed: g.Collapsed,
		WindowID:  g.WindowID,
		TabIDs:    tabIDs,
		TabCount:  g.TabCount,
	}
}

func groupIDString(id *int) string {
	if id == nil || *id < 0 {
		return ""
	}
	return strconv.Itoa(*id)
}

func (b *Bridge) activeTabID() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.active
}

func (b *Bridge) setActiveTabID(id string) {
	b.mu.Lock()
	b.active = strings.TrimSpace(id)
	b.mu.Unlock()
}

func actionTargets(tabs []browser.Tab, activeID string, limit int) []browser.Tab {
	if limit <= 0 || len(tabs) == 0 {
		return nil
	}
	out := make([]browser.Tab, 0, min(limit, len(tabs)))
	seen := map[string]bool{}
	add := func(tab browser.Tab) {
		if tab.ID == "" || seen[tab.ID] || len(out) >= limit {
			return
		}
		seen[tab.ID] = true
		out = append(out, tab)
	}
	for _, tab := range tabs {
		if tab.ID == activeID {
			add(tab)
		}
	}
	for _, tab := range tabs {
		if tab.Popup || tab.WindowType == "popup" {
			add(tab)
		}
	}
	for _, tab := range tabs {
		if tab.Active && tab.WindowFocused {
			add(tab)
		}
	}
	for _, tab := range tabs {
		if tab.Active {
			add(tab)
		}
	}
	return out
}

func parseTabID(id string) int {
	n, _ := strconv.Atoi(id)
	return n
}

// requireTabID validates a caller-supplied tab id for operations that target a
// specific tab (focus/close). An empty or non-numeric id used to be silently
// coerced to 0 by parseTabID, which the extension rejected with the opaque "No
// tab with id: 0" — surfacing here as a clear, actionable error instead so a
// batched script fails loudly at the offending step.
func requireTabID(id string) (int, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return 0, errors.New("tab id is required")
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid tab id %q", id)
	}
	return n, nil
}

func requireGroupID(id string) (int, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return 0, errors.New("group id is required")
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid group id %q", id)
	}
	return n, nil
}

func screenshotFromRaw(raw json.RawMessage) (browser.Screenshot, error) {
	return screenshotFromRawMIME(raw, "image/png")
}

func screenshotFromRawMIME(raw json.RawMessage, mimeType string) (browser.Screenshot, error) {
	var payload struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return browser.Screenshot{}, err
	}
	if payload.Data == "" {
		return browser.Screenshot{}, errors.New("screenshot returned no data")
	}
	data, err := base64.StdEncoding.DecodeString(payload.Data)
	if err != nil {
		return browser.Screenshot{}, err
	}
	return browser.Screenshot{MIMEType: mimeType, Data: data, Base64: payload.Data}, nil
}

func (b *Bridge) ExecuteBatch(ctx context.Context, steps []browser.BatchStep) (browser.BatchResult, error) {
	// Keep the caller context for the final observation so a cancelled run can
	// still report current page state; step execution uses the cancel-aware ctx.
	obsCtx := ctx
	entry, release := b.cancels.register(ctx, cancelToken(ctx, ""))
	defer release()
	ctx = entry.ctx

	// Resolve the active tab ONCE for the whole sequence and pin it into the
	// step context, so each step's contextTabID() short-circuits instead of
	// re-issuing get_active_tab_id per step (3-11x per step otherwise). A
	// focus_tab / open step legitimately moves the active tab, so we re-pin after
	// any retargeting step (see retargetPinnedTab).
	stepCtx := b.pinActiveTab(ctx)

	result := browser.BatchResult{OK: true, Steps: make([]browser.BatchStepResult, 0, len(steps)), TabID: browser.TabIDFromContext(stepCtx)}
	for i, step := range steps {
		if entry.Cancelled() {
			result.Cancelled = true
			result.OK = false
			result.Error = "cancelled"
			break
		}
		sr, retargetTo := b.executeBatchStep(stepCtx, i, step)
		stepCtx = b.retargetPinnedTab(ctx, stepCtx, retargetTo)
		result.Steps = append(result.Steps, sr)
		if !sr.OK {
			if entry.Cancelled() {
				result.Cancelled = true
				result.OK = false
				result.Error = "cancelled"
				result.Steps = result.Steps[:len(result.Steps)-1]
				break
			}
			result.OK = false
			result.Error = sr.Error
			break
		}
	}
	result.StepsCompleted = len(result.Steps)
	// Pin the observation to the tab the sequence ended on (the step pin, which
	// already tracked focus_tab/open moves) so the closing snapshot resolves the
	// active tab once via the pinned id instead of re-issuing get_active_tab_id
	// across its tryCached/store/evaluate sub-calls. Falls back to obsCtx when no
	// tab could be pinned, preserving the prior per-call behaviour.
	if pinned := browser.TabIDFromContext(stepCtx); pinned != "" {
		obsCtx = browser.WithTabID(obsCtx, pinned)
		// Refresh the reported tab id to the tab the sequence actually ended on
		// (focus_tab/open retargets), so a batch that moved focus does not return
		// the initial tab_id alongside the new tab's URL/title.
		result.TabID = pinned
	} else {
		obsCtx = b.pinActiveTab(obsCtx)
		if p := browser.TabIDFromContext(obsCtx); p != "" {
			result.TabID = p
		}
	}
	snap, snapErr := b.Snapshot(obsCtx, snapshot.SnapshotOptions{ViewportOnly: true})
	if snapErr == nil {
		result.URL = snap.URL
		result.Title = snap.Title
		if snap.Metadata != nil {
			if v, ok := snap.Metadata["version"].(float64); ok {
				result.Version = int64(v)
			}
			if focus, ok := snap.Metadata["focused_ref"].(string); ok {
				result.Focus = focus
			}
		}
	}
	return result, nil
}

// executeBatchStep runs one batch step and returns its result plus the retarget
// target tab id — the KNOWN id of the tab a successful focus_tab/open moved focus
// to ("" for any other step or on failure), used by the loop to re-pin without
// reading the mutable active-tab cache.
func (b *Bridge) executeBatchStep(ctx context.Context, index int, step browser.BatchStep) (browser.BatchStepResult, string) {
	sr := browser.BatchStepResult{Index: index, Action: step.Action, OK: true}
	var actionErr error
	retargetTo := ""
	switch step.Action {
	case "click":
		if step.Ref == "" {
			actionErr = errors.New("click requires ref")
			break
		}
		actionErr = b.clickRef(ctx, step.Ref)
		b.settle(ctx, batchActionSettle)
	case "type":
		if step.Ref == "" || step.Text == "" {
			actionErr = errors.New("type requires ref and text")
			break
		}
		actionErr = b.typeRef(ctx, step.Ref, step.Text)
		b.settle(ctx, batchActionSettle)
	case "fill":
		_, actionErr = b.fillOptions(ctx, snapshot.FillOptions{Ref: step.Ref, Text: step.Text, Replace: true})
		b.settle(ctx, batchActionSettle)
	case "select":
		if step.Ref == "" || step.Value == "" {
			actionErr = errors.New("select requires ref and value")
			break
		}
		_, actionErr = b.selectValue(ctx, step.Ref, step.Value)
		b.settle(ctx, batchActionSettle)
	case "press":
		if step.Key == "" {
			actionErr = errors.New("press requires key")
			break
		}
		actionErr = b.pressKey(ctx, step.Key)
		b.settle(ctx, batchActionSettle)
	case "scroll":
		_, actionErr = b.scrollDirection(ctx, step.Direction)
		b.settle(ctx, batchActionSettle)
	case "hover":
		if step.Ref == "" {
			actionErr = errors.New("hover requires ref")
			break
		}
		actionErr = b.hoverRef(ctx, step.Ref)
		b.settle(ctx, batchActionSettle)
	case "wait":
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 10 * time.Second
		}
		actionErr = b.WaitFor(ctx, step.Condition, timeout)
	case "open":
		if step.URL == "" {
			actionErr = errors.New("open requires url")
			break
		}
		var openRes browser.OpenResult
		openRes, actionErr = b.Open(ctx, step.URL)
		if actionErr == nil {
			retargetTo = openRes.Tab.ID
		}
	case "focus_tab":
		if step.ID == "" {
			actionErr = errors.New("focus_tab requires id")
			break
		}
		actionErr = b.FocusTab(ctx, step.ID)
		if actionErr == nil {
			retargetTo = step.ID
		}
	case "assert_visible":
		if step.Ref == "" {
			actionErr = errors.New("assert_visible requires ref")
			break
		}
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		actionErr = b.AssertVisible(ctx, step.Ref, timeout)
	case "assert_text":
		if step.Ref == "" || step.Text == "" {
			actionErr = errors.New("assert_text requires ref and text")
			break
		}
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		actionErr = b.AssertText(ctx, step.Ref, step.Text, timeout)
	case "assert_value":
		if step.Ref == "" || step.Value == "" {
			actionErr = errors.New("assert_value requires ref and value")
			break
		}
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		actionErr = b.AssertValue(ctx, step.Ref, step.Value, timeout)
	case "assert_hidden":
		if step.Ref == "" {
			actionErr = errors.New("assert_hidden requires ref")
			break
		}
		timeout := time.Duration(step.TimeoutMS) * time.Millisecond
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		actionErr = b.AssertHidden(ctx, step.Ref, timeout)
	default:
		actionErr = fmt.Errorf("unknown action %q", step.Action)
	}
	if actionErr != nil {
		sr.OK = false
		sr.Error = actionErr.Error()
	}
	return sr, retargetTo
}

func (b *Bridge) Observe(ctx context.Context) (browser.ObserveResult, error) {
	snap, err := b.Snapshot(ctx, snapshot.SnapshotOptions{ViewportOnly: true})
	if err != nil {
		return browser.ObserveResult{}, err
	}
	focus := ""
	if snap.Metadata != nil {
		if f, ok := snap.Metadata["focused_ref"].(string); ok {
			focus = f
		}
	}
	changed := make([]string, 0)
	for _, el := range snap.Elements {
		if el.Visible {
			summary := el.Role + " " + el.Ref + " " + el.Name
			if el.Value != "" {
				summary += " value:" + el.Value
			}
			changed = append(changed, summary)
		}
	}
	if len(changed) > 12 {
		changed = changed[:12]
	}
	return browser.ObserveResult{
		Version: 1,
		URL:     snap.URL,
		Title:   snap.Title,
		Focus:   focus,
		Changed: changed,
	}, nil
}

func (b *Bridge) AssertVisible(ctx context.Context, ref string, timeout time.Duration) error {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	return b.evalAssert(ctx, snapshot.AssertVisibleScript, ref, timeout.Milliseconds())
}

func (b *Bridge) AssertText(ctx context.Context, ref, text string, timeout time.Duration) error {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	return b.evalAssert(ctx, snapshot.AssertTextScript, ref, text, timeout.Milliseconds())
}

func (b *Bridge) AssertValue(ctx context.Context, ref, value string, timeout time.Duration) error {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	return b.evalAssert(ctx, snapshot.AssertValueScript, ref, value, timeout.Milliseconds())
}

func (b *Bridge) AssertHidden(ctx context.Context, ref string, timeout time.Duration) error {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	return b.evalAssert(ctx, snapshot.AssertHiddenScript, ref, timeout.Milliseconds())
}

func (b *Bridge) evalAssert(ctx context.Context, script string, args ...any) error {
	marshaled := make([]string, len(args))
	for i, arg := range args {
		value, _ := json.Marshal(arg)
		marshaled[i] = string(value)
	}
	var ok bool
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", script, strings.Join(marshaled, ",")), "", &ok); err != nil {
		return err
	}
	if !ok {
		return errors.New("assertion did not pass within timeout")
	}
	return nil
}

func (b *Bridge) CommitField(ctx context.Context, ref string) error {
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	refJSON, _ := json.Marshal(ref)
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s)", snapshot.CommitFieldScript, refJSON), "", &result); err != nil {
		return err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "commit failed"
		}
		return fmt.Errorf("commit: %s", result.Error)
	}
	return nil
}

// Notify raises a desktop notification at a human hand-off point by sending a
// "notify" command over the bridge. The extension turns it into a
// chrome.notifications.create call, which surfaces even when the agent tab is
// backgrounded. The result reports the honest delivery channel.
func (b *Bridge) Notify(ctx context.Context, opts browser.NotifyOptions) (browser.NotifyResult, error) {
	opts, err := browser.NormalizeNotifyOptions(opts)
	if err != nil {
		return browser.NotifyResult{}, err
	}
	raw, err := b.call(ctx, "notify", map[string]any{
		"kind":    opts.Kind,
		"title":   opts.Title,
		"message": opts.Message,
	})
	if err != nil {
		return browser.NotifyResult{}, err
	}
	var result browser.NotifyResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return browser.NotifyResult{}, err
	}
	if result.Delivery == "" {
		result.Delivery = "extension"
	}
	return result, nil
}

func (b *Bridge) ConsoleMessages(ctx context.Context) ([]browser.ConsoleMessage, error) {
	expr := `(function() {
		if (!window.__brwConsole) return [];
		var msgs = window.__brwConsole.slice();
		window.__brwConsole.length = 0;
		return msgs;
	})()`
	raw, err := b.call(ctx, "cdp", map[string]any{"method": "Runtime.evaluate", "params": map[string]any{"expression": expr, "returnByValue": true}})
	if err != nil {
		return nil, err
	}
	var evalResult struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &evalResult); err != nil {
		return nil, err
	}
	var msgs []browser.ConsoleMessage
	if len(evalResult.Result.Value) > 0 {
		if err := json.Unmarshal(evalResult.Result.Value, &msgs); err != nil {
			return nil, fmt.Errorf("parse console messages: %w", err)
		}
	}
	return msgs, nil
}

func (b *Bridge) WindowBounds(ctx context.Context) (snapshot.WindowBoundsResult, error) {
	var result snapshot.WindowBoundsResult
	if err := b.evaluate(ctx, snapshot.WindowBoundsScript, "", &result); err != nil {
		return snapshot.WindowBoundsResult{}, err
	}
	return result, nil
}

func (b *Bridge) ClickXY(ctx context.Context, x, y float64) (snapshot.ClickXYResult, error) {
	var result snapshot.ClickXYResult
	xJSON, _ := json.Marshal(x)
	yJSON, _ := json.Marshal(y)
	if err := b.evaluate(ctx, fmt.Sprintf("%s(%s,%s)", snapshot.ClickXYScript, xJSON, yJSON), "", &result); err != nil {
		return snapshot.ClickXYResult{}, err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "click failed"
		}
		return result, fmt.Errorf("click xy: %s", result.Error)
	}
	return result, nil
}

// downloadsUnsupportedNote is returned when the connected extension is too old to
// answer get_downloads (pre-issue-#6 builds) so callers still get the graceful
// Supported=false contract instead of a hard error.
const downloadsUnsupportedNote = "Download capture is unavailable: the connected brw extension predates chrome.downloads support (issue #6). Reload the brw extension, or restart brw with the direct-CDP backend. Check Supported=false to detect this programmatically."

// Downloads captures downloads over the extension bridge via the extension's
// chrome.downloads API (issue #6). The bridge cannot observe CDP
// Browser.downloadWillBegin / downloadProgress events — those only fire on a
// debugger-attached target — so the extension's service worker registers
// chrome.downloads.onCreated / onChanged listeners and buffers entries, which we
// drain here through the get_downloads RPC. The wire shape already matches
// browser.DownloadEntry, so it unmarshals directly. An extension too old to know
// the message returns the legacy Supported=false note rather than erroring.
func (b *Bridge) Downloads(ctx context.Context) (browser.DownloadsResult, error) {
	raw, err := b.call(ctx, "get_downloads", nil)
	if err != nil {
		if isUnknownMessageTypeErr(err) {
			return browser.DownloadsResult{
				Downloads: []browser.DownloadEntry{},
				Count:     0,
				Supported: false,
				Note:      downloadsUnsupportedNote,
			}, nil
		}
		return browser.DownloadsResult{}, err
	}
	var payload struct {
		Downloads []browser.DownloadEntry `json:"downloads"`
		Supported bool                    `json:"supported"`
		Note      string                  `json:"note"`
	}
	if len(raw) > 0 {
		if jsonErr := json.Unmarshal(raw, &payload); jsonErr != nil {
			return browser.DownloadsResult{}, fmt.Errorf("parse downloads: %w", jsonErr)
		}
	}
	if payload.Downloads == nil {
		payload.Downloads = []browser.DownloadEntry{}
	}
	return browser.DownloadsResult{
		Downloads: payload.Downloads,
		Count:     len(payload.Downloads),
		Supported: payload.Supported,
		Note:      payload.Note,
	}, nil
}

func (b *Bridge) GetTrace() browser.TraceResult { return browser.TraceResult{} }
func (b *Bridge) ClearTrace()                   {}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
