package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/recipe"
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

func TestLocalTransport(t *testing.T) {
	tests := []struct {
		name     string
		upstream string
		bridge   bool
		want     string
	}{
		{"direct cdp", "", false, brwidentity.TransportDirectCDP},
		{"extension bridge", "", true, brwidentity.TransportExtensionBridge},
		// A proxy cannot know how its upstream reaches Chrome, so it reports
		// empty and adopts the upstream's answer from /health.
		{"upstream proxy defers", "http://127.0.0.1:17410", false, ""},
		{"upstream proxy defers even with bridge set", "http://127.0.0.1:17410", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := localTransport(tt.upstream, tt.bridge); got != tt.want {
				t.Fatalf("localTransport(%q, %v) = %q, want %q", tt.upstream, tt.bridge, got, tt.want)
			}
		})
	}
}

func TestFindInstalledExtensionNeedsAManifest(t *testing.T) {
	dir := t.TempDir()
	withManifest := filepath.Join(dir, "good")
	bare := filepath.Join(dir, "bare")
	for _, d := range []string{withManifest, bare} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(withManifest, "manifest.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	orig := extensionSearchPaths
	t.Cleanup(func() { extensionSearchPaths = orig })

	extensionSearchPaths = func() []string { return []string{bare, withManifest} }
	got, ok := findInstalledExtension()
	if !ok || got != withManifest {
		t.Fatalf("findInstalledExtension() = %q,%v; want %q,true (a dir without a manifest is not our extension)", got, ok, withManifest)
	}

	extensionSearchPaths = func() []string { return []string{bare} }
	if got, ok := findInstalledExtension(); ok {
		t.Fatalf("findInstalledExtension() = %q,true; want not found", got)
	}
}

// A receipt only means something if it outlives the daemon that wrote it, so
// the write ledger is the provider — and only when the provider is a remote
// party. A directory of local JSON files is this machine, so it gets none.
func TestRecipeReceiptsOnlyComeFromAProviderThatOutlivesTheDaemon(t *testing.T) {
	remote, err := recipe.NewHTTPProvider(recipe.HTTPProviderConfig{BaseURL: "https://recipes.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if recipeReceiptsFor(remote) == nil {
		t.Fatal("an HTTP recipe provider is a write ledger and was not used as one")
	}

	local, err := recipe.NewCatalog(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := recipeReceiptsFor(local); got != nil {
		t.Fatalf("a local catalogue was used as a write ledger: %T", got)
	}
}
