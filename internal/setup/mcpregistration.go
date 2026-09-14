package setup

import (
	"encoding/json"
	"os"
)

// MCPServerEntry is one MCP server as an agent client's config records it.
// Command is the only field that answers the question doctor asks — whether a
// registration still launches an installed brw — because a client execs that
// path verbatim and never re-resolves it.
type MCPServerEntry struct {
	Name    string            `json:"name"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// ReadMCPServers reads the mcpServers block out of an agent client config in
// the Claude Code JSON shape. found is false only when the file does not exist:
// a client that was never installed is a different report from a config that is
// present and says nothing about brw.
//
// This is read-only by design. Claude Code rewrites its config while it runs,
// so brw registers through the client's own CLI and never edits the file.
func ReadMCPServers(path string) (servers map[string]MCPServerEntry, found bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var config struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, true, err
	}
	servers = make(map[string]MCPServerEntry, len(config.MCPServers))
	for name, entry := range config.MCPServers {
		servers[name] = MCPServerEntry{Name: name, Command: entry.Command, Args: entry.Args, Env: entry.Env}
	}
	return servers, true, nil
}
