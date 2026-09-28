package extensionbridge

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/navpolicy"
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/siteconsent"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/Don-Works/brw/internal/usagelog"
	"github.com/coder/websocket"
)

const (
	// The per-frame ceiling stays deliberately small so one WebSocket message
	// cannot make the daemon allocate an unbounded buffer. The extension splits
	// larger logical responses below this limit and the receiver independently
	// caps the aggregate transfer. The 64 MiB logical cap is intentionally below
	// the artifact store's 128 MiB raw-byte default: base64-backed extension
	// captures support roughly 48 MiB of binary data without multi-hundred-MiB
	// transient allocations. Direct transports may support the store's full cap.
	extensionFrameReadLimitBytes     = 4 << 20
	maxChunkedResponseBytes          = 64 << 20
	maxChunkedResponseChunks         = 128
	maxBufferedChunkedResponsesBytes = 128 << 20
)

type Bridge struct {
	addr               string
	timeout            time.Duration
	allowedExtensionID string
	identity           brwidentity.Identity
	navPolicy          *navpolicy.Policy
	// consent backs the /consent surface the extension's options page reads and
	// revokes through. Nil means the daemon was started without site consent.
	consent *siteconsent.Guard
	// containment records which tabs already have subresource containment
	// installed, so arming costs one message per tab rather than one per action.
	containment tabArm
	// webmcp, when true, arms the WebMCP shim (snapshot.WebMCPInstallScript) on
	// every tab brw opens or drives; webmcpArm records which tabs have it.
	webmcp    bool
	webmcpArm tabArm
	// routes mirrors the declarativeNetRequest session rules the extension holds
	// per tab, so brw_route can list and rebuild them without asking Chrome.
	routes bridgeRouteTable
	usage  *usagelog.Recorder
	server *http.Server

	// authToken, when non-empty, is a per-launch shared secret the extension may
	// present in its hello. The daemon serves it over the loopback /status
	// endpoint (which a browser web page cannot read cross-origin), so the real
	// 0.2.0+ extension can prove itself. A WRONG token is always rejected; a
	// MISSING token is rejected unless requireToken is cleared (graceful: upgrading
	// the daemon never bricks an already-installed pre-0.2.0 extension). Empty
	// disables the check entirely (library/test/embedder use); the empty-Origin
	// rejection still applies in all cases.
	authToken string
	// requireToken, when true (the default), rejects a hello that carries no token (strict
	// mode). Default false keeps the bridge backward-compatible with an extension
	// that has not yet been reloaded to 0.2.0.
	requireToken bool
	// compatWarnOnce logs the "no token, accepting for compatibility" notice at
	// most once per daemon lifetime instead of on every MV3 reconnect.
	compatWarnOnce sync.Once

	mu   sync.RWMutex
	conn *websocket.Conn
	// shuttingDown is set before Shutdown detaches the active socket. It keeps
	// reconnect waiters and handlers that raced the HTTP shutdown from
	// registering new work after the pending/chunk maps have been drained.
	shuttingDown bool
	acceptLog    acceptLogLimiter
	hello        hello
	active       string
	// agentPinKnown is true only after the CURRENT connection has
	// authoritatively reported its extension-owned tab pin. It prevents a
	// no-tab_id isolation request that starts in an MV3 reconnect gap from
	// returning b.active before the replacement worker has reconciled a lost
	// tabs.onRemoved notification. Guarded by mu.
	agentPinKnown bool
	pending       map[string]chan response
	// responseChunks holds partial logical responses split across bounded
	// WebSocket frames. Guarded by mu and charged by responseChunkBytes so several
	// concurrent callers cannot multiply the per-response cap without bound.
	responseChunks     map[string]*responseChunkAssembly
	responseChunkBytes int
	writeMu            sync.Mutex
	nextID             atomic.Uint64

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
	// Different tabs still run in parallel up to the sema cap. Each keyed entry
	// is reference-counted across holders and waiters, then removed after its last
	// user; this bounds a long-running daemon without ever replacing a lock while
	// an old waiter still references it.
	tabLocksMu sync.Mutex
	tabLocks   map[string]*tabLockEntry

	// Backpressure / resilience counters, surfaced over /status for operators.
	inflight  atomic.Int64  // RPCs currently on the wire
	queued    atomic.Int64  // callers blocked waiting for an in-flight slot
	busyDrops atomic.Uint64 // calls rejected with ErrBridgeBusy (cap saturated)
	retries   atomic.Uint64 // idempotent calls retried after a transient drop

	connectedAt      time.Time
	lastSeenAt       time.Time
	disconnectedAt   time.Time
	disconnectReason string
	// lastHandshake is the endpoint config the most recent REFUSED handshake
	// reported. A refused connection never becomes b.hello, so without this the
	// only record of which status URL the extension tried is the extension's own
	// storage — which nothing outside the browser can read. Guarded by mu.
	lastHandshake handshakeReport

	// cancels tracks in-flight long-running operations (plan / batch / wait
	// loops) keyed by an operation token so Cancel can stop a specific run
	// cooperatively. Mirrors the browser.Manager mechanism so cancellation
	// behaves identically across the CDP and extension transports.
	cancels *cancelRegistry

	// observeState is deliberately separate from action observations: brw_observe
	// promises changes since the previous brw_observe call, not since an action's
	// internal post-condition snapshot.
	observeMu       sync.Mutex
	observedState   map[string]*browser.SemanticState
	observeVersions map[string]int64

	traceMu sync.Mutex
	trace   []browser.TraceEntry

	// The extension returns a retained bounded download snapshot. Track changes
	// locally so deterministic recipe calls get a per-tab delta after their
	// pre-arm baseline, while ordinary brw_downloads calls remain full and
	// non-draining.
	downloadsMu          sync.Mutex
	downloadFingerprints map[string]string
	downloadVersions     map[string]uint64
	downloadCursors      map[string]uint64
	downloadSequence     uint64

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
	// recentReplaces timestamps recent extension-connection replacements (guarded
	// by b.mu). Two browser profiles running brw against ONE bridge endlessly
	// displace each other ("replaced by new extension connection" churn = the
	// flashing extension icon); a burst here trips flapHoldUntil.
	recentReplaces []time.Time
	// flapHoldUntil, while in the future, makes handleExtension REJECT a new
	// extension connection instead of replacing the live one, breaking the flap so
	// the current connection stays stable. Extended on each rejected intruder, and
	// naturally bypassed once the live connection actually dies (b.conn == nil).
	flapHoldUntil time.Time
}

// SetNavigationPolicy installs controller-level navigation checks. Call before
// serving requests.
func (b *Bridge) SetNavigationPolicy(p *navpolicy.Policy) { b.navPolicy = p }

// SetUsageRecorder installs the privacy-safe lifecycle ledger. Connection
// reasons are fingerprinted rather than stored verbatim.
func (b *Bridge) SetUsageRecorder(recorder *usagelog.Recorder) { b.usage = recorder }

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

const (
	// flapWindow + flapThreshold detect extension-connection churn: this many
	// replacements within the window means two extensions are colliding on one
	// bridge (a flashing icon), not a normal single reconnect.
	flapWindow    = 10 * time.Second
	flapThreshold = 5
	// flapHoldDuration is how long, after a detected flap, the bridge holds the
	// current connection and rejects intruders. Extended on each rejected attempt
	// so a persistently colliding profile can't resume the churn.
	flapHoldDuration = 30 * time.Second
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
	Source  string `json:"source,omitempty"`
	Version string `json:"version,omitempty"`
	// Build is the manifest version of the LOADED extension code (as opposed
	// to Version, the wire-protocol version). Surfaced in /status and the
	// connect log line so an operator can tell whether an unpacked-extension
	// reload actually picked up the current on-disk build.
	Build     string `json:"build,omitempty"`
	Chrome    string `json:"chrome,omitempty"`
	Platform  string `json:"platform,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Profile   string `json:"profile,omitempty"`
	Label     string `json:"label,omitempty"`
	// AgentTabID is the extension-owned working-tab pin, not Chrome's foreground
	// tab. A pointer distinguishes a current extension explicitly reporting
	// "none" (0) from an older extension that omits the additive field. Both are
	// accepted on the wire; omission fails closed by clearing cached ownership.
	AgentTabID *int `json:"agent_tab_id,omitempty"`
	// StatusURL and BridgeURL are the endpoints the extension is ACTUALLY using,
	// and ConfigSource ("stored", "packaged" or "built-in") says which layer of
	// its config supplied them. The extension's chrome.storage.local config
	// silently overrides the packaged bridge-defaults.json, so nothing on disk
	// tells `brwctl doctor` which endpoint is live; only the extension can say.
	// Omitted by an extension older than 0.6.0, so an empty value means
	// "unreported", never "none".
	StatusURL    string `json:"status_url,omitempty"`
	BridgeURL    string `json:"bridge_url,omitempty"`
	ConfigSource string `json:"config_source,omitempty"`
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
	ID         string          `json:"id,omitempty"`
	Type       string          `json:"type,omitempty"`
	TabID      int             `json:"tabId,omitempty"`
	OK         bool            `json:"ok,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	Hello      hello           `json:"hello,omitempty"`
	Encoding   string          `json:"encoding,omitempty"`
	ChunkIndex *int            `json:"chunk_index,omitempty"`
	ChunkCount *int            `json:"chunk_count,omitempty"`
	TotalBytes *int            `json:"total_bytes,omitempty"`
	ChunkData  string          `json:"data,omitempty"`
}

type responseChunkAssembly struct {
	conn       *websocket.Conn
	nextIndex  int
	chunkCount int
	totalBytes int
	received   int
	parts      [][]byte
}

type tabLockEntry struct {
	ch   chan struct{}
	refs int
}

func New(addr string, timeout time.Duration, allowedExtensionID string) *Bridge {
	return NewWithIdentity(addr, timeout, allowedExtensionID, brwidentity.Identity{})
}

func NewWithIdentity(addr string, timeout time.Duration, allowedExtensionID string, identity brwidentity.Identity) *Bridge {
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	b := &Bridge{
		addr:                 addr,
		timeout:              timeout,
		allowedExtensionID:   strings.TrimSpace(allowedExtensionID),
		identity:             identity,
		pending:              map[string]chan response{},
		responseChunks:       map[string]*responseChunkAssembly{},
		cancels:              newCancelRegistry(),
		observedState:        map[string]*browser.SemanticState{},
		observeVersions:      map[string]int64{},
		trace:                make([]browser.TraceEntry, 0, 256),
		downloadFingerprints: map[string]string{},
		downloadVersions:     map[string]uint64{},
		downloadCursors:      map[string]uint64{},
		emulationStates:      map[string]bridgeDeviceEmulationState{},
		connReady:            make(chan struct{}),
		tabLocks:             map[string]*tabLockEntry{},
		maxInflight:          defaultBridgeMaxInflight,
		requireToken:         true,
		sema:                 make(chan struct{}, defaultBridgeMaxInflight),
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
	// The options page is the only surface a user of the signed-in browser has,
	// so the grant list and its revocations are served here beside /status
	// rather than only on the control-plane API, which the extension's page
	// cannot reach across the control plane's cross-origin guard.
	mux.HandleFunc("/consent", b.handleConsent)
	mux.HandleFunc("/consent/revoke", b.handleConsentRevoke)
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

// SetRequireToken sets strict mode (the daemon's default), where a hello with no
// token is rejected rather than accepted for backward-compatibility. Call once
// before ListenAndServe. Default (false) keeps a not-yet-reloaded extension
// working.
func (b *Bridge) SetRequireToken(v bool) { b.requireToken = v }

// RequireToken reports whether a tokenless hello is refused.
func (b *Bridge) RequireToken() bool { return b.requireToken }

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
// worker to reconnect after finding the socket down, before failing. Observed
// worker respawns after a StatusGoingAway close take 3-11s, so a 3s grace failed
// even the typical case with "extension bridge is not connected" while the
// worker was on its way back. 12s rides out the reconnect for essentially every
// routine going-away event without hanging a caller indefinitely — the overall
// op deadline (b.timeout, 20s default) still applies on top, and idempotent
// reads additionally retry once after a transient drop.
const bridgeReconnectGrace = 12 * time.Second

// replacedDrainReason is the error stamped on pending RPCs when a NEW extension
// with a DIFFERENT identity takes over the bridge: the displaced extension can
// never answer them, so they fail immediately instead of hanging for the full
// call timeout. Deliberately NOT retryable-transparent — a retry would be
// answered by the other browser.
const replacedDrainReason = "extension connection replaced by a different extension"

// disconnectDrainReason is the error releaseConn stamps on pending RPCs when the
// socket drops. It is recognised as a transient transport failure so an
// idempotent read can be retried after the worker reconnects.
const disconnectDrainReason = "extension disconnected"

// shutdownDrainReason is deliberately distinct from disconnectDrainReason:
// shutdown is terminal, so idempotent calls must not enter the MV3 reconnect
// retry path after their pending response has been drained.
const shutdownDrainReason = "extension bridge shutting down"

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

var errBridgeShuttingDown = errors.New(shutdownDrainReason)

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
	"list_tabs":             true,
	"list_tab_groups":       true,
	"get_active_tab_id":     true,
	"get_document_identity": true,
	"cached_snapshot":       true,
	"get_downloads":         true,
}

func isIdempotentType(typ string) bool { return idempotentBridgeTypes[typ] }

func isTransientTransportErr(err error) bool {
	return errors.Is(err, errBridgeTransport) || errors.Is(err, errBridgeNotConnected)
}

// isUnknownMessageTypeErr reports whether err is the extension's "unknown message
// type" rejection, surfaced when the connected extension predates a bridge RPC
// (e.g. an old build that lacks get_downloads). Callers use it to degrade
// gracefully to an unsupported result rather than erroring.
// unknownMessageType is the error the extension's service worker returns for a
// message type it does not implement, which is how the bridge tells an OLD
// extension apart from a real failure and falls back rather than erroring.
//
// It is the extension's text, produced in JavaScript
// (extension/service_worker.js), so Go cannot share the literal with it. What it
// can do is keep exactly one Go copy and assert the two still agree:
// TestTheExtensionStillSendsTheUnknownMessageTypeError reads the service worker
// and fails when the wording moves. Without that, a reworded message turns every
// capability probe into a hard failure on browsers running an older extension,
// with nothing to say why.
const unknownMessageType = "unknown message type"

func isUnknownMessageTypeErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), unknownMessageType)
}

func (b *Bridge) ListenAndServe() error {
	return b.server.ListenAndServe()
}

func (b *Bridge) Shutdown(ctx context.Context) error {
	var conn *websocket.Conn
	b.mu.Lock()
	if !b.shuttingDown {
		b.shuttingDown = true
		// Wake every caller parked in getConn. They observe shuttingDown before
		// consulting this gate, so leaving it closed cannot make a loop spin.
		close(b.connReady)
	}
	conn = b.conn
	b.conn = nil
	b.agentPinKnown = false
	b.disconnectedAt = time.Now().UTC()
	b.disconnectReason = shutdownDrainReason
	b.drainPendingLocked(shutdownDrainReason)
	b.mu.Unlock()

	// Never perform coder/websocket's graceful close handshake under b.mu: an
	// unresponsive peer can consume its full multi-second close timeout and
	// block status/dispatch. The server itself still receives the caller's
	// bounded Shutdown context below.
	if conn != nil {
		_ = conn.CloseNow()
	}
	return b.server.Shutdown(ctx)
}

// recordHandshakeRejection publishes a refused handshake on /status. The
// connection never goes live, so nothing else records it: an operator diagnosing
// a bridge that is turning the extension away otherwise sees only "no extension
// has connected" and reloads the same stale token. A live connection's reason is
// left alone, because a rejected newcomer is not why that one ended.
//
// The rejected hello is recorded too (minus the token, which verifyHandshake has
// already zeroed): the commonest reason a hello arrives without a token is that
// the extension's status URL addresses a port nothing listens on, and that URL
// is knowable only from the extension.
func (b *Bridge) recordHandshakeRejection(h hello, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		return
	}
	// The reason is built from the refused frame, and it reaches an operator's
	// terminal twice: as last_handshake.reason, and as disconnect_reason, which
	// brwctl doctor prints on the bridge_connected line. One sanitize covers
	// both; leaving either raw hands a rogue local client the escape sequences.
	reason := sanitizeHandshakeField(err.Error())
	b.disconnectReason = "handshake rejected: " + reason
	b.disconnectedAt = time.Now().UTC()
	b.lastHandshake = handshakeReport{
		StatusURL:    sanitizeHandshakeField(h.StatusURL),
		BridgeURL:    sanitizeHandshakeField(h.BridgeURL),
		ConfigSource: sanitizeHandshakeField(h.ConfigSource),
		Reason:       reason,
		At:           formatStatusTime(b.disconnectedAt),
	}
}

func (b *Bridge) handleStatus(w http.ResponseWriter, r *http.Request) {
	markInitiatorSensitive(w)
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
	lastHandshake := b.lastHandshake
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
	if !lastHandshake.Empty() {
		status["last_handshake"] = lastHandshake
	}
	// See tokenServable (bridge_tokenissue.go) for exactly who this reaches and
	// what it does not defend against.
	if token != "" && b.tokenServable(r) {
		status["token"] = token
	}
	writeJSON(w, http.StatusOK, status)
}

// Busy reports whether any RPC is on the wire, queued for a slot, or awaiting
// the extension's response.
func (b *Bridge) Busy() bool {
	if b.inflight.Load() > 0 || b.queued.Load() > 0 {
		return true
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.pending) > 0
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
	tabID := b.contextTabID(ctx)
	raw, err := b.call(ctx, "get_console_messages", map[string]any{"tabId": parseTabID(tabID)})
	if err == nil {
		var payload struct {
			Messages []browser.ConsoleMessage `json:"messages"`
		}
		if jsonErr := json.Unmarshal(raw, &payload); jsonErr != nil {
			return nil, jsonErr
		}
		if payload.Messages == nil {
			payload.Messages = []browser.ConsoleMessage{}
		}
		return payload.Messages, nil
	}
	if !isUnknownMessageTypeErr(err) {
		return nil, err
	}
	// Backward-compatible fallback for an extension predating native Runtime
	// event capture. It cannot see load-time exceptions, but still drains console
	// calls emitted after installation.
	var ignored json.RawMessage
	if err := b.evaluate(ctx, snapshot.ConsoleCaptureInstallScript, tabID, &ignored); err != nil {
		return nil, err
	}
	var msgs []browser.ConsoleMessage
	if err := b.evaluate(ctx, snapshot.ConsoleCaptureDrainScript, tabID, &msgs); err != nil {
		return nil, err
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

// markInitiatorSensitive marks a response whose BODY depends on who asked.
//
// /status includes the handshake token only for a caller tokenServable accepts,
// and /consent is served only to that same caller, so both vary on Origin and
// Sec-Fetch-Site. An unmarked 200 carrying a token is cacheable and
// indistinguishable from the tokenless one, which is the shape the initiator
// check exists to close. Chrome partitions its HTTP cache by top-level site and
// the extension fetches with cache:"no-store", so this closes a gap rather than
// a live path — and it is one line either way.
func markInitiatorSensitive(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Add("Vary", "Origin")
	w.Header().Add("Vary", "Sec-Fetch-Site")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
