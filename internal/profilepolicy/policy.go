package profilepolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultBridgeExtensionID is the stable id of the published brw Chrome
// extension. It is derived from the public key pinned in extension/manifest.json
// (the "key" field), so a load-unpacked dev install and the Chrome Web Store
// build share this one id. An unconfigured bridge pins to it rather than the
// chrome-extension://* wildcard. An explicit profile bridge_extension_id still
// overrides it.
const DefaultBridgeExtensionID = "amocjcgddnoakjijfggdpnefdnboilpe"

type Policy struct {
	WorkspaceBindings []WorkspaceBinding `json:"workspace_bindings,omitempty"`
	Profiles          []Profile          `json:"profiles"`
	Transports        []Transport        `json:"transports,omitempty"`
}

type WorkspaceBinding struct {
	Workspace         string   `json:"workspace"`
	DefaultProfile    string   `json:"default_profile,omitempty"`
	AllowedProfiles   []string `json:"allowed_profiles,omitempty"`
	DefaultTransport  string   `json:"default_transport,omitempty"`
	AllowedTransports []string `json:"allowed_transports,omitempty"`
}

type Profile struct {
	Name                   string `json:"name"`
	Description            string `json:"description,omitempty"`
	Kind                   string `json:"kind,omitempty"`
	UserDataDir            string `json:"user_data_dir"`
	ProfileDirectory       string `json:"profile_directory,omitempty"`
	DirectCDPAllowed       bool   `json:"direct_cdp_allowed"`
	ExtensionBridgeAllowed bool   `json:"extension_bridge_allowed"`
	BridgeExtensionID      string `json:"bridge_extension_id,omitempty"`
	BridgeInstallMode      string `json:"bridge_install_mode,omitempty"`
	BridgeHTTPAddr         string `json:"bridge_http_addr,omitempty"`
	BridgeWSAddr           string `json:"bridge_ws_addr,omitempty"`
	DevToolsMCPAllowed     bool   `json:"devtools_mcp_allowed,omitempty"`
	DevToolsMCPMode        string `json:"devtools_mcp_mode,omitempty"`
}

type Transport struct {
	Name             string   `json:"name"`
	Kind             string   `json:"kind"`
	Host             string   `json:"host,omitempty"`
	User             string   `json:"user,omitempty"`
	PreferredNetwork string   `json:"preferred_network,omitempty"`
	AppDir           string   `json:"app_dir,omitempty"`
	Command          string   `json:"command,omitempty"`
	CommandArgs      []string `json:"command_args,omitempty"`
	Bind             string   `json:"bind,omitempty"`
	Expose           string   `json:"expose,omitempty"`
}

func Load(path string) (Policy, error) {
	if path == "" {
		var err error
		path, err = Discover("")
		if err != nil {
			return Policy{}, err
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Policy{}, err
	}
	var policy Policy
	if err := json.Unmarshal(data, &policy); err != nil {
		return Policy{}, err
	}
	for i := range policy.Profiles {
		policy.Profiles[i].UserDataDir = expandPath(policy.Profiles[i].UserDataDir)
	}
	return policy, nil
}

func Discover(start string) (string, error) {
	var candidates []string
	if configDir, err := os.UserConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(configDir, "brw", "browser-profiles.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, "Library", "Application Support", "brw", "config", "browser-profiles.json"),
			filepath.Join(home, ".local", "share", "brw", "config", "browser-profiles.json"),
		)
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}

	if start == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		for _, rel := range []string{
			filepath.Join("config", "browser-profiles.json"),
			filepath.Join("brw", "config", "browser-profiles.json"),
		} {
			candidate := filepath.Join(dir, rel)
			if _, err := os.Stat(candidate); err == nil {
				return filepath.Clean(candidate), nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("browser profile policy not found; set BRW_PROFILE_POLICY or create ~/.config/brw/browser-profiles.json")
}

func (p Policy) Find(name string) (Profile, error) {
	for _, profile := range p.Profiles {
		if profile.Name == name {
			return profile, nil
		}
	}
	return Profile{}, fmt.Errorf("profile %q is not allowed by workspace policy", name)
}

func (p Policy) ResolveProfile(workspace, name string) (Profile, error) {
	binding, bound := p.FindWorkspace(workspace)
	if !bound && workspace != "" && len(p.WorkspaceBindings) > 0 {
		// The policy declares workspace authority (it has bindings) but this
		// non-empty workspace matches none of them. Fail CLOSED — otherwise an
		// unrecognised workspace label silently bypasses every AllowedProfiles
		// list and can select any profile (including another tenant's).
		return Profile{}, fmt.Errorf("workspace %q is not defined in the profile policy; refusing to resolve a profile for an unrecognised workspace", workspace)
	}
	if name == "" && bound {
		name = binding.DefaultProfile
	}
	if name == "" {
		return Profile{}, errors.New("--profile is required when workspace has no default_profile")
	}
	if bound && len(binding.AllowedProfiles) > 0 && !contains(binding.AllowedProfiles, name) {
		return Profile{}, fmt.Errorf("profile %q is not allowed for workspace %q", name, workspace)
	}
	return p.Find(name)
}

func (p Policy) FindTransport(name string) (Transport, error) {
	for _, transport := range p.Transports {
		if transport.Name == name {
			return transport, nil
		}
	}
	return Transport{}, fmt.Errorf("transport %q is not allowed by workspace policy", name)
}

func (p Policy) ResolveTransport(workspace, name string) (Transport, error) {
	binding, bound := p.FindWorkspace(workspace)
	if !bound && workspace != "" && len(p.WorkspaceBindings) > 0 {
		// Fail closed for an unrecognised workspace when bindings exist — see
		// ResolveProfile. An unknown workspace must not pick any transport
		// (e.g. an SSH transport to an arbitrary host).
		return Transport{}, fmt.Errorf("workspace %q is not defined in the profile policy; refusing to resolve a transport for an unrecognised workspace", workspace)
	}
	if name == "" && bound {
		name = binding.DefaultTransport
	}
	if name == "" {
		return Transport{}, errors.New("--transport is required when workspace has no default_transport")
	}
	if bound && len(binding.AllowedTransports) > 0 && !contains(binding.AllowedTransports, name) {
		return Transport{}, fmt.Errorf("transport %q is not allowed for workspace %q", name, workspace)
	}
	return p.FindTransport(name)
}

func (p Policy) FindWorkspace(name string) (WorkspaceBinding, bool) {
	if name == "" {
		return WorkspaceBinding{}, false
	}
	for _, binding := range p.WorkspaceBindings {
		if binding.Workspace == name {
			return binding, true
		}
	}
	return WorkspaceBinding{}, false
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func expandPath(path string) string {
	if path == "" {
		return ""
	}
	if path == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return os.ExpandEnv(path)
}
