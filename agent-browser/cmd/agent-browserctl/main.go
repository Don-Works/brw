package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/revitt/agent-browser/internal/cdp"
	"github.com/revitt/agent-browser/internal/profilepolicy"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(0)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "doctor":
		err = doctor(os.Args[2:])
	case "mcp-config":
		err = mcpConfig(os.Args[2:])
	case "macos-policy":
		err = macOSPolicy(os.Args[2:])
	case "pack-extension":
		err = packExtension(os.Args[2:])
	case "update-xml":
		err = updateXML(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: agent-browserctl <command> [options]

commands:
  doctor          verify profile policy, app install, and bridge extension state
  mcp-config      print an MCP server config for a policy profile/transport
  macos-policy    write a Chrome ExtensionSettings .mobileconfig
  pack-extension  pack the bridge extension as a CRX using installed Chrome
  update-xml      write a Chrome extension update manifest XML`)
}

func doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	var profileName, policyPath, appDir string
	fs.StringVar(&profileName, "profile", "", "workspace profile name")
	fs.StringVar(&policyPath, "profile-policy", "", "profile policy JSON path")
	fs.StringVar(&appDir, "app-dir", defaultAppDir(), "agent-browser app install directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if profileName == "" {
		return errors.New("--profile is required")
	}

	policy, err := profilepolicy.Load(policyPath)
	if err != nil {
		return err
	}
	profile, err := policy.Find(profileName)
	if err != nil {
		return err
	}

	report := map[string]any{
		"profile": profile.Name,
		"kind":    profile.Kind,
		"app_dir": appDir,
	}
	var failures []string
	for _, rel := range []string{
		"bin/agent-browserd",
		"bin/browsercheck",
		"bin/agent-browser-devtools-mcp",
		"extension/manifest.json",
		"config/browser-profiles.json",
	} {
		path := filepath.Join(appDir, rel)
		if _, err := os.Stat(path); err != nil {
			failures = append(failures, "missing "+path)
		}
	}

	profileDir := filepath.Join(profile.UserDataDir, profile.ProfileDirectory)
	report["chrome_profile_dir"] = profileDir
	if _, err := os.Stat(profileDir); err != nil {
		failures = append(failures, "missing Chrome profile dir "+profileDir)
	}

	if profile.ExtensionBridgeAllowed {
		id := profile.BridgeExtensionID
		if id == "" {
			id = profilepolicy.DefaultBridgeExtensionID
		}
		installed, source, err := chromeExtensionInstalled(profileDir, id)
		report["bridge_extension_id"] = id
		report["bridge_extension_installed"] = installed
		if source != "" {
			report["bridge_extension_source"] = source
		}
		if err != nil {
			failures = append(failures, err.Error())
		} else if !installed {
			failures = append(failures, "bridge extension "+id+" is not installed in "+profileDir)
		}
	}

	report["ok"] = len(failures) == 0
	if len(failures) > 0 {
		report["failures"] = failures
	}
	writeJSON(os.Stdout, report)
	if len(failures) > 0 {
		return errors.New("doctor failed")
	}
	return nil
}

func mcpConfig(args []string) error {
	fs := flag.NewFlagSet("mcp-config", flag.ContinueOnError)
	var profileName, transportName, policyPath, mode string
	fs.StringVar(&profileName, "profile", "", "workspace profile name")
	fs.StringVar(&transportName, "transport", "local", "workspace transport name")
	fs.StringVar(&policyPath, "profile-policy", "", "profile policy JSON path")
	fs.StringVar(&mode, "mode", "auto", "auto, direct, or bridge")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if profileName == "" {
		return errors.New("--profile is required")
	}

	policy, err := profilepolicy.Load(policyPath)
	if err != nil {
		return err
	}
	profile, err := policy.Find(profileName)
	if err != nil {
		return err
	}
	transport, err := policy.FindTransport(transportName)
	if err != nil {
		return err
	}
	if mode == "auto" {
		if profile.DirectCDPAllowed {
			mode = "direct"
		} else if profile.ExtensionBridgeAllowed {
			mode = "bridge"
		} else {
			return fmt.Errorf("profile %q has no allowed runtime mode", profile.Name)
		}
	}
	if mode == "bridge" && !profile.ExtensionBridgeAllowed {
		return fmt.Errorf("profile %q is not allowed for bridge mode", profile.Name)
	}
	if mode == "direct" && !profile.DirectCDPAllowed {
		return fmt.Errorf("profile %q is not allowed for direct mode", profile.Name)
	}

	argsOut := runtimeArgs(profile.Name, mode, remotePolicyPath(transport))
	command := ""
	switch transport.Kind {
	case "stdio":
		command = transport.Command
		if command == "" {
			command = "agent-browserd"
		}
	case "ssh-stdio":
		host := transport.Host
		if transport.User != "" {
			host = transport.User + "@" + host
		}
		command = "ssh"
		argsOut = []string{host, shellJoin(append([]string{remoteBinary(transport, "agent-browserd")}, argsOut...))}
	default:
		return fmt.Errorf("transport %q has unsupported kind %q for stdio MCP config", transport.Name, transport.Kind)
	}

	name := "agent-browser"
	if transport.Name != "local" {
		name += "-" + transport.Name
	}
	result := map[string]any{
		"mcpServers": map[string]any{
			name: map[string]any{
				"command": command,
				"args":    argsOut,
			},
		},
	}
	writeJSON(os.Stdout, result)
	return nil
}

func macOSPolicy(args []string) error {
	fs := flag.NewFlagSet("macos-policy", flag.ContinueOnError)
	var profileName, policyPath, updateURL, installMode, output string
	fs.StringVar(&profileName, "profile", "", "workspace profile name")
	fs.StringVar(&policyPath, "profile-policy", "", "profile policy JSON path")
	fs.StringVar(&updateURL, "update-url", "", "Chrome extension update URL")
	fs.StringVar(&installMode, "install-mode", "normal_installed", "normal_installed or force_installed")
	fs.StringVar(&output, "output", "", "output .mobileconfig path; stdout when empty")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if profileName == "" {
		return errors.New("--profile is required")
	}
	if updateURL == "" {
		return errors.New("--update-url is required")
	}
	if installMode != "normal_installed" && installMode != "force_installed" {
		return errors.New("--install-mode must be normal_installed or force_installed")
	}

	policy, err := profilepolicy.Load(policyPath)
	if err != nil {
		return err
	}
	profile, err := policy.Find(profileName)
	if err != nil {
		return err
	}
	id := profile.BridgeExtensionID
	if id == "" {
		id = profilepolicy.DefaultBridgeExtensionID
	}
	data := []byte(chromePolicyMobileconfig(id, installMode, updateURL))
	return writeOutput(output, data, 0o644)
}

func packExtension(args []string) error {
	fs := flag.NewFlagSet("pack-extension", flag.ContinueOnError)
	var chromePath, extensionDir, keyPath, outDir string
	fs.StringVar(&chromePath, "chrome-path", "", "Chrome executable path")
	fs.StringVar(&extensionDir, "extension-dir", "extension", "extension directory")
	fs.StringVar(&keyPath, "key", "packaging/agent-browser-bridge.pem", "extension private key")
	fs.StringVar(&outDir, "out-dir", "dist/extension", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	chrome, err := cdp.FindChrome(chromePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	absExt, err := filepath.Abs(extensionDir)
	if err != nil {
		return err
	}
	absKey, err := filepath.Abs(keyPath)
	if err != nil {
		return err
	}
	cmd := exec.Command(chrome, "--pack-extension="+absExt, "--pack-extension-key="+absKey)
	var stderr bytes.Buffer
	cmd.Stdout = os.Stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pack extension: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	src := absExt + ".crx"
	dst := filepath.Join(outDir, "agent-browser-bridge.crx")
	if err := copyFile(src, dst); err != nil {
		return err
	}
	_ = os.Remove(src)
	fmt.Fprintf(os.Stderr, "wrote %s\n", dst)
	return nil
}

func updateXML(args []string) error {
	fs := flag.NewFlagSet("update-xml", flag.ContinueOnError)
	var profileName, policyPath, crxURL, version, output string
	fs.StringVar(&profileName, "profile", "", "workspace profile name")
	fs.StringVar(&policyPath, "profile-policy", "", "profile policy JSON path")
	fs.StringVar(&crxURL, "crx-url", "", "absolute URL to agent-browser-bridge.crx")
	fs.StringVar(&version, "version", extensionVersion("extension/manifest.json"), "extension version")
	fs.StringVar(&output, "output", "", "output XML path; stdout when empty")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if profileName == "" {
		return errors.New("--profile is required")
	}
	if crxURL == "" {
		return errors.New("--crx-url is required")
	}
	policy, err := profilepolicy.Load(policyPath)
	if err != nil {
		return err
	}
	profile, err := policy.Find(profileName)
	if err != nil {
		return err
	}
	id := profile.BridgeExtensionID
	if id == "" {
		id = profilepolicy.DefaultBridgeExtensionID
	}
	data := []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<gupdate xmlns="http://www.google.com/update2/response" protocol="2.0">
  <app appid="%s">
    <updatecheck codebase="%s" version="%s" />
  </app>
</gupdate>
`, xmlEscape(id), xmlEscape(crxURL), xmlEscape(version)))
	return writeOutput(output, data, 0o644)
}

func chromeExtensionInstalled(profileDir, id string) (bool, string, error) {
	for _, name := range []string{"Preferences", "Secure Preferences"} {
		path := filepath.Join(profileDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, "", err
		}
		var prefs struct {
			Extensions struct {
				Settings map[string]json.RawMessage `json:"settings"`
			} `json:"extensions"`
		}
		if err := json.Unmarshal(data, &prefs); err != nil {
			continue
		}
		if _, ok := prefs.Extensions.Settings[id]; ok {
			return true, path, nil
		}
	}
	return false, "", nil
}

func runtimeArgs(profile, mode, policyPath string) []string {
	args := []string{"--mcp", "--http", "off", "--profile", profile}
	if mode == "bridge" {
		args = append([]string{"--bridge"}, args...)
		args = append(args, "--bridge-addr", "127.0.0.1:17311")
	}
	if policyPath != "" {
		args = append(args, "--profile-policy", policyPath)
	}
	return args
}

func remoteBinary(t profilepolicy.Transport, name string) string {
	if t.AppDir == "" {
		return name
	}
	return filepath.Join(t.AppDir, "bin", name)
}

func remotePolicyPath(t profilepolicy.Transport) string {
	if t.AppDir == "" {
		return ""
	}
	return filepath.Join(t.AppDir, "config", "browser-profiles.json")
}

func defaultAppDir() string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "agent-browser")
	}
	return filepath.Join(home, ".local", "share", "agent-browser")
}

func shellJoin(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, quoteRemote(arg))
	}
	return strings.Join(quoted, " ")
}

func quoteRemote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.HasPrefix(s, "~/") {
		homePath := "$HOME/" + strings.TrimPrefix(s, "~/")
		return `"` + strings.ReplaceAll(homePath, `"`, `\"`) + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func chromePolicyMobileconfig(extensionID, installMode, updateURL string) string {
	identifier := "co.revitt.agent-browser.chrome-extension"
	uuid1 := randomHex()
	uuid2 := randomHex()
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>PayloadType</key>
  <string>Configuration</string>
  <key>PayloadVersion</key>
  <integer>1</integer>
  <key>PayloadIdentifier</key>
  <string>%s</string>
  <key>PayloadUUID</key>
  <string>%s</string>
  <key>PayloadDisplayName</key>
  <string>agent-browser Chrome bridge</string>
  <key>PayloadDescription</key>
  <string>Installs the agent-browser Chrome bridge extension for approved browser profiles.</string>
  <key>PayloadContent</key>
  <array>
    <dict>
      <key>PayloadType</key>
      <string>com.google.Chrome</string>
      <key>PayloadVersion</key>
      <integer>1</integer>
      <key>PayloadIdentifier</key>
      <string>%s.settings</string>
      <key>PayloadUUID</key>
      <string>%s</string>
      <key>PayloadDisplayName</key>
      <string>Google Chrome ExtensionSettings</string>
      <key>ExtensionSettings</key>
      <dict>
        <key>%s</key>
        <dict>
          <key>installation_mode</key>
          <string>%s</string>
          <key>update_url</key>
          <string>%s</string>
        </dict>
      </dict>
    </dict>
  </array>
</dict>
</plist>
`, identifier, uuid1, identifier, uuid2, xmlEscape(extensionID), xmlEscape(installMode), xmlEscape(updateURL))
}

func randomHex() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	return strings.ToUpper(hex.EncodeToString(b[:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" + hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" + hex.EncodeToString(b[10:]))
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func extensionVersion(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "0.1.0"
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil || manifest.Version == "" {
		return "0.1.0"
	}
	return manifest.Version
}

func writeJSON(w io.Writer, value any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(value)
}

func writeOutput(path string, data []byte, mode os.FileMode) error {
	if path == "" || path == "-" {
		_, err := os.Stdout.Write(data)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, mode)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
