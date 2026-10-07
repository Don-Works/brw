package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type bridgeTokenTarget struct {
	// Path is the file.
	Path string
	// OptedIn is true only when BRW_BRIDGE_TOKEN_FILE named the path.
	OptedIn bool
}

const bridgeTokenBaseName = "bridge-token"

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

func bridgeTokenAtLaunch(target bridgeTokenTarget, token string) error {
	if token != "" && target.OptedIn && target.Path != "" {
		if err := os.MkdirAll(filepath.Dir(target.Path), 0o700); err != nil {
			return fmt.Errorf("could not create the bridge token dir %s: %w", filepath.Dir(target.Path), err)
		}
		file, err := os.CreateTemp(filepath.Dir(target.Path), ".bridge-token-*")
		if err != nil {
			return fmt.Errorf("could not create the bridge token file: %w", err)
		}
		defer os.Remove(file.Name())
		_, writeErr := file.WriteString(token)
		if err := errors.Join(writeErr, file.Close()); err != nil {
			return fmt.Errorf("could not persist the bridge token: %w", err)
		}
		if err := os.Rename(file.Name(), target.Path); err != nil {
			return fmt.Errorf("could not persist the bridge token to %s: %w", target.Path, err)
		}
	}
	if token == "" {

		target.OptedIn = false
	}
	return sweepBridgeTokenFiles(target)
}

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
