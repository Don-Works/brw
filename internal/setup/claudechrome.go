package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// ClaudeInChromeWarning is the stable name of the doctor warning raised when Claude Code's own Chrome integration is on.
const ClaudeInChromeWarning = "claude_in_chrome_enabled"

// ClaudeInChromeState is what a read-only look at ~/.claude.json says about Claude Code's built-in Chrome integration.
type ClaudeInChromeState struct {
	Path    string   `json:"path,omitempty"`
	Enabled bool     `json:"enabled"`
	Signals []string `json:"signals,omitempty"`
}

// ClaudeConfigPath is the file Claude Code keeps its user state in.
func ClaudeConfigPath(home string) string {
	return filepath.Join(home, ".claude.json")
}

// DetectClaudeInChrome reports whether Claude Code's Chrome integration is active.
func DetectClaudeInChrome(path string) ClaudeInChromeState {
	state := ClaudeInChromeState{Path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		return ClaudeInChromeState{}
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(data, &config); err != nil {
		return ClaudeInChromeState{}
	}
	if value, ok := readBool(config, "claudeInChromeDefaultEnabled"); ok {
		state.Enabled = value
		if value {
			state.Signals = []string{"claudeInChromeDefaultEnabled"}
		}
		return state
	}
	onboarded, hasOnboarded := readBool(config, "hasCompletedClaudeInChromeOnboarding")
	installed, hasInstalled := readBool(config, "cachedChromeExtensionInstalled")
	if hasOnboarded && hasInstalled && onboarded && installed {
		state.Enabled = true
		state.Signals = []string{"hasCompletedClaudeInChromeOnboarding", "cachedChromeExtensionInstalled"}
	}
	return state
}

func readBool(config map[string]json.RawMessage, key string) (bool, bool) {
	raw, ok := config[key]
	if !ok {
		return false, false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, false
	}
	return value, true
}
