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

// bridgeTokenBaseName is the only name the daemon has ever written a token
// under, bare or with a -<workspace> suffix. The sweep matches on it so that it
// can never reach an operator's own file in the same directory.
const bridgeTokenBaseName = "bridge-token"

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
	name := bridgeTokenBaseName
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

// bridgeTokenAtLaunch is what a brwd launch does about the handshake token on
// disk: write it 0600 when an operator opted in, and sweep every token file an
// older brwd left in ~/.brw either way.
//
// token is empty on a launch that minted none — direct CDP, upstream proxy, any
// mode but the extension bridge. There is nothing to write then, and an opted-in
// path holding some previous launch's token is a secret at rest for a daemon
// that no longer exists, so it is swept like the rest rather than kept.
//
// The sweep is the point of the default path: a machine that ran an older brwd
// has a bridge-token file sitting in ~/.brw containing a token that daemon
// minted, and upgrading is exactly when nothing is left to notice it. A launch
// is the only pass that will ever happen, which is why this is called on the
// path EVERY launch takes and not from inside the bridge branch.
func bridgeTokenAtLaunch(target bridgeTokenTarget, token string) error {
	if token != "" && target.OptedIn && target.Path != "" {
		if err := os.MkdirAll(filepath.Dir(target.Path), 0o700); err != nil {
			return fmt.Errorf("could not create the bridge token dir %s: %w", filepath.Dir(target.Path), err)
		}
		if err := os.WriteFile(target.Path, []byte(token), 0o600); err != nil {
			return fmt.Errorf("could not persist the bridge token to %s: %w", target.Path, err)
		}
	}
	if token == "" {
		// Nothing was minted, so nothing is kept: a stale opted-in file is the
		// exposure the opt-in asked for, not one it is owed forever.
		target.OptedIn = false
	}
	return sweepBridgeTokenFiles(target)
}

// sweepBridgeTokenFiles removes every bridge token file in ~/.brw except one an
// operator asked for. The removal used to be the single path THIS launch
// resolved to, which left two holes docs/auth-model.md does not allow for: a
// machine that once ran a default-workspace daemon and now runs only
// workspace-bound ones kept ~/.brw/bridge-token forever, and pointing
// BRW_BRIDGE_TOKEN_FILE somewhere else meant the default path was never visited
// at all. Both are a token at rest that nothing else will ever collect.
//
// Only the two names this daemon has ever written are matched, so a sweep can
// never reach an operator's own file that happens to live in ~/.brw.
func sweepBridgeTokenFiles(keep bridgeTokenTarget) error {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	dir := filepath.Join(home, ".brw")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("could not read %s to clean up stale bridge token files: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !isBridgeTokenFileName(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if keep.OptedIn && samePath(path, keep.Path) {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("could not remove the stale bridge token file %s: %w", path, err)
		}
	}
	return nil
}

// isBridgeTokenFileName matches exactly what bridgeTokenFile can produce: the
// default name and the per-workspace one.
func isBridgeTokenFileName(name string) bool {
	return name == bridgeTokenBaseName || strings.HasPrefix(name, bridgeTokenBaseName+"-")
}

func samePath(left, right string) bool {
	leftAbs, err := filepath.Abs(left)
	if err != nil {
		leftAbs = left
	}
	rightAbs, err := filepath.Abs(right)
	if err != nil {
		rightAbs = right
	}
	return filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
}
