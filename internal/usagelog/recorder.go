// Package usagelog writes a deliberately metadata-only audit trail for brw.
//
// The event schema has no request-argument or response-content fields. That is
// intentional: callers may type passwords, upload private files, or browse URLs
// containing tokens. The ledger records enough operational metadata to diagnose
// reliability and tab hygiene without creating a second store of browser data.
package usagelog

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Don-Works/brw/internal/brwidentity"
)

const (
	HeaderSessionID        = "X-Brw-Session"
	HeaderOwnerID          = "X-Brw-Owner"
	HeaderRequestID        = "X-Brw-Request-Id"
	HeaderClient           = "X-Brw-Client"
	HeaderErrorClass       = "X-Brw-Error-Class"
	HeaderErrorFingerprint = "X-Brw-Error-Fingerprint"
	// HeaderAgentName carries an optional human-readable agent display name used
	// only to title the session's Chrome tab group. It must never carry secrets;
	// the daemon re-sanitizes it before use and it is not written to the ledger.
	HeaderAgentName = "X-Brw-Agent-Name"
)

// Event is one privacy-safe operational record. Keep this schema metadata-only:
// never add args, text, values, page content, screenshots, headers, bodies,
// filesystem paths, titles, or URLs (including query strings).
type Event struct {
	Timestamp        string `json:"ts"`
	Version          string `json:"version,omitempty"`
	Layer            string `json:"layer"`
	Operation        string `json:"operation"`
	Outcome          string `json:"outcome"`
	DurationMS       int64  `json:"duration_ms,omitempty"`
	HTTPStatus       int    `json:"http_status,omitempty"`
	ErrorClass       string `json:"error_class,omitempty"`
	ErrorFingerprint string `json:"error_fingerprint,omitempty"`
	Retryable        bool   `json:"retryable,omitempty"`
	SessionID        string `json:"session_id,omitempty"`
	RequestID        string `json:"request_id,omitempty"`
	Client           string `json:"client,omitempty"`
	Workspace        string `json:"workspace,omitempty"`
	Profile          string `json:"profile,omitempty"`
	Mode             string `json:"mode,omitempty"`
	ExtensionBuild   string `json:"extension_build,omitempty"`
	PID              int    `json:"pid"`
}

type Config struct {
	Path     string
	MaxBytes int64
	Backups  int
	Version  string
	Identity brwidentity.Identity
}

// Recorder appends newline-delimited JSON and rotates it by size. Record is safe
// for concurrent HTTP/MCP handlers. A recorder is process-local; the long-lived
// browser daemon is the canonical writer for upstream proxy traffic.
type Recorder struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	backups  int
	version  string
	identity brwidentity.Identity
	file     *os.File
	closed   bool
}

func New(cfg Config) (*Recorder, error) {
	path := strings.TrimSpace(cfg.Path)
	if path == "" {
		return nil, errors.New("usage log path is required")
	}
	if cfg.MaxBytes < 0 {
		return nil, errors.New("usage log max bytes must be non-negative")
	}
	if cfg.Backups < 0 {
		return nil, errors.New("usage log backups must be non-negative")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create usage log directory: %w", err)
	}
	r := &Recorder{
		path: path, maxBytes: cfg.MaxBytes, backups: cfg.Backups,
		version: cfg.Version, identity: cfg.Identity,
	}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Recorder) Path() string {
	if r == nil {
		return ""
	}
	return r.path
}

func (r *Recorder) Record(event Event) error {
	if r == nil {
		return nil
	}
	if event.Timestamp == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if event.Version == "" {
		event.Version = r.version
	}
	if event.Workspace == "" {
		event.Workspace = r.identity.Workspace
	}
	if event.Profile == "" {
		event.Profile = r.identity.Profile
	}
	if event.Mode == "" {
		event.Mode = r.identity.Mode
	}
	event.Version = SafeVersion(event.Version)
	event.Layer = SafeID(event.Layer)
	event.Operation = SafeID(event.Operation)
	event.Outcome = SafeID(event.Outcome)
	event.SessionID = SafeID(event.SessionID)
	event.RequestID = SafeID(event.RequestID)
	event.Client = SafeID(event.Client)
	event.ErrorClass = SafeID(event.ErrorClass)
	event.ErrorFingerprint = SafeFingerprint(event.ErrorFingerprint)
	event.ExtensionBuild = SafeVersion(event.ExtensionBuild)
	event.Workspace = SafeID(event.Workspace)
	event.Profile = SafeID(event.Profile)
	event.Mode = SafeID(event.Mode)
	if event.Layer == "" {
		event.Layer = "unknown"
	}
	if event.Operation == "" {
		event.Operation = "unknown"
	}
	if event.Outcome == "" {
		event.Outcome = "unknown"
	}
	event.PID = os.Getpid()

	line, err := json.Marshal(event)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return os.ErrClosed
	}
	if r.maxBytes > 0 {
		info, statErr := r.file.Stat()
		if statErr != nil {
			return statErr
		}
		if info.Size() > 0 && info.Size()+int64(len(line)) > r.maxBytes {
			if err := r.rotate(); err != nil {
				return err
			}
		}
	}
	_, err = r.file.Write(line) // one append write: a record is never split by us
	return err
}

func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.file == nil {
		return nil
	}
	return r.file.Close()
}

func (r *Recorder) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open usage log: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("secure usage log permissions: %w", err)
	}
	r.file = f
	return nil
}

func (r *Recorder) rotate() error {
	if err := r.file.Close(); err != nil {
		return err
	}
	r.file = nil
	if r.backups == 0 {
		if err := os.Remove(r.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		_ = os.Remove(fmt.Sprintf("%s.%d", r.path, r.backups))
		for i := r.backups - 1; i >= 1; i-- {
			oldPath := fmt.Sprintf("%s.%d", r.path, i)
			newPath := fmt.Sprintf("%s.%d", r.path, i+1)
			if err := os.Rename(oldPath, newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := os.Rename(r.path, r.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return r.open()
}

var safeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,95}$`)
var safeVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

func SafeID(value string) string {
	value = strings.TrimSpace(value)
	if !safeIDPattern.MatchString(value) {
		return ""
	}
	return value
}

func SafeVersion(value string) string {
	value = strings.TrimSpace(value)
	if !safeVersionPattern.MatchString(value) {
		return ""
	}
	return value
}

func SafeFingerprint(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 24 {
		return ""
	}
	if _, err := hex.DecodeString(value); err != nil {
		return ""
	}
	return value
}

var fallbackID atomic.Uint64

// NewID returns a non-secret correlation id. It never incorporates usernames,
// arguments, URLs, or browser content.
func NewID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err == nil {
		return hex.EncodeToString(buf)
	}
	return fmt.Sprintf("fallback-%x-%x", time.Now().UnixNano(), fallbackID.Add(1))
}

// Fingerprint makes recurring failures correlatable without retaining or even
// hashing caller-controlled error text. It hashes only an allowlisted failure
// shape, so a low-entropy password embedded in an error cannot be recovered by
// guessing candidates against the ledger.
func Fingerprint(message string) string {
	sum := sha256.Sum256([]byte(failureShape(message)))
	return hex.EncodeToString(sum[:12])
}

func failureShape(message string) string {
	message = strings.ToLower(message)
	switch {
	case strings.Contains(message, "tab is leased by another browser session"):
		return "tab_contended"
	case strings.Contains(message, "no tab"), strings.Contains(message, "tab not found"), strings.Contains(message, "cannot find tab"), strings.Contains(message, "target closed"):
		return "tab_lost"
	case strings.Contains(message, "no response from downstream"):
		return "no_response_from_downstream"
	case strings.Contains(message, "deadline exceeded"):
		return "deadline_exceeded"
	case strings.Contains(message, "timed out"), strings.Contains(message, "timeout"):
		return "timeout"
	case strings.Contains(message, "context canceled"), strings.Contains(message, "context cancelled"):
		return "canceled"
	case strings.Contains(message, "bridge busy"):
		return "bridge_busy"
	case strings.Contains(message, "max inflight"), strings.Contains(message, "too many concurrent"):
		return "concurrency_limit"
	case strings.Contains(message, "not connected"):
		return "not_connected"
	case strings.Contains(message, "websocket"):
		return "websocket"
	case strings.Contains(message, "connection reset"):
		return "connection_reset"
	case strings.Contains(message, "broken pipe"):
		return "broken_pipe"
	case strings.Contains(message, "unexpected end of json"):
		return "unexpected_json_eof"
	case strings.Contains(message, "ref not found"):
		return "ref_not_found"
	case strings.Contains(message, "not actionable"):
		return "not_actionable"
	case strings.Contains(message, "grouping is not supported by tabs in this window"),
		strings.Contains(message, "tab grouping unavailable"):
		return "tab_grouping_unsupported"
	case strings.Contains(message, "frozen by chrome"):
		return "tab_frozen"
	case strings.Contains(message, "discarded by chrome"):
		return "tab_discarded"
	case strings.Contains(message, "navigation") && (strings.Contains(message, "blocked") || strings.Contains(message, "denied")):
		return "navigation_denied"
	case strings.Contains(message, "identity mismatch"):
		return "identity_mismatch"
	case strings.Contains(message, "status "):
		return "http_status"
	default:
		return "other"
	}
}

// ClassifyError returns only a stable, non-sensitive category.
func ClassifyError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "tab is leased by another browser session"):
		return "tab_contended"
	case strings.Contains(msg, "no tab"), strings.Contains(msg, "tab not found"), strings.Contains(msg, "cannot find tab"), strings.Contains(msg, "target closed"):
		return "tab_lost"
	case strings.Contains(msg, "deadline exceeded"),
		strings.Contains(msg, "timed out"),
		strings.Contains(msg, "timeout"),
		strings.Contains(msg, "no response from downstream"):
		return "timeout"
	case strings.Contains(msg, "bridge busy"),
		strings.Contains(msg, "max inflight"),
		strings.Contains(msg, "too many concurrent"):
		return "busy"
	case strings.Contains(msg, "not connected"),
		strings.Contains(msg, "bridge transport"),
		strings.Contains(msg, "websocket"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "unexpected end of json"):
		return "transport"
	case strings.Contains(msg, "grouping is not supported by tabs in this window"),
		strings.Contains(msg, "tab grouping unavailable"):
		return "capability"
	case strings.Contains(msg, "frozen by chrome"):
		return "tab_frozen"
	case strings.Contains(msg, "discarded by chrome"):
		return "tab_discarded"
	default:
		return "tool"
	}
}

func Retryable(class string) bool {
	switch class {
	case "timeout", "busy", "transport":
		return true
	default:
		return false
	}
}
