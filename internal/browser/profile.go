package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ProfilerController is the optional transport capability for Chrome's own
// performance trace and CPU profiler. Separate from Controller for the same
// reason the other capabilities are: a lane that cannot hold a debugger session
// long enough to collect a trace degrades to a named error.
type ProfilerController interface {
	Profile(context.Context, ProfileOptions) (ProfileResult, error)
}

// ErrProfileUnsupported is returned by transports that cannot run a trace or a
// CPU profile.
var ErrProfileUnsupported = errors.New("performance tracing and CPU profiling are not supported on the extension-bridge transport: they need a debugger session held open for the whole capture, and the extension attaches and detaches per operation; use a direct-CDP profile")

// Profile kinds.
const (
	ProfileKindTrace = "trace"
	ProfileKindCPU   = "cpu"
)

// ProfileOptions starts or stops a capture.
type ProfileOptions struct {
	// Action is start or stop.
	Action string `json:"action"`
	// Kind is trace (a Chrome performance trace) or cpu (a V8 CPU profile).
	// Defaults to trace. It must match the kind that was started.
	Kind string `json:"kind,omitempty"`
	// Categories narrows a trace to the given Chrome trace categories. Omitted
	// uses a general timeline set. Ignored for kind=cpu.
	Categories []string `json:"categories,omitempty"`
	// TTLSeconds shortens how long the stored artifact is kept. Applies when the
	// capture is stopped and stored by the surface that owns the artifact store.
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
	TabID      string `json:"tab_id,omitempty"`
}

// ProfileArtifact is the handle to a stored trace or profile, filled in by the
// surface that owns the artifact store.
type ProfileArtifact struct {
	ID        string    `json:"id"`
	MIMEType  string    `json:"mime_type"`
	SizeBytes int64     `json:"size_bytes"`
	SHA256    string    `json:"sha256,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// ProfileResult is the reply for start and stop.
type ProfileResult struct {
	OK     bool   `json:"ok"`
	Action string `json:"action"`
	Kind   string `json:"kind"`
	TabID  string `json:"tab_id,omitempty"`
	// Running reports whether a capture of this kind is now in progress.
	Running bool `json:"running"`
	// Bytes is the size of the captured document, on stop.
	Bytes int `json:"bytes,omitempty"`
	// Data is the captured trace/profile JSON. It is not serialised on the wire;
	// the owning surface stores it as an artifact and replaces it with Artifact.
	Data []byte `json:"-"`
	// Artifact is the stored handle, when a store was available.
	Artifact *ProfileArtifact `json:"artifact,omitempty"`
	// Note explains anything the caller should know (a discarded capture, a
	// data-loss flag, a missing store).
	Note string `json:"note,omitempty"`
}

// defaultTraceCategories is a general timeline set: the frames, script
// execution and loading events a performance question is usually about.
var defaultTraceCategories = []string{
	"devtools.timeline",
	"v8.execute",
	"blink.user_timing",
	"loading",
	"disabled-by-default-devtools.timeline",
}

// NormalizeProfile validates a start/stop request.
func NormalizeProfile(opts ProfileOptions) (ProfileOptions, error) {
	out := opts
	out.Action = strings.ToLower(strings.TrimSpace(opts.Action))
	switch out.Action {
	case "start", "stop":
	default:
		return out, fmt.Errorf("unknown profile action %q: use start or stop", opts.Action)
	}
	out.Kind = strings.ToLower(strings.TrimSpace(opts.Kind))
	if out.Kind == "" {
		out.Kind = ProfileKindTrace
	}
	switch out.Kind {
	case ProfileKindTrace, ProfileKindCPU:
	default:
		return out, fmt.Errorf("unknown profile kind %q: use trace or cpu", opts.Kind)
	}
	if len(opts.Categories) > 0 {
		for _, category := range opts.Categories {
			if strings.ContainsAny(category, ",\r\n") {
				return out, fmt.Errorf("trace category %q must not contain a comma or newline", category)
			}
		}
		out.Categories = append([]string(nil), opts.Categories...)
	}
	if out.Action == "start" && out.Kind == ProfileKindTrace && len(out.Categories) == 0 {
		out.Categories = append([]string(nil), defaultTraceCategories...)
	}
	return out, nil
}
