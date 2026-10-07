// Package usagelog writes a deliberately metadata-only audit trail for brw.
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
	"github.com/Don-Works/brw/internal/siteconsent"
)

const (
	HeaderSessionID        = "X-Brw-Session"
	HeaderOwnerID          = "X-Brw-Owner"
	HeaderRequestID        = "X-Brw-Request-Id"
	HeaderClient           = "X-Brw-Client"
	HeaderErrorClass       = "X-Brw-Error-Class"
	HeaderErrorFingerprint = "X-Brw-Error-Fingerprint"
	// HeaderAgentName carries an optional human-readable agent display name used only to title the session's Chrome tab group.
	HeaderAgentName = "X-Brw-Agent-Name"
)

// Event is one privacy-safe operational record.
type Event struct {
	SnapshotMode     string `json:"snapshot_mode,omitempty"`
	OutputFormat     string `json:"output_format,omitempty"`
	DeltaRequested   *bool  `json:"delta_requested,omitempty"`
	DeltaReturned    *bool  `json:"delta_returned,omitempty"`
	ResultTruncated  *bool  `json:"result_truncated,omitempty"`
	ReadSettleMS     *int64 `json:"read_settle_ms,omitempty"`
	ElementLimit     *int64 `json:"element_limit,omitempty"`
	ReturnedElements *int64 `json:"returned_elements,omitempty"`

	Timestamp                   string `json:"ts"`
	Version                     string `json:"version,omitempty"`
	Layer                       string `json:"layer"`
	Operation                   string `json:"operation"`
	Outcome                     string `json:"outcome"`
	DurationMS                  int64  `json:"duration_ms,omitempty"`
	SchemaVersion               int    `json:"schema_version,omitempty"`
	DurationUS                  int64  `json:"duration_us,omitempty"`
	Scope                       string `json:"scope,omitempty"`
	Representation              string `json:"representation,omitempty"`
	InputBytes                  *int64 `json:"input_bytes,omitempty"`
	OutputBytes                 *int64 `json:"output_bytes,omitempty"`
	InputQueryBytes             *int64 `json:"input_query_bytes,omitempty"`
	InputTextChars              *int64 `json:"input_text_chars,omitempty"`
	OutputTextChars             *int64 `json:"output_text_chars,omitempty"`
	EstimatedInputTokensChars4  *int64 `json:"estimated_input_tokens_chars4,omitempty"`
	EstimatedOutputTokensChars4 *int64 `json:"estimated_output_tokens_chars4,omitempty"`
	BinaryInputBytes            *int64 `json:"binary_input_bytes,omitempty"`
	BinaryOutputBytes           *int64 `json:"binary_output_bytes,omitempty"`
	StructuredOutputBytes       *int64 `json:"structured_output_bytes,omitempty"`
	HTTPStatus                  int    `json:"http_status,omitempty"`
	ErrorClass                  string `json:"error_class,omitempty"`
	ErrorFingerprint            string `json:"error_fingerprint,omitempty"`
	Retryable                   bool   `json:"retryable,omitempty"`
	SessionID                   string `json:"session_id,omitempty"`
	RequestID                   string `json:"request_id,omitempty"`
	Client                      string `json:"client,omitempty"`
	Workspace                   string `json:"workspace,omitempty"`
	Profile                     string `json:"profile,omitempty"`
	Mode                        string `json:"mode,omitempty"`
	ExtensionBuild              string `json:"extension_build,omitempty"`
	PID                         int    `json:"pid"`
}

type Config struct {
	Path     string
	MaxBytes int64
	Backups  int
	Version  string
	Identity brwidentity.Identity
}

// Recorder appends newline-delimited JSON and rotates it by size.
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
	SanitizeObservation(&event)
	event.SchemaVersion = 1
	event.Scope = safeScope(event.Scope)
	event.Representation = safeRepresentation(event.Representation)
	event.Version = SafeVersion(event.Version)
	event.Layer = SafeID(event.Layer)
	event.Operation = SafeID(event.Operation)
	event.Outcome = SafeID(event.Outcome)
	event.SessionID = SafeID(event.SessionID)
	event.RequestID = SafeID(event.RequestID)
	event.Client = SafeID(event.Client)
	event.ErrorClass = SafeErrorClass(event.ErrorClass)
	event.Retryable = Retryable(event.ErrorClass)
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
	if r.file == nil {
		if err := r.open(); err != nil {
			return err
		}
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
	_, err = r.file.Write(line)
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

// SafeErrorClass returns a known error category or an empty string.
func SafeErrorClass(value string) string {
	switch value {
	case "rpc", "tool", "invalid_argument", "not_found", "stale_reference",
		"target_not_found", "target_not_actionable", "page_script_error", "artifact_error", "artifact_not_found",
		"policy_denied", "timeout", "canceled", "navigation_failed", "tab_contended", "takeover_held",
		"foreign_extension_frame", "tab_not_drivable", "debugger_conflict", "tab_lost", "tab_frozen", "tab_discarded",
		"busy", "transport", "capability", "document_identity_unavailable", "document_changed",
		"approval_required", "approval_pending", "approval_approved", "approval_denied", "approval_consumed",
		"approval_expired", "approval_stale", "approval_not_found", "approval_binding_mismatch", "approval_invalid_request",
		"approval_store_closed", "approval_persistence_failed", "approval_queue_full":
		return value
	}
	return ""
}

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

// NewID returns a non-secret correlation id.
func NewID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err == nil {
		return hex.EncodeToString(buf)
	}
	return fmt.Sprintf("fallback-%x-%x", time.Now().UnixNano(), fallbackID.Add(1))
}

// Fingerprint makes recurring failures correlatable without retaining or even hashing caller-controlled error text.
func Fingerprint(message string) string {
	sum := sha256.Sum256([]byte(failureShape(message)))
	return hex.EncodeToString(sum[:12])
}

func failureShape(message string) string {
	message = strings.ToLower(message)
	switch {
	case strings.Contains(message, "tab is leased by another browser session"):
		return "tab_contended"

	case strings.Contains(message, "foreign_extension_frame"):
		return "foreign_extension_frame"
	case strings.Contains(message, "cannot access a chrome-extension"),
		strings.Contains(message, "cannot access contents of the page"),
		strings.Contains(message, "cannot attach to this target"),
		strings.Contains(message, "no drivable tab"):
		return "tab_not_drivable"

	case strings.Contains(message, "another debugger"):
		return "debugger_conflict"
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
	case strings.Contains(message, "ref not found"),

		strings.Contains(message, "not recoverable"):
		return "ref_not_found"
	case strings.Contains(message, "not actionable"):
		return "not_actionable"
	case strings.Contains(message, "no visible element found for text"):
		return "text_not_found"
	case strings.Contains(message, "no fill target found"),
		strings.Contains(message, "no file input found"),
		strings.Contains(message, "no visible option found"),
		strings.Contains(message, "select option not found"):
		return "target_not_found"
	case strings.Contains(message, "ref hidden"):
		return "not_actionable"
	case strings.Contains(message, "runtime exception"):
		return "runtime_exception"
	case isDeviceArgumentError(message):
		return "invalid_device_argument"
	case strings.Contains(message, "one of path/paths, bytes_base64, or url is required"),
		strings.Contains(message, "path or paths is required"):
		return "upload_source_required"
	case strings.Contains(message, "provide exactly one of path/paths, bytes_base64, or url"):
		return "upload_source_conflict"
	case strings.Contains(message, "upload file") && (strings.Contains(message, "no such file") || strings.Contains(message, "not exist")):
		return "upload_file_not_found"
	case strings.Contains(message, "upload file") && strings.Contains(message, "is a directory"):
		return "upload_file_is_directory"
	case strings.Contains(message, "upload url resolves to a private"),
		strings.Contains(message, "blocked to prevent ssrf"):
		return "upload_url_blocked"
	case strings.Contains(message, "bytes_base64 exceeds"),
		strings.Contains(message, "decode bytes_base64"):
		return "invalid_upload_payload"
	case strings.Contains(message, "parse url"),
		strings.Contains(message, "url scheme") && strings.Contains(message, "not supported"):
		return "invalid_upload_url"
	case strings.Contains(message, "artifact not found"):
		return "artifact_not_found"
	case strings.Contains(message, "invalid artifact request"):
		return "invalid_artifact_request"
	case strings.Contains(message, "artifact operation failed"):
		return "artifact_operation_failed"
	case isInvalidArgumentError(message):
		return "invalid_argument"
	case strings.Contains(message, "no current window"):
		return "no_current_window"
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

func isDeviceArgumentError(message string) bool {
	return strings.Contains(message, "unknown device preset") ||
		strings.Contains(message, "width and height are required for") ||
		strings.Contains(message, "width and height must be <=") ||
		strings.Contains(message, "device_scale_factor must be <=") ||
		strings.Contains(message, "max_touch_points must be <=") ||
		strings.Contains(message, "orientation must be portrait, landscape, or omitted")
}

func isInvalidArgumentError(message string) bool {
	return isDeviceArgumentError(message) ||
		strings.Contains(message, "one of path/paths, bytes_base64, or url is required") ||
		strings.Contains(message, "provide exactly one of path/paths, bytes_base64, or url") ||
		strings.Contains(message, "path or paths is required") ||
		strings.Contains(message, "invalid artifact request") ||
		strings.Contains(message, "invalid artifact read window") ||
		strings.Contains(message, "invalid artifact search") ||
		strings.Contains(message, "invalid artifact id") ||
		strings.Contains(message, "direction must be one of back, forward, reload") ||
		strings.Contains(message, "tab id is required") ||
		strings.Contains(message, "group id is required") ||
		strings.Contains(message, "invalid tab id") ||
		strings.Contains(message, "invalid group id") ||
		strings.Contains(message, "click requires ref") ||
		strings.Contains(message, "click_text requires text") ||
		strings.Contains(message, "type requires ref and text") ||
		strings.Contains(message, "select requires ref and value") ||
		strings.Contains(message, "press requires key") ||
		strings.Contains(message, "hover requires ref") ||
		strings.Contains(message, "open requires url") ||
		strings.Contains(message, "navigate_to requires url") ||
		strings.Contains(message, "focus_tab requires id") ||
		strings.Contains(message, "ref is required") ||
		strings.Contains(message, "at least one file path is required") ||
		strings.Contains(message, "text is required") ||
		strings.Contains(message, "unsupported scroll direction") ||
		strings.Contains(message, "bytes_base64 exceeds") ||
		strings.Contains(message, "decode bytes_base64") ||
		strings.Contains(message, "parse url") ||
		(strings.Contains(message, "url scheme") && strings.Contains(message, "not supported")) ||
		strings.Contains(message, "unknown action")
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
	if errors.Is(err, os.ErrNotExist) {
		return "not_found"
	}

	if siteconsent.IsRefusal(err) {
		return "policy_denied"
	}
	msg := strings.ToLower(err.Error())
	switch {

	case strings.Contains(msg, "navigation failed:"):
		return "navigation_failed"
	case strings.Contains(msg, "tab is leased by another browser session"):
		return "tab_contended"

	case strings.Contains(msg, "holds takeover of this browser"):
		return "takeover_held"

	case strings.Contains(msg, "foreign_extension_frame"):
		return "foreign_extension_frame"
	case strings.Contains(msg, "cannot access a chrome-extension"),
		strings.Contains(msg, "cannot access contents of the page"),
		strings.Contains(msg, "cannot attach to this target"),
		strings.Contains(msg, "no drivable tab"):
		return "tab_not_drivable"
	case strings.Contains(msg, "another debugger"):
		return "debugger_conflict"
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
	case strings.Contains(msg, "requires main-document identity support"):
		return "capability"
	case strings.Contains(msg, "could not verify the main document"):
		return "document_identity_unavailable"
	case strings.Contains(msg, "crossed a main-document boundary"):
		return "document_changed"
	case strings.Contains(msg, "upload url resolves to a private"),
		strings.Contains(msg, "blocked to prevent ssrf"):
		return "policy_denied"
	case strings.Contains(msg, "ref not found"), strings.Contains(msg, "not recoverable"):
		return "stale_reference"
	case strings.Contains(msg, "no visible element found for text"),
		strings.Contains(msg, "no fill target found"),
		strings.Contains(msg, "no file input found"),
		strings.Contains(msg, "no visible option found"),
		strings.Contains(msg, "select option not found"):
		return "target_not_found"
	case strings.Contains(msg, "not actionable"), strings.Contains(msg, "ref hidden"):
		return "target_not_actionable"
	case strings.Contains(msg, "runtime exception"):
		return "page_script_error"
	case strings.Contains(msg, "artifact operation failed"):
		return "artifact_error"
	case isInvalidArgumentError(msg):
		return "invalid_argument"
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

	case "timeout", "busy", "transport", "takeover_held":
		return true
	default:
		return false
	}
}
