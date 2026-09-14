package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/setup"
)

const (
	upgradeFromVersion = "1.0.0"
	upgradeToVersion   = "9.9.9"
	// The bridge endpoint file each per-profile extension copy carries. It is
	// per-install state an upgrade must not discard, so the fixture plants one
	// and the test asserts it survives.
	fixtureBridgeDefaults = `{"endpoint":"ws://127.0.0.1:47311/extension","token":"t0"}`
)

// upgradeFixture is an installed brw plus a release server: an app directory to
// replace, a per-profile extension copy to refresh, a policy naming a daemon,
// and an endpoint publishing an archive and its checksum.
type upgradeFixture struct {
	t          *testing.T
	home       string
	appDir     string
	policyPath string
	runner     *fakeRunner
	archive    []byte
	// publishedSHA is what the release endpoint claims the archive hashes to.
	// A test breaks the upgrade by publishing a different one.
	publishedSHA    string
	publishChecksum bool
	tagName         string
	requests        atomic.Int64
	health          daemonHealth
	daemon          *httptest.Server
	release         *httptest.Server
}

func newUpgradeFixture(t *testing.T) *upgradeFixture {
	t.Helper()
	home := t.TempDir()
	fx := &upgradeFixture{
		t:               t,
		home:            home,
		appDir:          filepath.Join(home, "app"),
		policyPath:      filepath.Join(home, "config", "browser-profiles.json"),
		runner:          newFakeRunner(),
		tagName:         "v" + upgradeToVersion,
		publishChecksum: true,
	}
	fx.health = daemonHealth{OK: true}

	// The install being upgraded.
	fx.write(filepath.Join(fx.appDir, "bin", "brwd"), "old brwd")
	fx.write(filepath.Join(fx.appDir, "bin", "brwctl"), "old brwctl")
	fx.write(filepath.Join(fx.appDir, "extension", "manifest.json"), `{"name":"brw","version":"1.0.0"}`)
	fx.write(filepath.Join(fx.appDir, "extension", "background.js"), "// old\n")
	fx.write(filepath.Join(fx.appDir, "extension-work", "manifest.json"), `{"name":"brw","version":"1.0.0"}`)
	fx.write(filepath.Join(fx.appDir, "extension-work", "background.js"), "// old\n")
	fx.write(filepath.Join(fx.appDir, "extension-work", setup.BridgeDefaultsFile), fixtureBridgeDefaults)
	// Not part of the payload: an upgrade must not be able to reach it.
	fx.write(filepath.Join(fx.appDir, "config", "browser-profiles.json"), "{}")

	fx.archive = buildReleaseArchive(t, upgradeToVersion, "linux", "amd64")
	sum := sha256.Sum256(fx.archive)
	fx.publishedSHA = hex.EncodeToString(sum[:])

	fx.daemon = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(fx.health)
	}))
	t.Cleanup(fx.daemon.Close)

	fx.release = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.requests.Add(1)
		archiveName := "brw_" + upgradeToVersion + "_linux_amd64.tar.gz"
		switch r.URL.Path {
		case "/releases/latest":
			_, _ = w.Write([]byte(`{"tag_name":"` + fx.tagName + `"}`))
		case "/download/" + archiveName:
			_, _ = w.Write(fx.archive)
		case "/download/" + archiveName + ".sha256":
			if !fx.publishChecksum {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(fx.publishedSHA + "  " + archiveName + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fx.release.Close)

	policy := profilepolicy.Policy{Profiles: []profilepolicy.Profile{{
		Name:                   fixtureProfile,
		Kind:                   setup.BrowserChrome,
		UserDataDir:            filepath.Join(home, "browser"),
		ExtensionBridgeAllowed: true,
		BridgeHTTPAddr:         fx.daemon.URL,
		// No bridge WS address: the fixture's daemon serves /health only, and
		// an unreachable bridge is correctly read as "no work in flight".
		BridgeWSAddr: "127.0.0.1:1",
	}}}
	encoded, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	fx.write(fx.policyPath, string(encoded))

	params := setup.ServiceParams{GOOS: "linux", Profile: fixtureProfile, Home: home}
	fx.write(params.UnitPath(), "[Service]\n")
	return fx
}

func (fx *upgradeFixture) write(path, content string) {
	fx.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fx.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *upgradeFixture) read(path string) string {
	fx.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		fx.t.Fatal(err)
	}
	return string(data)
}

func (fx *upgradeFixture) options() upgradeOptions {
	return upgradeOptions{
		appDir:          fx.appDir,
		policyPath:      fx.policyPath,
		apiURL:          fx.release.URL + "/releases/latest",
		baseURL:         fx.release.URL + "/download",
		home:            fx.home,
		goos:            "linux",
		goarch:          "amd64",
		current:         upgradeFromVersion,
		skipAttestation: true,
		runner:          fx.runner,
		client:          fx.release.Client(),
		out:             io.Discard,
	}
}

// buildReleaseArchive produces the tarball shape scripts/install.sh unpacks: a
// single brw_<version>_<os>_<arch> directory holding bin/ and extension/.
func buildReleaseArchive(t *testing.T, version, goos, goarch string) []byte {
	t.Helper()
	root := "brw_" + version + "_" + goos + "_" + goarch
	entries := []struct {
		name    string
		content string
		mode    int64
	}{
		{root + "/bin/brwd", "new brwd " + version, 0o755},
		{root + "/bin/brwctl", "new brwctl " + version, 0o755},
		{root + "/bin/brwcheck", "new brwcheck " + version, 0o755},
		{root + "/bin/brw-devtools-mcp", "new brw-devtools-mcp " + version, 0o755},
		{root + "/extension/manifest.json", `{"name":"brw","version":"` + version + `"}`, 0o644},
		{root + "/extension/background.js", "// " + version + "\n", 0o644},
		{root + "/skills/brw/SKILL.md", "# brw " + version + "\n", 0o644},
	}
	var raw bytes.Buffer
	zipped := gzip.NewWriter(&raw)
	archive := tar.NewWriter(zipped)
	for _, entry := range entries {
		if err := archive.WriteHeader(&tar.Header{
			Name:     entry.name,
			Mode:     entry.mode,
			Size:     int64(len(entry.content)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(entry.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipped.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func refusalFrom(t *testing.T, err error) *upgradeRefusal {
	t.Helper()
	var refusal *upgradeRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("error %v is not a named refusal", err)
	}
	return refusal
}

// TestUpgradeCheckReportsTheAvailableVersion drives --check against a fake
// release endpoint and asserts it changes nothing.
func TestUpgradeCheckReportsTheAvailableVersion(t *testing.T) {
	cases := []struct {
		name         string
		current      string
		tagName      string
		wantLatest   string
		wantUpToDate bool
	}{
		{name: "a newer release is published", current: "1.0.0", tagName: "v9.9.9", wantLatest: "9.9.9"},
		{name: "already on the latest", current: "9.9.9", tagName: "v9.9.9", wantLatest: "9.9.9", wantUpToDate: true},
		{name: "ahead of the latest", current: "12.0.0", tagName: "v9.9.9", wantLatest: "9.9.9", wantUpToDate: true},
		{name: "unstamped dev build", current: "dev", tagName: "v9.9.9", wantLatest: "9.9.9"},
		{name: "tag without the v prefix", current: "1.0.0", tagName: "9.9.9", wantLatest: "9.9.9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newUpgradeFixture(t)
			fx.tagName = tc.tagName
			opts := fx.options()
			opts.current = tc.current
			opts.check = true

			result, err := runUpgrade(opts)
			if err != nil {
				t.Fatal(err)
			}
			if result.Action != "checked" {
				t.Fatalf("action = %q, want checked", result.Action)
			}
			if result.LatestVersion != tc.wantLatest {
				t.Fatalf("latest = %q, want %q", result.LatestVersion, tc.wantLatest)
			}
			if result.UpToDate != tc.wantUpToDate {
				t.Fatalf("up_to_date = %v, want %v", result.UpToDate, tc.wantUpToDate)
			}
			if result.CurrentVersion != tc.current {
				t.Fatalf("current = %q", result.CurrentVersion)
			}
			// --check looks; it does not touch.
			if got := fx.read(filepath.Join(fx.appDir, "bin", "brwd")); got != "old brwd" {
				t.Fatalf("--check replaced the binary: %q", got)
			}
			if result.Archive != "" || result.SHA256 != "" {
				t.Fatalf("--check downloaded something: %+v", result)
			}
		})
	}
}

// TestUpgradeChecksumMismatchLeavesTheInstallUntouched: an archive that does
// not hash to its published checksum is never unpacked, so nothing on disk
// moves and the running install stays the one that was verified.
func TestUpgradeChecksumMismatchLeavesTheInstallUntouched(t *testing.T) {
	fx := newUpgradeFixture(t)
	fx.publishedSHA = strings.Repeat("a", 64)

	result, err := runUpgrade(fx.options())
	refusal := refusalFrom(t, err)
	if refusal.Reason != "checksum_mismatch" {
		t.Fatalf("refusal = %+v, want checksum_mismatch", refusal)
	}
	if !strings.Contains(refusal.Detail, fx.publishedSHA) {
		t.Fatalf("refusal does not name the published checksum: %q", refusal.Detail)
	}
	if refusal.Fix == "" {
		t.Fatal("refusal carries no next command")
	}
	if result.Action == "upgraded" {
		t.Fatalf("result claims an upgrade happened: %+v", result)
	}
	for path, want := range map[string]string{
		filepath.Join(fx.appDir, "bin", "brwd"):                "old brwd",
		filepath.Join(fx.appDir, "bin", "brwctl"):              "old brwctl",
		filepath.Join(fx.appDir, "extension", "background.js"): "// old\n",
	} {
		if got := fx.read(path); got != want {
			t.Fatalf("%s = %q, want the pre-upgrade content %q", path, got, want)
		}
	}
	version, err := setup.ExtensionPayloadVersion(filepath.Join(fx.appDir, "extension"))
	if err != nil || version != upgradeFromVersion {
		t.Fatalf("extension payload = %q (%v), want the pre-upgrade version", version, err)
	}
	if fx.runner.called("systemctl") {
		t.Fatalf("a refused upgrade restarted the service: %v", fx.runner.calls)
	}
}

// TestUpgradeRefusesWhileADaemonIsBusy: replacing the binaries under a daemon
// that is mid-request loses that agent's work, and the agent cannot tell the
// difference between the upgrade and a crash.
func TestUpgradeRefusesWhileADaemonIsBusy(t *testing.T) {
	fx := newUpgradeFixture(t)
	fx.health.TabLeases = tabLeaseStats{ActiveTabs: 2, Owners: 1, InFlight: 1}

	_, err := runUpgrade(fx.options())
	refusal := refusalFrom(t, err)
	if refusal.Reason != "daemon_busy" {
		t.Fatalf("refusal = %+v, want daemon_busy", refusal)
	}
	if !strings.Contains(refusal.Detail, fixtureProfile) || !strings.Contains(refusal.Detail, "in flight") {
		t.Fatalf("refusal does not say which daemon is busy doing what: %q", refusal.Detail)
	}
	if !strings.Contains(refusal.Fix, "--force") {
		t.Fatalf("refusal does not name a way through: %q", refusal.Fix)
	}
	if got := fx.read(filepath.Join(fx.appDir, "bin", "brwd")); got != "old brwd" {
		t.Fatalf("a refused upgrade still replaced the binary: %q", got)
	}

	// --force is the stated way through, so it must actually work.
	opts := fx.options()
	opts.force = true
	if _, err := runUpgrade(opts); err != nil {
		t.Fatalf("--force still refused: %v", err)
	}
	if got := fx.read(filepath.Join(fx.appDir, "bin", "brwd")); got != "new brwd "+upgradeToVersion {
		t.Fatalf("--force did not upgrade: %q", got)
	}
}

// TestUpgradeReplacesThePayloadAndRefreshesPerProfileExtensions is the whole
// upgrade end to end against a fake release endpoint.
func TestUpgradeReplacesThePayloadAndRefreshesPerProfileExtensions(t *testing.T) {
	fx := newUpgradeFixture(t)

	result, err := runUpgrade(fx.options())
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != "upgraded" || result.LatestVersion != upgradeToVersion {
		t.Fatalf("result = %+v", result)
	}
	if result.SHA256 != fx.publishedSHA {
		t.Fatalf("sha256 = %q, want the verified digest %q", result.SHA256, fx.publishedSHA)
	}

	if got := fx.read(filepath.Join(fx.appDir, "bin", "brwd")); got != "new brwd "+upgradeToVersion {
		t.Fatalf("brwd was not replaced: %q", got)
	}
	info, err := os.Stat(filepath.Join(fx.appDir, "bin", "brwd"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("replaced brwd is not executable: %v", info.Mode())
	}

	for _, dir := range []string{"extension", "extension-work"} {
		version, err := setup.ExtensionPayloadVersion(filepath.Join(fx.appDir, dir))
		if err != nil {
			t.Fatal(err)
		}
		if version != upgradeToVersion {
			t.Fatalf("%s is still at %s; the browser would keep running the old code", dir, version)
		}
		if got := fx.read(filepath.Join(fx.appDir, dir, "background.js")); got != "// "+upgradeToVersion+"\n" {
			t.Fatalf("%s/background.js = %q", dir, got)
		}
	}
	// The per-profile bridge endpoint and token are install state, not payload.
	if got := fx.read(filepath.Join(fx.appDir, "extension-work", setup.BridgeDefaultsFile)); got != fixtureBridgeDefaults {
		t.Fatalf("the per-profile bridge defaults were lost: %q", got)
	}
	if len(result.RefreshedExtensions) != 1 || result.RefreshedExtensions[0] != "extension-work" {
		t.Fatalf("refreshed = %v", result.RefreshedExtensions)
	}

	// Config is outside the payload and must survive an upgrade untouched.
	if got := fx.read(filepath.Join(fx.appDir, "config", "browser-profiles.json")); got != "{}" {
		t.Fatalf("upgrade reached into config/: %q", got)
	}

	if !fx.runner.called("systemctl --user restart brwd-chrome-profile.service") {
		t.Fatalf("the daemon was never restarted onto the new binary: %v", fx.runner.calls)
	}
	if len(result.RestartedServices) != 1 {
		t.Fatalf("restarted services = %v", result.RestartedServices)
	}
	if len(result.Manual) == 0 {
		t.Fatal("nothing told the operator to reload the extension")
	}
}

// TestUpgradeInstallsAPinnedOlderVersion: --version is a pin or a rollback, so
// it installs what it names rather than reporting the machine as current.
func TestUpgradeInstallsAPinnedOlderVersion(t *testing.T) {
	fx := newUpgradeFixture(t)
	opts := fx.options()
	opts.current = "12.0.0"
	opts.version = upgradeToVersion

	result, err := runUpgrade(opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != "upgraded" {
		t.Fatalf("action = %q, want the pinned version installed", result.Action)
	}
	if !result.UpToDate {
		t.Fatal("up_to_date should still report honestly that the pin is older")
	}
	if got := fx.read(filepath.Join(fx.appDir, "bin", "brwd")); got != "new brwd "+upgradeToVersion {
		t.Fatalf("the pinned version was not installed: %q", got)
	}
}

func TestUpgradeDoesNothingWhenAlreadyCurrent(t *testing.T) {
	fx := newUpgradeFixture(t)
	opts := fx.options()
	opts.current = upgradeToVersion

	result, err := runUpgrade(opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != "up-to-date" || !result.UpToDate {
		t.Fatalf("result = %+v", result)
	}
	if got := fx.read(filepath.Join(fx.appDir, "bin", "brwd")); got != "old brwd" {
		t.Fatalf("an up-to-date install was replaced anyway: %q", got)
	}
}

// TestUpgradeRefusesWithoutAPublishedChecksum: no checksum means no way to know
// what was downloaded, and install.sh refuses there too.
func TestUpgradeRefusesWithoutAPublishedChecksum(t *testing.T) {
	fx := newUpgradeFixture(t)
	fx.publishChecksum = false

	_, err := runUpgrade(fx.options())
	refusal := refusalFrom(t, err)
	if refusal.Reason != "checksum_unavailable" {
		t.Fatalf("refusal = %+v, want checksum_unavailable", refusal)
	}
	if got := fx.read(filepath.Join(fx.appDir, "bin", "brwd")); got != "old brwd" {
		t.Fatalf("an unverified archive was installed: %q", got)
	}
}

// TestUpgradeRefreshExtensionsResyncsWithoutDownloading backs the fix command
// doctor prints for a per-profile payload that has fallen behind.
func TestUpgradeRefreshExtensionsResyncsWithoutDownloading(t *testing.T) {
	fx := newUpgradeFixture(t)
	fx.write(filepath.Join(fx.appDir, "extension", "manifest.json"), `{"name":"brw","version":"2.0.0"}`)
	fx.write(filepath.Join(fx.appDir, "extension", "background.js"), "// 2.0.0\n")
	before := fx.requests.Load()

	opts := fx.options()
	opts.refreshOnly = true
	result, err := runUpgrade(opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != "refreshed" || len(result.RefreshedExtensions) != 1 {
		t.Fatalf("result = %+v", result)
	}
	version, err := setup.ExtensionPayloadVersion(filepath.Join(fx.appDir, "extension-work"))
	if err != nil || version != "2.0.0" {
		t.Fatalf("per-profile payload = %q (%v)", version, err)
	}
	if got := fx.read(filepath.Join(fx.appDir, "extension-work", setup.BridgeDefaultsFile)); got != fixtureBridgeDefaults {
		t.Fatalf("bridge defaults were lost by the refresh: %q", got)
	}
	if fx.requests.Load() != before {
		t.Fatal("--refresh-extensions reached the network")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"1.0.0", "1.0.0", 0},
		{"v1.2.0", "1.2", 0},
		{"0.9.9", "0.10.0", -1},
		{"1.2.3-rc1", "1.2.3", -1},
		{"1.2.3", "1.2.3-rc1", 1},
		{"dev", "1.0.0", -1},
		{"1.0.0", "dev", 1},
		{"dev", "dev", 0},
	}
	for _, tc := range cases {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Fatalf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestExpectedSHA256 covers both published shapes, matching the two
// scripts/install.sh accepts.
func TestExpectedSHA256(t *testing.T) {
	digest := strings.Repeat("b", 64)
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "single artifact file", body: digest + "  brw_1.0.0_linux_amd64.tar.gz\n", want: digest},
		{name: "release-wide sums", body: "0000  ./other.tar.gz\n" + digest + "  ./brw_1.0.0_linux_amd64.tar.gz\n", want: digest},
		{name: "binary marker", body: digest + " *brw_1.0.0_linux_amd64.tar.gz\n", want: digest},
		{name: "uppercase digest", body: strings.ToUpper(digest) + "  brw_1.0.0_linux_amd64.tar.gz\n", want: digest},
		{name: "no entry for this artifact", body: "0000  ./other.tar.gz\n", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := expectedSHA256([]byte(tc.body), "brw_1.0.0_linux_amd64.tar.gz"); got != tc.want {
				t.Fatalf("expectedSHA256 = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExtractTarGzRefusesToEscape: a release archive is remote input, and one
// crafted entry name would otherwise write outside the unpack directory.
func TestExtractTarGzRefusesToEscape(t *testing.T) {
	dir := t.TempDir()
	var raw bytes.Buffer
	zipped := gzip.NewWriter(&raw)
	archive := tar.NewWriter(zipped)
	body := "owned"
	if err := archive.WriteHeader(&tar.Header{
		Name: "../escaped.txt", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipped.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "evil.tar.gz")
	if err := os.WriteFile(path, raw.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	err := extractTarGz(path, filepath.Join(dir, "unpack"))
	if err == nil || !strings.Contains(err.Error(), "escapes the unpack directory") {
		t.Fatalf("extract error = %v, want a containment refusal", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "escaped.txt")); !os.IsNotExist(err) {
		t.Fatalf("the archive wrote outside the unpack directory: %v", err)
	}
}
