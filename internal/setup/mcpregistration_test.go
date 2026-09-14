package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadMCPServers(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name        string
		content     string
		write       bool
		wantFound   bool
		wantErr     bool
		wantCommand string
	}{
		{
			name:        "a registered brw server",
			write:       true,
			content:     `{"mcpServers":{"brw":{"command":"/opt/brw/bin/brwd","args":["--bridge","--mcp"],"env":{"BRW_PROFILE":"chrome-profile"}}}}`,
			wantFound:   true,
			wantCommand: "/opt/brw/bin/brwd",
		},
		{
			name:      "a config that registers nothing",
			write:     true,
			content:   `{"mcpServers":{}}`,
			wantFound: true,
		},
		{
			name:      "a config with unrelated keys",
			write:     true,
			content:   `{"theme":"dark"}`,
			wantFound: true,
		},
		{
			// A client that was never installed is not a broken config.
			name: "no config file at all",
		},
		{
			name:      "a config that is not JSON",
			write:     true,
			content:   "{",
			wantFound: true,
			wantErr:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".json")
			if tc.write {
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			servers, found, err := ReadMCPServers(path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}
			if tc.wantCommand == "" {
				return
			}
			entry, ok := servers["brw"]
			if !ok {
				t.Fatalf("servers = %+v, want a brw entry", servers)
			}
			if entry.Command != tc.wantCommand {
				t.Fatalf("command = %q, want %q", entry.Command, tc.wantCommand)
			}
			if entry.Name != "brw" || len(entry.Args) != 2 || entry.Env["BRW_PROFILE"] != "chrome-profile" {
				t.Fatalf("entry = %+v", entry)
			}
		})
	}
}
