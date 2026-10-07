package mcp

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// upstreamVersioner is implemented by the --upstream-http controller. A proxy
// builds the page scripts for WebMCP, reads and snapshots itself and only asks
// the daemon to evaluate them, so a proxy started before an upgrade keeps the
// old behaviour against a new daemon until its session reconnects.
type upstreamVersioner interface {
	UpstreamVersion(ctx context.Context) (string, error)
}

const (
	versionSkewRecheck = time.Minute
	versionSkewTimeout = 2 * time.Second
)

type versionSkew struct {
	mu        sync.Mutex
	checkedAt time.Time
	note      string
	daemon    string
}

// versionSkewNote is a one-line warning when this proxy and its daemon run
// different builds, and "" otherwise. It asks the daemon at most once a minute.
func (s *Server) versionSkewNote(ctx context.Context) string {
	upstream, ok := s.manager.(upstreamVersioner)
	if !ok || Version == "dev" {
		return ""
	}
	s.skew.mu.Lock()
	defer s.skew.mu.Unlock()
	if !s.skew.checkedAt.IsZero() && time.Since(s.skew.checkedAt) < versionSkewRecheck {
		return s.skew.note
	}
	checkCtx, cancel := context.WithTimeout(ctx, versionSkewTimeout)
	defer cancel()
	daemon, err := upstream.UpstreamVersion(checkCtx)
	s.skew.checkedAt = time.Now()
	s.skew.note = ""
	s.skew.daemon = ""
	if err == nil {
		s.skew.daemon = daemon
	}
	if err == nil && daemon != "" && daemon != Version {
		s.skew.note = fmt.Sprintf("brw version skew: this session's brw MCP proxy is %s but the daemon is %s. "+
			"The proxy builds page scripts and the daemon controls the browser. Align both with the same reviewed build, "+
			"restart the affected daemon after its active sessions finish, and reconnect this MCP connection.", Version, daemon)
	}
	return s.skew.note
}

func (s *Server) identityVersions(ctx context.Context, payload map[string]any) {
	if _, ok := s.manager.(upstreamVersioner); !ok {
		return
	}
	s.versionSkewNote(ctx)
	s.skew.mu.Lock()
	daemon := s.skew.daemon
	s.skew.mu.Unlock()
	payload["proxy_version"] = Version
	payload["daemon_version"] = daemon
	alignment := "unknown"
	if daemon != "" {
		alignment = "mismatch"
		if daemon == Version {
			alignment = "matched"
		}
	}
	payload["version_alignment"] = alignment
}

// withSkewNote appends note to a tool result's content, leaving structured
// content untouched so a caller parsing it sees the same object.
func withSkewNote(result any, note string) any {
	if note == "" {
		return result
	}
	payload, ok := result.(map[string]any)
	if !ok {
		return result
	}
	content, ok := payload["content"].([]toolContent)
	if !ok {
		return result
	}
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		out[k] = v
	}
	out["content"] = append(append([]toolContent(nil), content...), toolContent{Type: "text", Text: note})
	return out
}
