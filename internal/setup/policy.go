// Package setup holds the pure, testable half of `brwctl setup`: deriving a working profile policy from nothing, rendering a per-user background service, locating the bundled agent skill, and the read-only environment probes doctor and setup share.
package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/profilepolicy"
)

// Transport lanes as a human names them on the command line.
const (
	TransportBridge    = "bridge"
	TransportDirectCDP = "direct-cdp"
	// TransportHeadless is direct CDP on a brw-owned headless profile that is never signed in: the lane for quick public browsing.
	TransportHeadless = "headless"
)

// The two browsers with special standing: Chrome is the fallback when nothing has been run, and Chromium is the one brw champions.
const (
	BrowserChrome   = "chrome"
	BrowserChromium = "chromium"
)

// LocalTransportName is the policy transport a local install runs over.
const LocalTransportName = "local"

// DefaultHTTPPort is brwd's own default control port.
const DefaultHTTPPort = 17310

// PolicyRequest is everything setup needs to author or extend a policy.
type PolicyRequest struct {
	Workspace string
	Profile   string
	Browser   string
	Transport string
	// ProfileDirectory is the browser profile directory inside the user data directory.
	ProfileDirectory string
	// UserDataDir overrides the table, which is how a Chromium build brw has no entry for is bound without a code change.
	UserDataDir string
	// BRWDPath is written as the local stdio transport's command.
	BRWDPath string
	// MCPClient is the agent client the operator named on the command line.
	MCPClient string
	HTTPPort  int
	Home      string
	GOOS      string
}

// Change is one finding about a policy: either an edit setup will make, or a statement that the policy already satisfies that requirement.
type Change struct {
	Kind   string
	Detail string
	Edit   bool
}

// DefaultProfileName is the profile a request gets when the operator names none.
func DefaultProfileName(browser, transport string) string {
	switch transport {
	case TransportDirectCDP:
		return browser + "-agent"
	case TransportHeadless:
		return browser + "-headless"
	}
	return browser + "-profile"
}

// DefaultWorkspaceName keeps the workspace label tied to the profile, so a second setup run for another browser adds a binding instead of fighting over one workspace's default_profile.
func DefaultWorkspaceName(browser, transport string) string {
	return "brw-" + DefaultProfileName(browser, transport)
}

func (r PolicyRequest) userDataDir() string {
	if r.UserDataDir != "" {
		return r.UserDataDir
	}
	return BrowserUserDataDir(r.GOOS, r.Browser)
}

// NewProfile builds the profile a first-time user needs for one lane.
func NewProfile(req PolicyRequest) profilepolicy.Profile {
	port := req.HTTPPort
	if port <= 0 {
		port = DefaultHTTPPort
	}
	if req.Transport == TransportHeadless {
		return profilepolicy.Profile{
			Name:                   req.Profile,
			Description:            "brw-owned headless " + BrowserDisplayName(req.Browser) + " for quick public browsing. Never signed in and separate from every human profile.",
			Kind:                   req.Browser,
			UserDataDir:            "~/.brw/" + req.Browser + "-headless",
			DirectCDPAllowed:       true,
			ExtensionBridgeAllowed: false,
			Headless:               true,
			BridgeHTTPAddr:         "127.0.0.1:" + strconv.Itoa(port),
		}
	}
	if req.Transport == TransportDirectCDP {
		return profilepolicy.Profile{
			Name:                   req.Profile,
			Description:            "brw-owned " + BrowserDisplayName(req.Browser) + " driven over direct CDP. Sign in once with `brwd --workspace " + req.Workspace + " --login`; the session persists in this profile directory.",
			Kind:                   req.Browser,
			UserDataDir:            "~/.brw/" + req.Browser + "-agent",
			DirectCDPAllowed:       true,
			ExtensionBridgeAllowed: false,
			BridgeHTTPAddr:         "127.0.0.1:" + strconv.Itoa(port),
		}
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

// Merge folds the request into an existing policy and reports every edit.
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

	if req.MCPClient != "" && merged.MCPClient != req.MCPClient {
		merged.MCPClient = req.MCPClient
		changes = append(changes, Change{Kind: "mcp_client", Detail: fmt.Sprintf("record agent client choice %q", req.MCPClient), Edit: true})
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

	if len(binding.AllowedProfiles) > 0 && !slices.Contains(binding.AllowedProfiles, req.Profile) {
		binding.AllowedProfiles = append(binding.AllowedProfiles, req.Profile)
		changes = append(changes, Change{Kind: "workspace", Detail: fmt.Sprintf("allow profile %q for workspace %q", req.Profile, req.Workspace), Edit: true})
		filled = true
	}
	if len(binding.AllowedTransports) > 0 && !slices.Contains(binding.AllowedTransports, LocalTransportName) {
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
	switch transport {
	case TransportDirectCDP:
		return "direct CDP"
	case TransportHeadless:
		return "headless direct CDP"
	}
	return "extension bridge"
}

// Changed reports whether any change in the list actually edits the policy, so a caller can skip the backup-and-write when a re-run has nothing to do.
func Changed(changes []Change) bool {
	for _, change := range changes {
		if change.Edit {
			return true
		}
	}
	return false
}

// DefaultPolicyPath is where setup writes a policy when the operator names no path and none of the standard locations already holds one.
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

// EncodePolicy renders a policy the way a human would have written it, so a merged file stays reviewable in a diff and in git.
func EncodePolicy(policy profilepolicy.Policy) ([]byte, error) {
	data, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// BackupPath is where an existing policy is copied before setup edits it.
func BackupPath(path string, now time.Time) string {
	return path + ".bak." + now.UTC().Format("20060102T150405Z")
}

// WritePolicy backs up any existing file, then replaces it atomically at 0600.
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

// LoadPolicyFile reads a policy without profilepolicy's discovery or path expansion.
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

// ProfileDirectories lists the browser profile directories that exist inside a user data directory, Default first and then numbered profiles in order.
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
	return append(append(defaultProfile, numbered...), others...)
}

// PickProfileDirectory chooses the profile directory a generated policy binds to.
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
	clone := policy
	clone.WorkspaceBindings = slices.Clone(policy.WorkspaceBindings)
	clone.Profiles = slices.Clone(policy.Profiles)
	clone.Transports = slices.Clone(policy.Transports)
	for i := range clone.WorkspaceBindings {
		clone.WorkspaceBindings[i].AllowedProfiles = slices.Clone(clone.WorkspaceBindings[i].AllowedProfiles)
		clone.WorkspaceBindings[i].AllowedTransports = slices.Clone(clone.WorkspaceBindings[i].AllowedTransports)
	}
	for i := range clone.Transports {
		clone.Transports[i].CommandArgs = slices.Clone(clone.Transports[i].CommandArgs)
	}
	return clone
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
