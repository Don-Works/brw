// Package setup holds the pure, testable half of `brwctl setup`: deriving a
// working profile policy from nothing, rendering a per-user background service,
// locating the bundled agent skill, and the read-only environment probes doctor
// and setup share. The command layer in cmd/brwctl performs the side effects.
package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/profilepolicy"
)

// Transport lanes as a human names them on the command line. The policy file's
// own "transports" array means something different (a stdio or ssh-stdio hop to
// a brwd), so these two words never appear there.
const (
	TransportBridge    = "bridge"
	TransportDirectCDP = "direct-cdp"
)

// The two browsers with special standing: Chrome is the fallback when nothing
// has been run, and Chromium is the one brw champions. Every other browser is
// data in browsers.go and needs no constant.
const (
	BrowserChrome   = "chrome"
	BrowserChromium = "chromium"
)

// LocalTransportName is the policy transport a local install runs over. It is
// the name `brwctl mcp-config` falls back to when no workspace binding names
// one, so writing it is what makes a zero-config machine resolvable.
const LocalTransportName = "local"

// DefaultHTTPPort is brwd's own default control port. The bridge WebSocket
// listens on the next port up, matching brwd's --bridge-addr default, so a
// hand-run `brwd --bridge` and a serviced one land on the same pair.
const DefaultHTTPPort = 17310

// PolicyRequest is everything setup needs to author or extend a policy. GOOS
// and Home are parameters rather than package lookups so the whole derivation
// is testable for every platform from any platform.
type PolicyRequest struct {
	Workspace string
	Profile   string
	Browser   string
	Transport string
	// ProfileDirectory is the browser profile directory inside the user data
	// directory. Empty means Default, the one Chrome creates on first launch.
	ProfileDirectory string
	// UserDataDir overrides the table, which is how a Chromium build brw has no
	// entry for is bound without a code change. Empty means look Browser up.
	UserDataDir string
	// BRWDPath is written as the local stdio transport's command. An absolute
	// path is what makes the MCP server start under a client that does not
	// inherit the user's PATH; "brwd" is the degraded fallback.
	BRWDPath string
	HTTPPort int
	Home     string
	GOOS     string
}

// Change is one finding about a policy: either an edit setup will make, or a
// statement that the policy already satisfies that requirement. Edit is what
// callers branch on, so the wording of Detail stays free to change.
type Change struct {
	Kind   string
	Detail string
	Edit   bool
}

// DefaultProfileName is the profile a request gets when the operator names
// none. The browser and lane are both in the name because a machine ends up
// with one profile per (browser, lane) pair and they must not collide.
func DefaultProfileName(browser, transport string) string {
	if transport == TransportDirectCDP {
		return browser + "-agent"
	}
	return browser + "-profile"
}

// DefaultWorkspaceName keeps the workspace label tied to the profile, so a
// second setup run for another browser adds a binding instead of fighting over
// one workspace's default_profile.
func DefaultWorkspaceName(browser, transport string) string {
	return "brw-" + DefaultProfileName(browser, transport)
}

// userDataDir prefers what the operator passed over what the table knows.
func (r PolicyRequest) userDataDir() string {
	if r.UserDataDir != "" {
		return r.UserDataDir
	}
	return BrowserUserDataDir(r.GOOS, r.Browser)
}

// NewProfile builds the profile a first-time user needs for one lane. A bridge
// profile points at the browser the human already uses and forbids direct CDP:
// a second Chrome on a live profile directory corrupts it. A direct-CDP profile
// gets its own brw-owned directory for the same reason.
func NewProfile(req PolicyRequest) profilepolicy.Profile {
	if req.Transport == TransportDirectCDP {
		return profilepolicy.Profile{
			Name:                   req.Profile,
			Description:            "brw-owned " + BrowserDisplayName(req.Browser) + " driven over direct CDP. Sign in once with `brwd --workspace " + req.Workspace + " --login`; the session persists in this profile directory.",
			Kind:                   req.Browser,
			UserDataDir:            "~/.brw/" + req.Browser + "-agent",
			DirectCDPAllowed:       true,
			ExtensionBridgeAllowed: false,
		}
	}
	port := req.HTTPPort
	if port <= 0 {
		port = DefaultHTTPPort
	}
	profileDirectory := req.ProfileDirectory
	if profileDirectory == "" {
		profileDirectory = "Default"
	}
	return profilepolicy.Profile{
		Name:                   req.Profile,
		Description:            BrowserDisplayName(req.Browser) + " " + profileDirectory + " profile, bridged through the brw extension.",
		Kind:                   req.Browser,
		UserDataDir:            req.userDataDir(),
		ProfileDirectory:       profileDirectory,
		DirectCDPAllowed:       false,
		ExtensionBridgeAllowed: true,
		BridgeExtensionID:      profilepolicy.DefaultBridgeExtensionID,
		BridgeInstallMode:      "load_unpacked",
		BridgeHTTPAddr:         fmt.Sprintf("127.0.0.1:%d", port),
		BridgeWSAddr:           fmt.Sprintf("127.0.0.1:%d", port+1),
	}
}

// Merge folds the request into an existing policy and reports every edit. It
// only ever appends entries or fills fields that are empty; an entry the
// operator already wrote is left exactly as it is, so re-running setup on a
// configured machine is a no-op that says so.
//
// Filling an empty default_transport matters as much as adding the transport
// itself: a binding with neither is the state that makes `brwctl mcp-config`
// fail with "--transport is required when workspace has no default_transport",
// which a first-time user has no way to diagnose.
func Merge(existing profilepolicy.Policy, req PolicyRequest) (profilepolicy.Policy, []Change) {
	merged := clonePolicy(existing)
	var changes []Change

	brwd := req.BRWDPath
	if brwd == "" {
		brwd = "brwd"
	}
	if _, err := merged.FindTransport(LocalTransportName); err != nil {
		merged.Transports = append(merged.Transports, profilepolicy.Transport{
			Name:    LocalTransportName,
			Kind:    "stdio",
			Command: brwd,
		})
		changes = append(changes, Change{Kind: "transport", Detail: fmt.Sprintf("add stdio transport %q running %s", LocalTransportName, brwd), Edit: true})
	} else {
		changes = append(changes, Change{Kind: "transport", Detail: fmt.Sprintf("transport %q already defined", LocalTransportName)})
	}

	if _, err := merged.Find(req.Profile); err != nil {
		merged.Profiles = append(merged.Profiles, NewProfile(req))
		changes = append(changes, Change{Kind: "profile", Detail: fmt.Sprintf("add profile %q (%s, %s)", req.Profile, BrowserDisplayName(req.Browser), laneLabel(req.Transport)), Edit: true})
	} else {
		changes = append(changes, Change{Kind: "profile", Detail: fmt.Sprintf("profile %q already defined", req.Profile)})
	}

	index := -1
	for i := range merged.WorkspaceBindings {
		if merged.WorkspaceBindings[i].Workspace == req.Workspace {
			index = i
			break
		}
	}
	if index < 0 {
		merged.WorkspaceBindings = append(merged.WorkspaceBindings, profilepolicy.WorkspaceBinding{
			Workspace:         req.Workspace,
			DefaultProfile:    req.Profile,
			AllowedProfiles:   []string{req.Profile},
			DefaultTransport:  LocalTransportName,
			AllowedTransports: []string{LocalTransportName},
		})
		changes = append(changes, Change{Kind: "workspace", Detail: fmt.Sprintf("bind workspace %q to profile %q over transport %q", req.Workspace, req.Profile, LocalTransportName), Edit: true})
		return merged, changes
	}

	binding := &merged.WorkspaceBindings[index]
	filled := false
	if binding.DefaultProfile == "" {
		binding.DefaultProfile = req.Profile
		changes = append(changes, Change{Kind: "workspace", Detail: fmt.Sprintf("set workspace %q default_profile to %q", req.Workspace, req.Profile), Edit: true})
		filled = true
	}
	if binding.DefaultTransport == "" {
		binding.DefaultTransport = LocalTransportName
		changes = append(changes, Change{Kind: "workspace", Detail: fmt.Sprintf("set workspace %q default_transport to %q", req.Workspace, LocalTransportName), Edit: true})
		filled = true
	}
	// An empty allow-list means "anything in the policy", so only a non-empty
	// one needs extending; adding to an empty list would silently narrow it.
	if len(binding.AllowedProfiles) > 0 && !containsString(binding.AllowedProfiles, req.Profile) {
		binding.AllowedProfiles = append(binding.AllowedProfiles, req.Profile)
		changes = append(changes, Change{Kind: "workspace", Detail: fmt.Sprintf("allow profile %q for workspace %q", req.Profile, req.Workspace), Edit: true})
		filled = true
	}
	if len(binding.AllowedTransports) > 0 && !containsString(binding.AllowedTransports, LocalTransportName) {
		binding.AllowedTransports = append(binding.AllowedTransports, LocalTransportName)
		changes = append(changes, Change{Kind: "workspace", Detail: fmt.Sprintf("allow transport %q for workspace %q", LocalTransportName, req.Workspace), Edit: true})
		filled = true
	}
	if !filled {
		changes = append(changes, Change{Kind: "workspace", Detail: fmt.Sprintf("workspace %q already bound to profile %q over transport %q", req.Workspace, binding.DefaultProfile, binding.DefaultTransport)})
	}
	return merged, changes
}

func laneLabel(transport string) string {
	if transport == TransportDirectCDP {
		return "direct CDP"
	}
	return "extension bridge"
}

// Changed reports whether any change in the list actually edits the policy, so
// a caller can skip the backup-and-write when a re-run has nothing to do.
func Changed(changes []Change) bool {
	for _, change := range changes {
		if change.Edit {
			return true
		}
	}
	return false
}

// DefaultPolicyPath is where setup writes a policy when the operator names no
// path and none of the standard locations already holds one. Unlike
// profilepolicy.Discover it never walks up from the working directory: setup
// must not write into whatever checkout the operator happens to be standing in.
func DefaultPolicyPath(home string) (string, error) {
	for _, candidate := range userPolicyCandidates(home) {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	candidates := userPolicyCandidates(home)
	if len(candidates) == 0 {
		return "", fmt.Errorf("cannot determine a user config directory for the profile policy")
	}
	return candidates[0], nil
}

func userPolicyCandidates(home string) []string {
	var candidates []string
	if configDir, err := os.UserConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(configDir, "brw", "browser-profiles.json"))
	}
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".config", "brw", "browser-profiles.json"),
			filepath.Join(home, "Library", "Application Support", "brw", "config", "browser-profiles.json"),
			filepath.Join(home, ".local", "share", "brw", "config", "browser-profiles.json"),
		)
	}
	return dedupeStrings(candidates)
}

// EncodePolicy renders a policy the way a human would have written it, so a
// merged file stays reviewable in a diff and in git.
func EncodePolicy(policy profilepolicy.Policy) ([]byte, error) {
	data, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// BackupPath is where an existing policy is copied before setup edits it. The
// timestamp is in the name so repeated runs never overwrite an earlier backup.
func BackupPath(path string, now time.Time) string {
	return path + ".bak." + now.UTC().Format("20060102T150405Z")
}

// WritePolicy backs up any existing file, then replaces it atomically at 0600.
// The policy names browser profile directories, so it is owner-only.
func WritePolicy(path string, policy profilepolicy.Policy, now time.Time) (backup string, err error) {
	data, err := EncodePolicy(policy)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if existing, readErr := os.ReadFile(path); readErr == nil {
		backup = BackupPath(path, now)
		if err := os.WriteFile(backup, existing, 0o600); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(readErr) {
		return "", readErr
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return backup, err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return backup, err
	}
	return backup, nil
}

// LoadPolicyFile reads a policy without profilepolicy's discovery or path
// expansion. Setup edits the file as written, so expanding ~/ on load would
// bake this machine's home directory into the saved policy.
func LoadPolicyFile(path string) (profilepolicy.Policy, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return profilepolicy.Policy{}, false, nil
	}
	if err != nil {
		return profilepolicy.Policy{}, false, err
	}
	var policy profilepolicy.Policy
	if err := json.Unmarshal(data, &policy); err != nil {
		return profilepolicy.Policy{}, true, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	return policy, true, nil
}

// ProfileDirectories lists the browser profile directories that exist inside a
// user data directory, Default first and then numbered profiles in order. A
// directory counts as a profile only when it holds a Preferences file, which is
// what Chrome writes when a profile is first created.
func ProfileDirectories(userDataDir string) []string {
	entries, err := os.ReadDir(userDataDir)
	if err != nil {
		return nil
	}
	var defaultProfile, numbered, others []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Chrome's own scaffolding directories carry a Preferences file but are
		// not profiles a human signs into, and a bridge bound to one attaches to
		// nothing.
		if entry.Name() == "System Profile" || entry.Name() == "Guest Profile" {
			continue
		}
		if _, err := os.Stat(filepath.Join(userDataDir, entry.Name(), "Preferences")); err != nil {
			continue
		}
		switch {
		case entry.Name() == "Default":
			defaultProfile = append(defaultProfile, entry.Name())
		case strings.HasPrefix(entry.Name(), "Profile "):
			numbered = append(numbered, entry.Name())
		default:
			others = append(others, entry.Name())
		}
	}
	sort.Slice(numbered, func(i, j int) bool {
		left, leftErr := strconv.Atoi(strings.TrimPrefix(numbered[i], "Profile "))
		right, rightErr := strconv.Atoi(strings.TrimPrefix(numbered[j], "Profile "))
		if leftErr != nil || rightErr != nil {
			return numbered[i] < numbered[j]
		}
		return left < right
	})
	sort.Strings(others)
	return append(append(defaultProfile, numbered...), others...)
}

// PickProfileDirectory chooses the profile directory a generated policy binds
// to. Pointing at a directory that does not exist is the difference between a
// working bridge and a doctor failure a first-time user cannot act on, so an
// existing profile always wins over the name Chrome would create.
func PickProfileDirectory(userDataDir string) string {
	if found := ProfileDirectories(userDataDir); len(found) > 0 {
		return found[0]
	}
	return "Default"
}

// HasProfiles reports whether a browser has ever been run on this machine.
func HasProfiles(userDataDir string) bool {
	return len(ProfileDirectories(userDataDir)) > 0
}

func clonePolicy(policy profilepolicy.Policy) profilepolicy.Policy {
	clone := profilepolicy.Policy{
		WorkspaceBindings: append([]profilepolicy.WorkspaceBinding(nil), policy.WorkspaceBindings...),
		Profiles:          append([]profilepolicy.Profile(nil), policy.Profiles...),
		Transports:        append([]profilepolicy.Transport(nil), policy.Transports...),
	}
	for i := range clone.WorkspaceBindings {
		clone.WorkspaceBindings[i].AllowedProfiles = append([]string(nil), clone.WorkspaceBindings[i].AllowedProfiles...)
		clone.WorkspaceBindings[i].AllowedTransports = append([]string(nil), clone.WorkspaceBindings[i].AllowedTransports...)
	}
	for i := range clone.Transports {
		clone.Transports[i].CommandArgs = append([]string(nil), clone.Transports[i].CommandArgs...)
	}
	return clone
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func dedupeStrings(values []string) []string {
	seen := map[string]bool{}
	out := values[:0]
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
