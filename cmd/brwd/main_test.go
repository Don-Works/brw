package main

import (
	"os"
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

func TestMebibytesRejectsNonPositiveLimits(t *testing.T) {
	if got, err := mebibytes(128); err != nil || got != 128<<20 {
		t.Fatalf("got %d, %v", got, err)
	}
	for _, value := range []int{0, -1} {
		if _, err := mebibytes(value); err == nil {
			t.Fatalf("mebibytes(%d) accepted", value)
		}
	}
}

func TestDefaultArtifactRootIsStableAndIsolatedByRuntimeIdentity(t *testing.T) {
	first, err := defaultArtifactRoot(brwidentity.Identity{Workspace: "synthetic", Profile: "one"})
	if err != nil {
		t.Fatal(err)
	}
	again, _ := defaultArtifactRoot(brwidentity.Identity{Workspace: "synthetic", Profile: "one", Mode: "bridge"})
	other, _ := defaultArtifactRoot(brwidentity.Identity{Workspace: "synthetic", Profile: "two"})
	fallback, _ := defaultArtifactRoot(brwidentity.Identity{})
	if first != again || first == other || filepath.Base(fallback) != "default" || !strings.HasPrefix(filepath.Base(first), "runtime-") {
		t.Fatalf("first=%q again=%q other=%q fallback=%q", first, again, other, fallback)
	}
}

func TestReadPrivateTokenFileRequiresOwnerOnlyRegularFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "provider-token")
	if err := os.WriteFile(path, []byte("sensitive-provider-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readPrivateTokenFile(path); err != nil || got != "sensitive-provider-token" {
		t.Fatalf("got %q, %v", got, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateTokenFile(path); err == nil {
		t.Fatal("world-readable token file accepted")
	}
	link := filepath.Join(directory, "token-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateTokenFile(link); err == nil {
		t.Fatal("symlink token file accepted")
	}
}
