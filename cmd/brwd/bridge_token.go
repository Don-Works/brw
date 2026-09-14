package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// bridgeTokenTarget says where the per-launch extension-bridge handshake token
// would live on disk, and whether an operator asked for it to be written there.
//
// The daemon does NOT write the token by default any more. It used to, at
// ~/.brw/bridge-token, "for operator inspection"; nothing in the tree ever read
// it. What it did do was keep a second copy of the secret at rest, outliving the
// daemon that minted it, readable by every process running as this user —
// available even while brwd is not running, which the /status endpoint is not.
// The token is worth exactly one signed-in browser, so the copy no reader needed
// is gone and BRW_BRIDGE_TOKEN_FILE is the way to ask for it back.
type bridgeTokenTarget struct {
	// Path is the file. Empty means there is nowhere to write and nothing to
	// clean up (no home directory resolvable).
	Path string
	// OptedIn is true only when BRW_BRIDGE_TOKEN_FILE named the path. A default
	// path is a path to REMOVE, not one to write.
	OptedIn bool
}

// bridgeTokenFile resolves the target for this daemon. Workspace-bound daemons
// get a per-workspace name (bridge-token-<workspace>): several bridge daemons on
// one machine otherwise clobber a single shared file, last writer wins, so the
// persisted token matched only one of the running daemons.
func bridgeTokenFile(workspace string) bridgeTokenTarget {
	if override := strings.TrimSpace(os.Getenv("BRW_BRIDGE_TOKEN_FILE")); override != "" {
		return bridgeTokenTarget{Path: override, OptedIn: true}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return bridgeTokenTarget{}
	}
	name := "bridge-token"
	if workspace != "" {
		name += "-" + strings.Map(func(r rune) rune {
			switch r {
			case '/', '\\', ':':
				return '-'
			}
			return r
		}, workspace)
	}
	return bridgeTokenTarget{Path: filepath.Join(home, ".brw", name)}
}

// persistBridgeToken writes the token 0600 when an operator opted in, and
// otherwise deletes the file at the default path.
//
// The delete is the point of the default branch: a machine that ran an older
// brwd has a bridge-token file sitting in ~/.brw containing a token that daemon
// minted, and upgrading is exactly when nothing is left to notice it. Removing
// it on the next launch is the only pass that will ever happen.
func persistBridgeToken(target bridgeTokenTarget, token string) error {
	if target.Path == "" {
		return nil
	}
	if !target.OptedIn {
		if err := os.Remove(target.Path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("could not remove the stale bridge token file %s: %w", target.Path, err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(target.Path), 0o700); err != nil {
		return fmt.Errorf("could not create the bridge token dir %s: %w", filepath.Dir(target.Path), err)
	}
	if err := os.WriteFile(target.Path, []byte(token), 0o600); err != nil {
		return fmt.Errorf("could not persist the bridge token to %s: %w", target.Path, err)
	}
	return nil
}
