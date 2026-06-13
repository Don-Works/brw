package profilepolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const DefaultBridgeExtensionID = "hkomepfdcddgepbdalomhabiphokllkd"

type Policy struct {
	Profiles   []Profile   `json:"profiles"`
	Transports []Transport `json:"transports,omitempty"`
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
	DevToolsMCPAllowed     bool   `json:"devtools_mcp_allowed,omitempty"`
	DevToolsMCPMode        string `json:"devtools_mcp_mode,omitempty"`
}

type Transport struct {
	Name             string `json:"name"`
	Kind             string `json:"kind"`
	Host             string `json:"host,omitempty"`
	User             string `json:"user,omitempty"`
	PreferredNetwork string `json:"preferred_network,omitempty"`
	AppDir           string `json:"app_dir,omitempty"`
	Command          string `json:"command,omitempty"`
	Bind             string `json:"bind,omitempty"`
	Expose           string `json:"expose,omitempty"`
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
		candidate := filepath.Join(dir, ".mcplexer", "config", "browser-profiles.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		agentBrowserParent := filepath.Join(dir, "..", ".mcplexer", "config", "browser-profiles.json")
		if _, err := os.Stat(agentBrowserParent); err == nil {
			return filepath.Clean(agentBrowserParent), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("browser profile policy not found; expected .mcplexer/config/browser-profiles.json")
}

func (p Policy) Find(name string) (Profile, error) {
	for _, profile := range p.Profiles {
		if profile.Name == name {
			return profile, nil
		}
	}
	return Profile{}, fmt.Errorf("profile %q is not allowed by workspace policy", name)
}

func (p Policy) FindTransport(name string) (Transport, error) {
	for _, transport := range p.Transports {
		if transport.Name == name {
			return transport, nil
		}
	}
	return Transport{}, fmt.Errorf("transport %q is not allowed by workspace policy", name)
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
