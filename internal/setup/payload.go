package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// BridgeDefaultsFile carries the bridge endpoint for one installed extension copy, and nothing secret: the handshake token is minted per launch and stays in the daemon's memory.
const BridgeDefaultsFile = "bridge-defaults.json"

// PayloadItems are the top-level names a release archive owns.
var PayloadItems = []string{"bin", "extension", "tests", "skills", "reader", "doc"}

const perProfileExtensionPrefix = "extension-"

// ExtensionPayloadVersion is the manifest version of an unpacked extension directory: the build the browser runs once it has loaded that directory.
func ExtensionPayloadVersion(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return "", err
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", fmt.Errorf("read %s: %w", filepath.Join(dir, "manifest.json"), err)
	}
	if manifest.Version == "" {
		return "", fmt.Errorf("%s declares no version", filepath.Join(dir, "manifest.json"))
	}
	return manifest.Version, nil
}

// PerProfileExtensionDirs lists the per-profile extension payload copies under appDir.
func PerProfileExtensionDirs(appDir string) ([]string, error) {
	entries, err := os.ReadDir(appDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), perProfileExtensionPrefix) {
			continue
		}
		path := filepath.Join(appDir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			continue
		}
		dirs = append(dirs, path)
	}
	return dirs, nil
}

// RefreshExtensionPayloads brings every per-profile extension copy under appDir back in step with appDir/extension, preserving each copy's own bridge-defaults.json, and returns the directory names it refreshed.
func RefreshExtensionPayloads(appDir string) ([]string, error) {
	source := filepath.Join(appDir, "extension")
	if _, err := os.Stat(filepath.Join(source, "manifest.json")); err != nil {
		return nil, fmt.Errorf("no extension payload to refresh from: %w", err)
	}
	dirs, err := PerProfileExtensionDirs(appDir)
	if err != nil {
		return nil, err
	}
	refreshed := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if err := replacePayloadDir(source, dir); err != nil {
			return refreshed, err
		}
		refreshed = append(refreshed, filepath.Base(dir))
	}
	return refreshed, nil
}

// InstallPayload replaces every PayloadItems entry in appDir with the copy in unpacked and then refreshes the per-profile extension payloads, returning the names of the copies it refreshed.
func InstallPayload(unpacked, appDir string) ([]string, error) {
	if strings.TrimSpace(appDir) == "" || filepath.Clean(appDir) == string(filepath.Separator) {
		return nil, errors.New("refusing to install a payload over the filesystem root")
	}
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return nil, err
	}
	for _, item := range PayloadItems {
		source := filepath.Join(unpacked, item)
		if _, err := os.Stat(source); err != nil {
			continue
		}
		if err := replacePayloadDir(source, filepath.Join(appDir, item)); err != nil {
			return nil, err
		}
	}
	return RefreshExtensionPayloads(appDir)
}

func replacePayloadDir(src, dst string) error {
	defaults, defaultsMode, hadDefaults, err := readBridgeDefaults(dst)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(dst), ".brw-payload-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := copyTreeWithModes(src, stage); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(stage, BridgeDefaultsFile)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if hadDefaults {
		if err := os.WriteFile(filepath.Join(stage, BridgeDefaultsFile), defaults, defaultsMode); err != nil {
			return err
		}
	}
	previous := stage + "-old"
	if err := os.Rename(dst, previous); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(stage, dst); err != nil {
		if _, statErr := os.Lstat(previous); statErr == nil {
			return errors.Join(err, os.Rename(previous, dst))
		}
		return err
	}
	return os.RemoveAll(previous)
}

func readBridgeDefaults(dir string) (data []byte, mode os.FileMode, found bool, err error) {
	path := filepath.Join(dir, BridgeDefaultsFile)
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	data, err = os.ReadFile(path)
	if err != nil {
		return nil, 0, false, err
	}
	return data, info.Mode().Perm(), true, nil
}

func copyTreeWithModes(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, relative)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, 0o755)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case !info.Mode().IsRegular():
			return nil
		}
		return copyFileWithMode(path, target, info.Mode().Perm())
	})
}

func copyFileWithMode(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
