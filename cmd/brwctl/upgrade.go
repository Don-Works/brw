package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/discovery"
	"github.com/Don-Works/brw/internal/mcp"
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/setup"
)

const (
	releaseRepo = "Don-Works/brw"
	// releaseWorkflow is the only workflow whose signature is accepted for a
	// release artifact. Verifying an attestation without pinning the signer
	// proves only that something in GitHub built the file, which any fork can
	// arrange; scripts/install.sh pins the same workflow.
	releaseWorkflow    = releaseRepo + "/.github/workflows/release.yml"
	releaseLatestAPI   = "https://api.github.com/repos/" + releaseRepo + "/releases/latest"
	releaseDownloadURL = "https://github.com/" + releaseRepo + "/releases/download"
	installScriptURL   = "https://brw.donworks.co.uk/install.sh"
	// maxArchiveBytes bounds a download from a redirect-controlled URL so a
	// mirror cannot fill the disk before the checksum ever runs.
	maxArchiveBytes = int64(512 << 20)
)

const upgradeUsage = `usage: brwctl upgrade [options]

Replace this install in place with a published brw release: resolve the latest
version, verify the archive's SHA256 and its GitHub build provenance, swap the
binaries and extension payload, refresh every per-profile extension copy, and
restart the per-user daemons. Refuses while a daemon is mid-operation.

options:
  --check               report the available version and exit without changing anything
  --version V           install this version instead of the latest published one, even
                        when it is not newer than the running one (pin or roll back)
  --app-dir PATH        brw app install directory to replace
  --profile-policy PATH profile policy JSON, used to find the daemons to restart
  --skip-attestation    skip the build provenance check; the SHA256 check still runs
  --refresh-extensions  only re-sync the per-profile extension payloads from the
                        installed one, download nothing
  --force               proceed even when a daemon reports work in flight, or when the
                        running version is already the published one
  --json                print the result as JSON
  --timeout D           per-request timeout for the release and daemon probes
  --help                print this message`

// upgradeResult is the `brwctl upgrade` JSON contract.
type upgradeResult struct {
	Action              string   `json:"action"`
	CurrentVersion      string   `json:"current_version"`
	LatestVersion       string   `json:"latest_version,omitempty"`
	UpToDate            bool     `json:"up_to_date"`
	AppDir              string   `json:"app_dir,omitempty"`
	Archive             string   `json:"archive,omitempty"`
	SHA256              string   `json:"sha256,omitempty"`
	Provenance          string   `json:"provenance,omitempty"`
	RefreshedExtensions []string `json:"refreshed_extensions,omitempty"`
	RestartedServices   []string `json:"restarted_services,omitempty"`
	Manual              []string `json:"manual,omitempty"`
}

// upgradeRefusal is a refusal with a name. The name is what a caller matches
// on and what a log line is greppable by; Fix is the command that clears it.
type upgradeRefusal struct {
	Reason string `json:"reason"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

func (r *upgradeRefusal) Error() string {
	if r.Fix == "" {
		return r.Reason + ": " + r.Detail
	}
	return r.Reason + ": " + r.Detail + "; " + r.Fix
}

func refuse(reason, detail, fix string) *upgradeRefusal {
	return &upgradeRefusal{Reason: reason, Detail: detail, Fix: fix}
}

type upgradeOptions struct {
	appDir     string
	policyPath string
	version    string
	// apiURL and baseURL exist so the release endpoint can be pointed at a
	// mirror, and so this command is testable without reaching GitHub.
	apiURL  string
	baseURL string
	home    string
	goos    string
	goarch  string
	// current is the running binary's version, stamped in at build time.
	current         string
	check           bool
	force           bool
	asJSON          bool
	skipAttestation bool
	refreshOnly     bool
	timeout         time.Duration
	out             io.Writer
	runner          commandRunner
	client          *http.Client
}

func upgradeCommand(args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var help bool
	opts := upgradeOptions{
		goos:    runtime.GOOS,
		goarch:  runtime.GOARCH,
		current: mcp.Version,
		out:     os.Stdout,
		runner:  execRunner{},
	}
	fs.StringVar(&opts.appDir, "app-dir", defaultAppDir(), "brw app install directory")
	fs.StringVar(&opts.policyPath, "profile-policy", os.Getenv("BRW_PROFILE_POLICY"), "profile policy JSON path")
	fs.StringVar(&opts.version, "version", os.Getenv("BRW_VERSION"), "version to install (default: the latest release)")
	fs.StringVar(&opts.apiURL, "release-api", envDefault("BRW_RELEASE_API", releaseLatestAPI), "release metadata URL")
	fs.StringVar(&opts.baseURL, "base-url", os.Getenv("BRW_BASE_URL"), "release download base URL")
	fs.BoolVar(&opts.check, "check", false, "report the available version and exit")
	fs.BoolVar(&opts.force, "force", false, "upgrade even when a daemon reports work in flight")
	fs.BoolVar(&opts.asJSON, "json", false, "print the result as JSON")
	fs.BoolVar(&opts.skipAttestation, "skip-attestation", os.Getenv("BRW_SKIP_ATTESTATION") != "", "skip the build provenance check")
	fs.BoolVar(&opts.refreshOnly, "refresh-extensions", false, "only re-sync the per-profile extension payloads")
	fs.DurationVar(&opts.timeout, "timeout", 30*time.Second, "per-request timeout")
	fs.BoolVar(&help, "help", false, "print usage")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, upgradeUsage)
		return err
	}
	if help {
		fmt.Fprintln(os.Stdout, upgradeUsage)
		return nil
	}
	opts.home, _ = os.UserHomeDir()

	result, err := runUpgrade(opts)
	if opts.asJSON {
		writeJSON(os.Stdout, upgradeJSON(result, err))
	} else {
		renderUpgrade(opts.out, result, err)
	}
	return err
}

// upgradeJSON keeps a refusal inside the same document as a success, so a
// machine consumer never has to parse stderr to learn why nothing happened.
func upgradeJSON(result upgradeResult, err error) any {
	var refusal *upgradeRefusal
	if errors.As(err, &refusal) {
		return struct {
			upgradeResult
			Refusal *upgradeRefusal `json:"refusal"`
		}{result, refusal}
	}
	if err != nil {
		return struct {
			upgradeResult
			Error string `json:"error"`
		}{result, err.Error()}
	}
	return result
}

// runUpgrade does the whole upgrade, and does every check that can refuse it
// before it touches the app directory. Everything downloaded is verified in a
// temporary directory first, so an archive that fails its checksum or its
// provenance leaves the install exactly as it was.
func runUpgrade(opts upgradeOptions) (upgradeResult, error) {
	opts = opts.normalise()
	result := upgradeResult{CurrentVersion: opts.current, AppDir: opts.appDir}

	if opts.refreshOnly {
		refreshed, err := setup.RefreshExtensionPayloads(opts.appDir)
		if err != nil {
			return result, err
		}
		result.Action = "refreshed"
		result.RefreshedExtensions = refreshed
		result.Manual = reloadExtensionNote(refreshed)
		return result, nil
	}

	version, err := resolveTargetVersion(opts)
	if err != nil {
		return result, err
	}
	result.LatestVersion = version
	result.UpToDate = compareVersions(opts.current, version) >= 0
	if opts.check {
		result.Action = "checked"
		return result, nil
	}
	// An explicit --version is a pin or a rollback, so it installs what it names
	// even when that is not newer than what is running.
	pinned := strings.TrimSpace(opts.version) != ""
	if result.UpToDate && !opts.force && !pinned {
		result.Action = "up-to-date"
		return result, nil
	}

	archive, err := releaseArchiveName(version, opts.goos, opts.goarch)
	if err != nil {
		return result, err
	}
	result.Archive = archive

	policy := upgradePolicy(opts)
	// Refuse before the download as well as after it: an operator who ran this
	// mid-session should learn that in a second, not after 50 MB.
	if err := ensureNotBusy(opts, policy); err != nil {
		return result, err
	}

	tempDir, err := os.MkdirTemp("", "brw-upgrade-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(tempDir)

	base := opts.baseURL
	if base == "" {
		base = releaseDownloadURL + "/v" + version
	}
	base = strings.TrimRight(base, "/")
	archivePath := filepath.Join(tempDir, archive)
	if err := download(opts.client, base+"/"+archive, archivePath); err != nil {
		return result, refuse("download_failed",
			fmt.Sprintf("could not download %s/%s: %v", base, archive, err),
			"brwctl upgrade --version <a version that publishes a "+opts.goos+"/"+opts.goarch+" tarball>")
	}

	sum, err := verifyChecksum(opts, base, archive, archivePath)
	if err != nil {
		return result, err
	}
	result.SHA256 = sum

	provenance, err := verifyProvenance(opts, archivePath)
	if err != nil {
		return result, err
	}
	result.Provenance = provenance

	unpackDir := filepath.Join(tempDir, "unpack")
	if err := extractTarGz(archivePath, unpackDir); err != nil {
		return result, err
	}
	unpacked := filepath.Join(unpackDir, strings.TrimSuffix(archive, ".tar.gz"))
	if _, err := os.Stat(filepath.Join(unpacked, "bin")); err != nil {
		return result, fmt.Errorf("%s does not contain the expected %s/bin directory", archive, filepath.Base(unpacked))
	}

	// The window between the first busy check and here is a download long, and
	// this is the check that actually protects a running agent.
	if err := ensureNotBusy(opts, policy); err != nil {
		return result, err
	}

	refreshed, err := setup.InstallPayload(unpacked, opts.appDir)
	if err != nil {
		return result, err
	}
	result.RefreshedExtensions = refreshed
	if err := finishBinaries(opts); err != nil {
		// Unexecutable binaries are not an upgrade. Claiming one here would put
		// action:"upgraded" in the same document as the error that stopped it.
		return result, err
	}
	result.Action = "upgraded"
	restarted, manual := restartServices(opts, policy)
	result.RestartedServices = restarted
	result.Manual = manual
	result.Manual = append(result.Manual, reloadExtensionNote(refreshed)...)
	return result, nil
}

func (o upgradeOptions) normalise() upgradeOptions {
	if o.out == nil {
		o.out = io.Discard
	}
	if o.runner == nil {
		o.runner = execRunner{}
	}
	if o.timeout <= 0 {
		o.timeout = 30 * time.Second
	}
	if o.client == nil {
		o.client = upgradeClient(o.timeout)
	}
	if o.goos == "" {
		o.goos = runtime.GOOS
	}
	if o.goarch == "" {
		o.goarch = runtime.GOARCH
	}
	if o.current == "" {
		o.current = mcp.Version
	}
	if o.apiURL == "" {
		o.apiURL = releaseLatestAPI
	}
	return o
}

func reloadExtensionNote(refreshed []string) []string {
	if len(refreshed) == 0 {
		return nil
	}
	return []string{"An extension on 0.7.3 or later reloads itself onto the new payload once the agent is idle. An older one needs one reload by hand (chrome://extensions, Reload) or a browser restart."}
}

// upgradePolicy is best-effort: a machine with no readable policy still gets
// upgraded, it just has no daemons to check for in-flight work or to restart.
func upgradePolicy(opts upgradeOptions) profilepolicy.Policy {
	policy, err := profilepolicy.Load(opts.policyPath)
	if err != nil {
		return profilepolicy.Policy{}
	}
	return policy
}

func resolveTargetVersion(opts upgradeOptions) (string, error) {
	if version := strings.TrimPrefix(strings.TrimSpace(opts.version), "v"); version != "" {
		if !validVersion(version) {
			return "", fmt.Errorf("--version must look like 0.10.3, got %q", version)
		}
		return version, nil
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := fetchJSON(opts.client, opts.apiURL, &release); err != nil {
		return "", refuse("release_unavailable",
			"could not read the latest release from "+opts.apiURL+": "+err.Error(),
			"brwctl upgrade --version <version>")
	}
	version := strings.TrimPrefix(strings.TrimSpace(release.TagName), "v")
	if !validVersion(version) {
		return "", refuse("release_unavailable",
			fmt.Sprintf("%s returned tag_name %q, which is not a version", opts.apiURL, release.TagName),
			"brwctl upgrade --version <version>")
	}
	return version, nil
}

// validVersion accepts the same characters scripts/install.sh does. The version
// is interpolated into a URL and a path, so anything else is refused rather
// than escaped.
func validVersion(version string) bool {
	if version == "" {
		return false
	}
	for _, r := range version {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func releaseArchiveName(version, goos, goarch string) (string, error) {
	if goos != "darwin" && goos != "linux" {
		return "", refuse("unsupported_platform",
			"brw ships upgradeable tarballs for macOS and Linux only, not "+goos,
			"download the installer for this platform from https://github.com/"+releaseRepo+"/releases")
	}
	if goarch != "amd64" && goarch != "arm64" {
		return "", refuse("unsupported_platform",
			"brw ships amd64 and arm64 archives, not "+goarch,
			"build from source: https://github.com/"+releaseRepo)
	}
	return fmt.Sprintf("brw_%s_%s_%s.tar.gz", version, goos, goarch), nil
}

// verifyChecksum reproduces scripts/install.sh's rule exactly: the per-artifact
// .sha256 first, the release-wide SHA256SUMS.txt as a fallback, and no install
// at all when neither publishes one. A missing checksum is a refusal, never a
// downgrade to "install it anyway".
func verifyChecksum(opts upgradeOptions, base, name, path string) (string, error) {
	actual, err := sha256File(path)
	if err != nil {
		return "", err
	}
	var expected string
	for _, url := range []string{base + "/" + name + ".sha256", base + "/SHA256SUMS.txt"} {
		data, err := fetchBytes(opts.client, url)
		if err != nil {
			continue
		}
		if expected = expectedSHA256(data, name); expected != "" {
			break
		}
	}
	if expected == "" {
		return "", refuse("checksum_unavailable",
			"no published checksum for "+name+"; refusing to install an unverified archive",
			"gh release view v"+strings.TrimSuffix(strings.TrimPrefix(name, "brw_"), ".tar.gz")+" --repo "+releaseRepo)
	}
	if !strings.EqualFold(actual, expected) {
		return "", refuse("checksum_mismatch",
			fmt.Sprintf("%s hashes to %s but the published checksum is %s; the existing install was left untouched", name, actual, expected),
			"brwctl upgrade   # retry; if it repeats, the download source is not serving the published release")
	}
	return strings.ToLower(actual), nil
}

// verifyProvenance checks that GitHub attests this artifact was built by this
// repository's release workflow. Not being able to check (no gh, not logged in)
// is reported and allowed, exactly as scripts/install.sh treats it; a check that
// runs and fails aborts the upgrade.
func verifyProvenance(opts upgradeOptions, archivePath string) (string, error) {
	name := filepath.Base(archivePath)
	if opts.skipAttestation {
		return "skipped (--skip-attestation)", nil
	}
	if _, ok := opts.runner.look("gh"); !ok {
		return "unverified (gh is not installed; check by hand with: gh attestation verify " + name + " --repo " + releaseRepo + ")", nil
	}
	out, err := opts.runner.run("gh", "attestation", "verify", archivePath,
		"--repo", releaseRepo, "--signer-workflow", releaseWorkflow, "--format", "json")
	switch {
	case err == nil:
		if signer := firstJSONString([]byte(out), "buildSignerURI"); signer != "" {
			return "verified, signed by " + signer, nil
		}
		return "verified", nil
	case exitCode(err) == 4:
		// gh reserves exit 4 for "not authenticated", which is an inability to
		// check rather than a failed check.
		return "unverified (gh is not authenticated)", nil
	}
	return "", refuse("attestation_failed",
		"build provenance verification failed for "+name+": "+strings.TrimSpace(out),
		"gh auth login   # then re-run brwctl upgrade")
}

// ensureNotBusy refuses while any of this machine's daemons is mid-operation.
func ensureNotBusy(opts upgradeOptions, policy profilepolicy.Policy) error {
	if opts.force {
		return nil
	}
	// A short timeout of its own: a daemon that is not answering has no work to
	// interrupt, and waiting the download timeout to learn that helps nobody.
	busy := busyDaemons(policy, &http.Client{Timeout: 3 * time.Second})
	if len(busy) == 0 {
		return nil
	}
	details := make([]string, 0, len(busy))
	for _, daemon := range busy {
		details = append(details, daemon.Profile+" has "+daemon.Detail)
	}
	return refuse("daemon_busy",
		strings.Join(details, "; ")+"; replacing the binaries now would abort that work",
		"brwctl upgrade --force   # or wait for the run to finish and re-run brwctl upgrade")
}

// finishBinaries makes the replaced commands runnable. On Apple Silicon an
// unpacked Go binary whose signature did not survive the round trip is SIGKILLed
// ("Killed: 9"); only a binary that actually fails verification is re-signed, so
// a real Developer ID signature is never thrown away to fix a problem it does
// not have.
func finishBinaries(opts upgradeOptions) error {
	commands := []string{"brwd", "brwctl", "brwcheck", "brw-devtools-mcp"}
	paths := make([]string, 0, len(commands))
	for _, name := range commands {
		path := filepath.Join(opts.appDir, "bin", name)
		if opts.goos == "windows" {
			path += ".exe"
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := os.Chmod(path, 0o755); err != nil {
			return err
		}
		paths = append(paths, path)
	}
	if opts.goos != "darwin" {
		return nil
	}
	if _, ok := opts.runner.look("xattr"); ok {
		_, _ = opts.runner.run("xattr", "-cr", filepath.Join(opts.appDir, "bin"))
	}
	if _, ok := opts.runner.look("codesign"); !ok {
		return nil
	}
	for _, path := range paths {
		if _, err := opts.runner.run("codesign", "--verify", "--strict", path); err == nil {
			continue
		}
		_, _ = opts.runner.run("codesign", "--force", "--sign", "-", path)
	}
	return nil
}

func fetchBytes(client *http.Client, url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func renderUpgrade(w io.Writer, result upgradeResult, err error) {
	fmt.Fprintf(w, "installed %s\n", result.CurrentVersion)
	if result.LatestVersion != "" {
		fmt.Fprintf(w, "published %s\n", result.LatestVersion)
	}
	var refusal *upgradeRefusal
	if errors.As(err, &refusal) {
		fmt.Fprintf(w, "\nrefused (%s): %s\n", refusal.Reason, refusal.Detail)
		if refusal.Fix != "" {
			fmt.Fprintf(w, "run: %s\n", refusal.Fix)
		}
		return
	}
	if err != nil {
		return
	}
	switch result.Action {
	case "checked":
		if result.UpToDate {
			fmt.Fprintln(w, "\nthis install is current")
			return
		}
		fmt.Fprintf(w, "\n%s is available. Install it with: brwctl upgrade\n", result.LatestVersion)
	case "up-to-date":
		fmt.Fprintln(w, "\nthis install is current; nothing to do")
	case "refreshed":
		fmt.Fprintf(w, "\nrefreshed %d per-profile extension payload(s)\n", len(result.RefreshedExtensions))
	case "upgraded":
		fmt.Fprintf(w, "\nupgraded %s to %s\n", result.AppDir, result.LatestVersion)
		fmt.Fprintf(w, "sha256 %s\n", result.SHA256)
		fmt.Fprintf(w, "provenance %s\n", result.Provenance)
	}
	for _, name := range result.RefreshedExtensions {
		fmt.Fprintf(w, "  refreshed %s\n", name)
	}
	for _, label := range result.RestartedServices {
		fmt.Fprintf(w, "  restarted %s\n", label)
	}
	for _, item := range result.Manual {
		fmt.Fprintf(w, "  still to do: %s\n", item)
	}
}

// extractTarGz unpacks a release archive into dest. Every entry is contained
// inside dest before anything is written, and link entries are refused
// outright: an archive is remote input, and one crafted entry would otherwise
// let a mirror write outside the app directory entirely.
func extractTarGz(archive, dest string) error {
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	zipped, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("%s is not a gzip archive: %w", filepath.Base(archive), err)
	}
	defer zipped.Close()
	reader := tar.NewReader(zipped)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := containedPath(dest, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(header.Mode).Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, io.LimitReader(reader, maxArchiveBytes)); err != nil {
				_ = out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			// A containment check on a link entry can only read the linkname
			// lexically, against the parent the entry DECLARES. Two links that
			// each read as contained still chain into an escape, because the
			// second is created THROUGH the first and so lands somewhere else
			// entirely; repeating the hop climbs to the root. Refusal is the
			// only check that holds, and scripts/package-tarball.sh stages a
			// tree with no links, so a link entry is a mirror's invention.
			return fmt.Errorf("archive entry %q is a link, and a release archive contains none", header.Name)
		}
	}
}

func containedPath(root, name string) (string, error) {
	target := filepath.Join(root, filepath.FromSlash(name))
	if !within(root, target) {
		return "", fmt.Errorf("archive entry %q escapes the unpack directory", name)
	}
	return target, nil
}

func within(root, path string) bool {
	cleanRoot := filepath.Clean(root) + string(filepath.Separator)
	return strings.HasPrefix(filepath.Clean(path)+string(filepath.Separator), cleanRoot)
}

// compareVersions orders two release versions: -1 when a is older than b. An
// unparseable version — "dev", the default when the binary was not built with
// the release ldflags — sorts below every real release, so an unstamped build
// is always offered the upgrade rather than told it is current.
func compareVersions(a, b string) int {
	numsA, preA, okA := parseVersion(a)
	numsB, preB, okB := parseVersion(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return -1
	case !okB:
		return 1
	}
	for i := 0; i < len(numsA) || i < len(numsB); i++ {
		x, y := versionField(numsA, i), versionField(numsB, i)
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case preA == preB:
		return 0
	case preA == "":
		// 1.2.3 is newer than 1.2.3-rc1.
		return 1
	case preB == "":
		return -1
	}
	return strings.Compare(preA, preB)
}

func versionField(nums []int, i int) int {
	if i < len(nums) {
		return nums[i]
	}
	return 0
}

func parseVersion(v string) (nums []int, prerelease string, ok bool) {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "v"))
	if v == "" {
		return nil, "", false
	}
	if index := strings.IndexAny(v, "-+"); index >= 0 {
		prerelease = v[index+1:]
		v = v[:index]
	}
	for _, field := range strings.Split(v, ".") {
		n, err := strconv.Atoi(field)
		if err != nil {
			return nil, "", false
		}
		nums = append(nums, n)
	}
	return nums, prerelease, len(nums) > 0
}

// exitCode is the process exit status behind a runner error, or -1 when the
// command never ran. gh reserves 4 for "not authenticated", which is an
// inability to check rather than a failed check.
func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// firstJSONString returns the first value of key in document order. gh emits
// its attestation JSON on one line with the same key names at several nesting
// levels, so the answer has to come from a token scan rather than a map, whose
// iteration order would pick an arbitrary one.
func firstJSONString(data []byte, key string) string {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	for {
		token, err := decoder.Token()
		if err != nil {
			return ""
		}
		name, ok := token.(string)
		if !ok || name != key {
			continue
		}
		value, err := decoder.Token()
		if err != nil {
			return ""
		}
		if text, ok := value.(string); ok {
			return text
		}
	}
}

// expectedSHA256 finds the checksum for name in either a single-artifact
// "<sha>  <name>" file or the release-wide SHA256SUMS.txt, whose names carry a
// ./ prefix and may carry a binary marker. Same two shapes scripts/install.sh
// accepts, so the CLI and the installer cannot disagree about what verified.
func expectedSHA256(data []byte, name string) string {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.ReplaceAll(line, "*", ""))
		if len(fields) < 2 {
			continue
		}
		if filepath.Base(strings.TrimPrefix(fields[len(fields)-1], "./")) != name {
			continue
		}
		return strings.ToLower(fields[0])
	}
	return ""
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// upgradeClient refuses a redirect that downgrades https to http. A release URL
// is fetched before anything about it has been verified, so a redirect chain
// must not be able to move the download onto a plaintext hop.
func upgradeClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
				return fmt.Errorf("refusing redirect from https to %s", req.URL.Scheme)
			}
			return nil
		},
	}
}

func download(client *http.Client, url, dest string) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	file, err := os.Create(dest)
	if err != nil {
		return err
	}
	written, err := io.Copy(file, io.LimitReader(resp.Body, maxArchiveBytes+1))
	if err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if written > maxArchiveBytes {
		return fmt.Errorf("%s is larger than the %d byte limit", url, maxArchiveBytes)
	}
	return nil
}

// busyDaemon names one daemon that is mid-operation and what it is doing.
type busyDaemon struct {
	Profile string
	Detail  string
}

// busyDaemons reports which of this policy's daemons are working right now.
// Replacing a binary under a daemon that is driving a browser loses whatever
// the agent was in the middle of, and the agent sees a transport error it
// cannot distinguish from a crash.
func busyDaemons(policy profilepolicy.Policy, client *http.Client) []busyDaemon {
	var busy []busyDaemon
	// Profiles that pin no HTTP address share the default port, so probing per
	// profile asks the same daemon the same question N times and can refuse in
	// the name of a profile that daemon is not serving.
	probed := map[string]bool{}
	for _, profile := range policy.Profiles {
		url := discovery.HTTPURL(profile)
		if probed[url] {
			continue
		}
		probed[url] = true
		health, err := probeDaemonHealth(client, url)
		if err != nil {
			// Not answering: there is no in-flight work to interrupt.
			continue
		}
		// A daemon knows which profile it serves; the policy entry that happens
		// to name this port only knows which one it asked for.
		name := profile.Name
		if health.Identity.Profile != "" {
			name = health.Identity.Profile
		}
		if health.TabLeases.InFlight > 0 {
			busy = append(busy, busyDaemon{
				Profile: name,
				Detail: fmt.Sprintf("%d request(s) in flight across %d leased tab(s)",
					health.TabLeases.InFlight, health.TabLeases.ActiveTabs),
			})
			continue
		}
		if !profile.ExtensionBridgeAllowed {
			continue
		}
		status, err := probeBridgeStatus(client, discovery.WSAddr(profile))
		if err != nil {
			continue
		}
		if pending := status.Inflight + status.Queued + status.Pending; pending > 0 {
			busy = append(busy, busyDaemon{
				Profile: name,
				Detail:  fmt.Sprintf("%d bridge operation(s) in flight or queued", pending),
			})
		}
	}
	return busy
}

// restartServices restarts every per-user daemon running this install's brwd,
// so the upgraded binary is the one actually running afterwards. That is the
// unit `brwctl setup` wrote for each profile, plus any unit under another label
// whose program is this install's brwd: a hand-made unit is left as written,
// but restarting it is the only way its daemon picks up the new build.
//
// A profile with no unit is passed over in silence: a direct-CDP profile has no
// daemon of its own, and a stdio daemon is launched by the agent client, so a
// line per profile telling the operator to start something by hand is noise
// around the one case that is real — no unit anywhere.
func restartServices(opts upgradeOptions, policy profilepolicy.Policy) ([]string, []string) {
	var labels []string
	seen := map[string]bool{}
	add := func(label string) {
		if !seen[label] {
			seen[label] = true
			labels = append(labels, label)
		}
	}
	for _, profile := range policy.Profiles {
		params := setup.ServiceParams{GOOS: opts.goos, Profile: profile.Name, Home: opts.home}
		if _, err := os.Stat(params.UnitPath()); err == nil {
			add(params.Label())
		}
	}
	brwd := filepath.Join(opts.appDir, "bin", "brwd")
	for _, unit := range brwdServiceUnits(opts.goos, opts.home, brwd) {
		add(unit.Label)
	}

	var restarted, manual []string
	for _, label := range labels {
		args := serviceRestartArgsForLabel(opts.goos, label)
		if out, err := opts.runner.run(args[0], args[1:]...); err != nil {
			manual = append(manual, fmt.Sprintf("%s failed (%v %s); run it yourself", setup.Command(args), err, out))
			continue
		}
		restarted = append(restarted, label)
	}
	if len(labels) == 0 && len(policy.Profiles) > 0 {
		manual = append(manual, "No profile has an installed service unit, so nothing was restarted: restart the daemon yourself (or the agent client that launches it) to run the new binary.")
	}
	return restarted, manual
}

func serviceRestartArgs(goos string, params setup.ServiceParams) []string {
	return serviceRestartArgsForLabel(goos, params.Label())
}
