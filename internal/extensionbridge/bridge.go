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
	extensionFrameReadLimitBytes     = 4 << 20
	maxChunkedResponseBytes          = 64 << 20
	maxChunkedResponseChunks         = 128
	maxBufferedChunkedResponsesBytes = 128 << 20
)

type Bridge struct {
	tabAccessGuard     func(context.Context, string) error
	addr               string
	timeout            time.Duration
	allowedExtensionID string
	identity           brwidentity.Identity
	navPolicy          *navpolicy.Policy
	pacer              *browser.Pacer

	consent *siteconsent.Guard

	containment tabArm

	webmcp    bool
	webmcpArm tabArm

	routes bridgeRouteTable
	usage  *usagelog.Recorder
	server *http.Server

	authToken string

	requireToken bool

	compatWarnOnce sync.Once

	mu             sync.RWMutex
	conn           *websocket.Conn
	backgroundTabs map[string]*websocket.Conn

	shuttingDown bool
	acceptLog    acceptLogLimiter
	hello        hello
	active       string

	agentPinKnown bool
	pending       map[string]chan response

	responseChunks     map[string]*responseChunkAssembly
	responseChunkBytes int
	writeMu            sync.Mutex
	nextID             atomic.Uint64

	connReady chan struct{}

	sema        chan struct{}
	maxInflight int

	tabLocksMu sync.Mutex
	tabLocks   map[string]*tabLockEntry

	inflight  atomic.Int64
	queued    atomic.Int64
	busyDrops atomic.Uint64
	retries   atomic.Uint64

	connectedAt      time.Time
	lastSeenAt       time.Time
	disconnectedAt   time.Time
	disconnectReason string

	lastHandshake handshakeReport

	cancels *cancelRegistry

	observeMu       sync.Mutex
	observedState   map[string]*browser.SemanticState
	observeVersions map[string]int64

	traceMu sync.Mutex
	trace   []browser.TraceEntry

	downloadsMu          sync.Mutex
	downloadFingerprints map[string]string
	downloadVersions     map[string]uint64
	downloadCursors      map[string]uint64
	downloadSequence     uint64

	emulationMu     sync.Mutex
	emulationStates map[string]bridgeDeviceEmulationState

	defaultGroup string

	raiseWindowOnFocus bool

	followFocus bool

	autoOpenFailedAt time.Time

	recentReplaces []time.Time

	flapHoldUntil time.Time
}

// SetNavigationPolicy installs controller-level navigation checks.
func (b *Bridge) SetNavigationPolicy(p *navpolicy.Policy) { b.navPolicy = p }

// SetUsageRecorder installs the privacy-safe lifecycle ledger.
func (b *Bridge) SetUsageRecorder(recorder *usagelog.Recorder) { b.usage = recorder }

const isolationSeedURL = "about:blank"

const (
	autoOpenTimeout = 8 * time.Second

	autoOpenCooldown = 15 * time.Second
)

const (
	flapWindow    = 10 * time.Second
	flapThreshold = 5

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
	// Build is the loaded extension's manifest version (Version is the wire protocol), so an operator can see whether an unpacked reload took effect.
	Build     string `json:"build,omitempty"`
	Chrome    string `json:"chrome,omitempty"`
	Platform  string `json:"platform,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Profile   string `json:"profile,omitempty"`
	Label     string `json:"label,omitempty"`
	// AgentTabID is the extension-owned working-tab pin, not Chrome's foreground tab.
	AgentTabID *int `json:"agent_tab_id,omitempty"`
	// StatusURL and BridgeURL are the endpoints the extension actually uses, and ConfigSource ("stored", "packaged", "built-in") the config layer that supplied them; chrome.storage.local silently overrides bridge-defaults.json.
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

		raiseWindowOnFocus: true,

		followFocus: true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/extension", b.handleExtension)
	mux.HandleFunc("/status", b.handleStatus)

	mux.HandleFunc("/consent", b.handleConsent)
	mux.HandleFunc("/consent/revoke", b.handleConsentRevoke)

	b.server = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return b
}

const handshakeTimeout = 5 * time.Second

// SetAuthToken installs the per-launch handshake secret the extension may present in its hello.
func (b *Bridge) SetAuthToken(token string) { b.authToken = strings.TrimSpace(token) }

// SetRequireToken sets whether a hello with no token is rejected.
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

const defaultBridgeMaxInflight = 6

const bridgeReconnectGrace = 12 * time.Second

const replacedDrainReason = "extension connection replaced by a different extension"

const disconnectDrainReason = "extension disconnected"

const shutdownDrainReason = "extension bridge shutting down"

// ErrBridgeBusy means the in-flight cap is saturated.
var ErrBridgeBusy = errors.New("extension bridge busy: too many concurrent operations in flight, retry with backoff")

var errBridgeNotConnected = errors.New("extension bridge is not connected; load/click the Chrome extension first")

var errBridgeTransport = errors.New("extension bridge transport error")

var errBridgeShuttingDown = errors.New(shutdownDrainReason)

// SetMaxInflight caps concurrent RPCs on the shared socket; n<=0 disables it.
func (b *Bridge) SetMaxInflight(n int) {
	if n <= 0 {
		b.maxInflight = 0
		b.sema = nil
		return
	}
	b.maxInflight = n
	b.sema = make(chan struct{}, n)
}

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

		close(b.connReady)
	}
	conn = b.conn
	b.conn = nil
	b.agentPinKnown = false
	b.disconnectedAt = time.Now().UTC()
	b.disconnectReason = shutdownDrainReason
	b.drainPendingLocked(shutdownDrainReason)
	b.mu.Unlock()

	if conn != nil {
		_ = conn.CloseNow()
	}
	return b.server.Shutdown(ctx)
}

func (b *Bridge) recordHandshakeRejection(h hello, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		return
	}

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

func extensionOriginOK(origin string) bool {
	const prefix = "chrome-extension://"
	return strings.HasPrefix(origin, prefix) && len(origin) > len(prefix)
}

func (b *Bridge) effectiveExtensionID() string {
	if b.allowedExtensionID != "" {
		return b.allowedExtensionID
	}
	return strings.TrimSpace(profilepolicy.DefaultBridgeExtensionID)
}

// Notify raises a desktop notification via chrome.notifications, which shows even when the agent tab is backgrounded.
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
	if tabID != "" {
		ctx = browser.WithTabID(ctx, tabID)
	}
	if err := b.guardCurrentURL(ctx); err != nil {
		return nil, err
	}
	raw, err := b.call(ctx, "get_console_messages", map[string]any{"tabId": parseTabID(tabID)})
	if err == nil {
		if err := b.guardCurrentURL(ctx); err != nil {
			return nil, err
		}
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

// SetPacing makes the bridge space and type agent actions like a person.
func (b *Bridge) SetPacing(mode browser.PacingMode) { b.pacer = browser.NewPacer(mode) }

// Pacing reports the pacing mode in force.
func (b *Bridge) Pacing() browser.PacingMode { return b.pacer.Mode() }
