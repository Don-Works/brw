package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// ClaudeInChromeWarning is the stable name of the doctor warning raised when
// Claude Code's own Chrome integration is on. Consumers match on the name, so
// it never changes with the wording.
const ClaudeInChromeWarning = "claude_in_chrome_enabled"

// ClaudeInChromeState is what a read-only look at ~/.claude.json says about
// Claude Code's built-in Chrome integration. Enabled is false whenever the file
// is absent, unreadable or not JSON: a missing signal is not a warning.
type ClaudeInChromeState struct {
	Path    string   `json:"path,omitempty"`
	Enabled bool     `json:"enabled"`
	Signals []string `json:"signals,omitempty"`
}

// ClaudeConfigPath is the file Claude Code keeps its user state in. Claude Code
// rewrites this file while it runs, so brw only ever reads it.
func ClaudeConfigPath(home string) string {
	return filepath.Join(home, ".claude.json")
}

// DetectClaudeInChrome reports whether Claude Code's Chrome integration is
// active. Both brw and Claude-in-Chrome expose browser tools, and an agent with
// both loaded can drive the browser brw is not managing.
//
// claudeInChromeDefaultEnabled is authoritative when present, including when it
// is false — a user who ran /chrome and turned it off must not be nagged. Only
// when the key is absent does the onboarding-plus-extension pair stand in for
// it, which is the shape of a config written by an older Claude Code.
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
