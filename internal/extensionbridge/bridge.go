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
	// A small per-frame ceiling keeps one WebSocket message from forcing an
	// unbounded allocation; the extension chunks larger responses. The 64 MiB
	// logical cap sits below the artifact store's 128 MiB default because base64
	// captures past ~48 MiB of binary cause multi-hundred-MiB transient allocations.
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
	// consent backs the options page's /consent surface. Nil when site consent is off.
	consent *siteconsent.Guard
	// containment records tabs that already have subresource containment armed.
	containment tabArm
	// webmcp arms the WebMCP shim on every tab brw opens or drives.
	webmcp    bool
	webmcpArm tabArm
	// routes mirrors the extension's per-tab declarativeNetRequest session rules.
	routes bridgeRouteTable
	usage  *usagelog.Recorder
	server *http.Server

	// authToken is the per-launch handshake secret, served only over loopback
	// /status (unreadable cross-origin by a web page). A wrong token is always
	// rejected; a missing one only when requireToken is set. Empty disables the check.
	authToken string
	// requireToken rejects a hello with no token. New sets it true; clearing it
	// keeps a pre-0.2.0 extension that sends no token working.
	requireToken bool
	// compatWarnOnce keeps the no-token notice from repeating on every MV3 reconnect.
	compatWarnOnce sync.Once

	mu   sync.RWMutex
	conn *websocket.Conn
	// shuttingDown stops reconnect waiters and racing handlers from registering
	// work after Shutdown has drained the pending/chunk maps.
	shuttingDown bool
	acceptLog    acceptLogLimiter
	hello        hello
	active       string
	// agentPinKnown is true only once the CURRENT connection has reported its
	// extension-owned tab pin, so a request in an MV3 reconnect gap cannot act on
	// a stale b.active. Guarded by mu.
	agentPinKnown bool
	pending       map[string]chan response
	// responseChunks is charged against responseChunkBytes so concurrent callers
	// cannot multiply the per-response cap. Guarded by mu.
	responseChunks     map[string]*responseChunkAssembly
	responseChunkBytes int
	writeMu            sync.Mutex
	nextID             atomic.Uint64

	// connReady is closed when a connection goes live and replaced on every
	// reconnect (guarded by mu), so a call during an MV3 reconnect gap parks on it.
	connReady chan struct{}

	// sema caps RPCs on the shared socket: the extension runs one MV3 worker thread
	// and ~10 concurrent heavy calls starve it past b.timeout. nil means no cap.
	sema        chan struct{}
	maxInflight int

	// tabLocks serialize RPCs per tab so operations on one tab never interleave
	// CDP frames or ref state. Entries are refcounted over holders and waiters and
	// removed after the last, so a lock is never replaced under a waiter.
	tabLocksMu sync.Mutex
	tabLocks   map[string]*tabLockEntry

	// Surfaced over /status.
	inflight  atomic.Int64  // RPCs currently on the wire
	queued    atomic.Int64  // callers blocked waiting for an in-flight slot
	busyDrops atomic.Uint64 // calls rejected with ErrBridgeBusy (cap saturated)
	retries   atomic.Uint64 // idempotent calls retried after a transient drop

	connectedAt      time.Time
	lastSeenAt       time.Time
	disconnectedAt   time.Time
	disconnectReason string
	// lastHandshake is the most recent REFUSED handshake's reported config, the
	// only record outside the browser of which status URL it tried. Guarded by mu.
	lastHandshake handshakeReport

	// cancels mirrors browser.Manager's cancellation of plan/batch/wait loops.
	cancels *cancelRegistry

	// observedState is separate from action observations: brw_observe reports
	// changes since the previous brw_observe, not since an action's snapshot.
	observeMu       sync.Mutex
	observedState   map[string]*browser.SemanticState
	observeVersions map[string]int64

	traceMu sync.Mutex
	trace   []browser.TraceEntry

	// Download tracking gives recipe calls a per-tab delta after their pre-arm
	// baseline while plain brw_downloads stays full and non-draining.
	downloadsMu          sync.Mutex
	downloadFingerprints map[string]string
	downloadVersions     map[string]uint64
	downloadCursors      map[string]uint64
	downloadSequence     uint64

	// emulationStates lets clear restore UA/platform overrides, which CDP cannot clear.
	emulationMu     sync.Mutex
	emulationStates map[string]bridgeDeviceEmulationState

	// defaultGroup is the tab-group title brw_open uses when none is given. Set
	// by cmd/brwd; empty leaves Open ungrouped. Set once before serving.
	defaultGroup string
	// raiseWindowOnFocus makes focus_tab raise the Chrome window to the OS
	// foreground. The daemon sets it false so automation never steals OS focus.
	// Set once before serving.
	raiseWindowOnFocus bool
	// followFocus picks how a no-tab_id action resolves its tab. false
	// (isolation, the daemon default): only a tab brw opened or was named, never
	// the user's focused tab; with none, the first page action opens a background
	// tab. true (library default, --bridge-follow-focus): follow the focused tab.
	// Set once before serving.
	followFocus bool
	// autoOpenFailedAt drives a cooldown so a wedged extension yields one bounded
	// failure, not a full-timeout open per no-tab_id call. Guarded by b.mu.
	autoOpenFailedAt time.Time
	// recentReplaces timestamps connection replacements (guarded by b.mu). Two
	// profiles on one bridge displace each other endlessly; a burst trips flapHoldUntil.
	recentReplaces []time.Time
	// flapHoldUntil, while in the future, rejects new extension connections instead
	// of replacing the live one. Bypassed once the live connection dies.
	flapHoldUntil time.Time
}

// SetNavigationPolicy installs controller-level navigation checks. Call before
// serving requests.
func (b *Bridge) SetNavigationPolicy(p *navpolicy.Policy) { b.navPolicy = p }

// SetUsageRecorder installs the privacy-safe lifecycle ledger. Connection
// reasons are fingerprinted rather than stored verbatim.
func (b *Bridge) SetUsageRecorder(recorder *usagelog.Recorder) { b.usage = recorder }

// isolationSeedURL is what brw opens to claim a working tab when a first page
// action arrives in isolation mode before any brw_open.
const isolationSeedURL = "about:blank"

const (
	// autoOpenTimeout keeps an unresponsive extension from holding the caller for
	// the full bridge timeout.
	autoOpenTimeout = 8 * time.Second
	// autoOpenCooldown fast-fails further auto-opens after a failure.
	autoOpenCooldown = 15 * time.Second
)

const (
	// This many replacements within flapWindow means two extensions are colliding
	// on one bridge, not a normal reconnect.
	flapWindow    = 10 * time.Second
	flapThreshold = 5
	// flapHoldDuration is extended on each rejected attempt.
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
	// Build is the loaded extension's manifest version (Version is the wire
	// protocol), so an operator can see whether an unpacked reload took effect.
	Build     string `json:"build,omitempty"`
	Chrome    string `json:"chrome,omitempty"`
	Platform  string `json:"platform,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Profile   string `json:"profile,omitempty"`
	Label     string `json:"label,omitempty"`
	// AgentTabID is the extension-owned working-tab pin, not Chrome's foreground
	// tab. nil (older extension) fails closed by clearing cached ownership; 0 means none.
	AgentTabID *int `json:"agent_tab_id,omitempty"`
	// StatusURL and BridgeURL are the endpoints the extension actually uses, and
	// ConfigSource ("stored", "packaged", "built-in") the config layer that supplied
	// them; chrome.storage.local silently overrides bridge-defaults.json. Empty
	// means unreported (pre-0.6.0), not none.
	StatusURL    string `json:"status_url,omitempty"`
	BridgeURL    string `json:"bridge_url,omitempty"`
	ConfigSource string `json:"config_source,omitempty"`
	// Token is zeroed before the hello is stored or echoed in /status.
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
		// The daemon sets this false so automation never takes OS focus.
		raiseWindowOnFocus: true,
		// The daemon sets this false (isolation).
		followFocus: true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/extension", b.handleExtension)
	mux.HandleFunc("/status", b.handleStatus)
	// The options page cannot reach the control-plane API through its
	// cross-origin guard, so consent is served here beside /status.
	mux.HandleFunc("/consent", b.handleConsent)
	mux.HandleFunc("/consent/revoke", b.handleConsentRevoke)
	// No read/write timeout: the connection is hijacked into a long-lived WS.
	b.server = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return b
}

const handshakeTimeout = 5 * time.Second

// SetAuthToken installs the per-launch handshake secret the extension may
// present in its hello. Call once before ListenAndServe. An empty token leaves
// the handshake check disabled (the empty-Origin rejection still applies).
func (b *Bridge) SetAuthToken(token string) { b.authToken = strings.TrimSpace(token) }

// SetRequireToken sets whether a hello with no token is rejected. Call once
// before ListenAndServe.
func (b *Bridge) SetRequireToken(v bool) { b.requireToken = v }

// RequireToken reports whether a tokenless hello is refused.
func (b *Bridge) RequireToken() bool { return b.requireToken }

// NewAuthToken returns a fresh 256-bit URL-safe random token for SetAuthToken.
func NewAuthToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// defaultBridgeMaxInflight: the extension handles commands on one MV3 worker
// thread, and past a handful of heavy operations every in-flight call times
// out together.
const defaultBridgeMaxInflight = 6

// bridgeReconnectGrace: observed MV3 worker respawns after StatusGoingAway take
// 3-11s, and a 3s grace failed the typical case.
const bridgeReconnectGrace = 12 * time.Second

// replacedDrainReason fails pending RPCs when a different extension takes over.
// Not retryable: the other browser would answer the retry.
const replacedDrainReason = "extension connection replaced by a different extension"

// disconnectDrainReason is treated as transient so idempotent reads retry.
const disconnectDrainReason = "extension disconnected"

// shutdownDrainReason is distinct from disconnectDrainReason so shutdown does
// not enter the reconnect retry path.
const shutdownDrainReason = "extension bridge shutting down"

// ErrBridgeBusy means the in-flight cap is saturated. It is backpressure: retry
// with backoff or reduce concurrency.
var ErrBridgeBusy = errors.New("extension bridge busy: too many concurrent operations in flight, retry with backoff")

// errBridgeNotConnected is transient: the MV3 worker reconnects shortly.
var errBridgeNotConnected = errors.New("extension bridge is not connected; load/click the Chrome extension first")

// errBridgeTransport wraps a transient transport failure; idempotent ops may retry.
var errBridgeTransport = errors.New("extension bridge transport error")

var errBridgeShuttingDown = errors.New(shutdownDrainReason)

// SetMaxInflight caps concurrent RPCs on the shared socket; n<=0 disables it.
// Call once before serving: it is not safe to race with live calls.
func (b *Bridge) SetMaxInflight(n int) {
	if n <= 0 {
		b.maxInflight = 0
		b.sema = nil
		return
	}
	b.maxInflight = n
	b.sema = make(chan struct{}, n)
}

// idempotentBridgeTypes are safe to re-issue after a transport drop. "cdp" is
// excluded because it carries writes too.
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

// unknownMessageType is the extension's rejection for a message type it lacks,
// used to fall back on an older extension. The text lives in
// extension/service_worker.js; TestTheExtensionStillSendsTheUnknownMessageTypeError
// fails if the two drift.
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
		// Callers check shuttingDown before this gate, so leaving it closed cannot spin.
		close(b.connReady)
	}
	conn = b.conn
	b.conn = nil
	b.agentPinKnown = false
	b.disconnectedAt = time.Now().UTC()
	b.disconnectReason = shutdownDrainReason
	b.drainPendingLocked(shutdownDrainReason)
	b.mu.Unlock()

	// Never run the graceful close handshake under b.mu: an unresponsive peer can
	// hold it for seconds and block status/dispatch.
	if conn != nil {
		_ = conn.CloseNow()
	}
	return b.server.Shutdown(ctx)
}

// recordHandshakeRejection publishes a refused handshake on /status, with the
// hello's endpoints, which only the extension knows (a missing token usually
// means its status URL points at the wrong port). A live connection is left alone.
func (b *Bridge) recordHandshakeRejection(h hello, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		return
	}
	// The reason reaches a terminal via last_handshake and disconnect_reason
	// (brwctl doctor); sanitize so a rogue client cannot inject escape sequences.
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
		// Saturation signals for tuning --bridge-max-inflight.
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
	// See tokenServable (bridge_tokenissue.go) for who this reaches.
	if token != "" && b.tokenServable(r) {
		status["token"] = token
	}
	writeJSON(w, http.StatusOK, status)
}

// Busy reports whether any RPC is on the wire, queued, or awaiting a response.
func (b *Bridge) Busy() bool {
	if b.inflight.Load() > 0 || b.queued.Load() > 0 {
		return true
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.pending) > 0
}

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

// extensionOriginOK rejects an empty Origin, which coder/websocket would
// otherwise treat as same-origin.
func extensionOriginOK(origin string) bool {
	const prefix = "chrome-extension://"
	return strings.HasPrefix(origin, prefix) && len(origin) > len(prefix)
}

// effectiveExtensionID falls back to profilepolicy.DefaultBridgeExtensionID
// rather than the chrome-extension://* wildcard. "" means the wildcard.
func (b *Bridge) effectiveExtensionID() string {
	if b.allowedExtensionID != "" {
		return b.allowedExtensionID
	}
	return strings.TrimSpace(profilepolicy.DefaultBridgeExtensionID)
}

// Notify raises a desktop notification via chrome.notifications, which shows
// even when the agent tab is backgrounded.
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
	// Fallback for an extension without native Runtime event capture; it misses
	// load-time exceptions.
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

// markInitiatorSensitive is required because /status and /consent bodies depend
// on tokenServable; an unmarked 200 carrying the token would be cacheable.
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
