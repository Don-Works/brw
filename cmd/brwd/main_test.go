package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/brwidentity"
)

func TestEffectiveMCPIdleExitDefaultsOnlyDisposableUpstreamProxy(t *testing.T) {
	if got := effectiveMCPIdleExit(0, true, "http://127.0.0.1:17410", false); got != defaultProxyIdleExit {
		t.Fatalf("default = %s, want %s", got, defaultProxyIdleExit)
	}
	for name, tc := range map[string]struct {
		configured time.Duration
		mcp        bool
		upstream   string
		explicit   bool
		want       time.Duration
	}{
		"explicit zero disables": {mcp: true, upstream: "http://127.0.0.1:17410", explicit: true, want: 0},
		"explicit duration wins": {configured: 15 * time.Minute, mcp: true, upstream: "http://127.0.0.1:17410", explicit: true, want: 15 * time.Minute},
		"direct MCP unchanged":   {mcp: true, want: 0},
		"daemon unchanged":       {upstream: "http://127.0.0.1:17410", want: 0},
	} {
		t.Run(name, func(t *testing.T) {
			if got := effectiveMCPIdleExit(tc.configured, tc.mcp, tc.upstream, tc.explicit); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestUsageLogFileNameContainsOnlySafeIdentityMetadata(t *testing.T) {
	got := usageLogFileName(brwidentity.Identity{
		Workspace: "agent / brw:chromium", Profile: "Default", Mode: "bridge",
	})
	if got != "agent-brw-chromium-Default-bridge.ndjson" {
		t.Fatalf("got %q", got)
	}
	if strings.ContainsAny(got, `/\\:`) {
		t.Fatalf("unsafe filename %q", got)
	}
}

func TestResolveUsageLogPathOffAndExplicit(t *testing.T) {
	if got, err := resolveUsageLogPath("off", brwidentity.Identity{}); err != nil || got != "" {
		t.Fatalf("off = %q, %v", got, err)
	}
	explicit := filepath.Join(t.TempDir(), "usage.ndjson")
	if got, err := resolveUsageLogPath(explicit, brwidentity.Identity{}); err != nil || got != explicit {
		t.Fatalf("explicit = %q, %v", got, err)
	}
}

func TestUsageLogMaxBytes(t *testing.T) {
	if got, err := usageLogMaxBytes(20); err != nil || got != 20*1024*1024 {
		t.Fatalf("got %d, %v", got, err)
	}
	if _, err := usageLogMaxBytes(-1); err == nil {
		t.Fatal("expected negative-size error")
	}
}
