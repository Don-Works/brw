package brwidentity

import "fmt"

// Identity is the runtime identity exposed by a brw HTTP daemon. MCP wrappers
// use it to prove that an upstream loopback daemon is the workspace/profile they
// were launched for, rather than trusting a port number or namespace label.
type Identity struct {
	Workspace        string `json:"workspace,omitempty"`
	Profile          string `json:"profile,omitempty"`
	UserDataDir      string `json:"user_data_dir,omitempty"`
	ProfileDirectory string `json:"profile_directory,omitempty"`
	Mode             string `json:"mode,omitempty"`
}

func (i Identity) Empty() bool {
	return i.Workspace == "" &&
		i.Profile == "" &&
		i.UserDataDir == "" &&
		i.ProfileDirectory == "" &&
		i.Mode == ""
}

// Mismatches compares non-empty expected fields. Empty expected fields are
// treated as "do not care" so an upstream proxy can require workspace/profile
// identity without pinning the daemon's transport mode.
func (i Identity) Mismatches(expected Identity) []string {
	var out []string
	check := func(field, got, want string) {
		if want != "" && got != want {
			out = append(out, fmt.Sprintf("%s got %q want %q", field, got, want))
		}
	}
	check("workspace", i.Workspace, expected.Workspace)
	check("profile", i.Profile, expected.Profile)
	check("user_data_dir", i.UserDataDir, expected.UserDataDir)
	check("profile_directory", i.ProfileDirectory, expected.ProfileDirectory)
	check("mode", i.Mode, expected.Mode)
	return out
}
