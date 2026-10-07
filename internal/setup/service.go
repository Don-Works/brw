package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// LaunchAgentPrefix namespaces every LaunchAgent setup writes.
const LaunchAgentPrefix = "co.donworks.brwd."

// ServiceParams is the fully resolved description of one per-user background brwd.
type ServiceParams struct {
	GOOS       string
	Workspace  string
	Profile    string
	PolicyPath string
	BRWDPath   string
	HTTPAddr   string
	// BridgeAddr is the extension bridge WebSocket address.
	BridgeAddr string
	LogPath    string
	WorkingDir string
	Home       string
}

// Label is the launchd label / systemd unit name / scheduled task name for this profile's daemon.
func (p ServiceParams) Label() string {
	if p.GOOS == "darwin" {
		return LaunchAgentPrefix + sanitiseLabel(p.Profile)
	}
	return "brwd-" + sanitiseLabel(p.Profile)
}

// UnitPath is where the platform's per-user service manager reads this unit from.
func (p ServiceParams) UnitPath() string {
	switch p.GOOS {
	case "darwin":
		return filepath.Join(p.Home, "Library", "LaunchAgents", p.Label()+".plist")
	case "windows":
		return filepath.Join(p.Home, "AppData", "Local", "brw", p.Label()+".cmd")
	default:
		return filepath.Join(p.Home, ".config", "systemd", "user", p.Label()+".service")
	}
}

// Args is the brwd command line the service runs.
func (p ServiceParams) Args() []string {
	args := []string{p.BRWDPath}
	if p.Workspace != "" {
		args = append(args, "--workspace", p.Workspace)
	}
	if p.Profile != "" {
		args = append(args, "--profile", p.Profile)
	}
	if p.PolicyPath != "" {
		args = append(args, "--profile-policy", p.PolicyPath)
	}
	if p.BridgeAddr != "" {
		args = append(args, "--bridge")
	}
	args = append(args, "--http", p.HTTPAddr)
	if p.BridgeAddr != "" {
		args = append(args, "--bridge-addr", p.BridgeAddr)
	}
	return args
}

// DefaultLogPath is the per-user log the service writes both streams to.
func DefaultLogPath(goos, home, profile string) string {
	name := "brwd-" + sanitiseLabel(profile) + ".log"
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "Logs", "brw", name)
	case "windows":
		return filepath.Join(home, "AppData", "Local", "brw", "logs", name)
	default:
		return filepath.Join(home, ".local", "state", "brw", name)
	}
}

// LaunchAgentPlist renders the macOS LaunchAgent.
func LaunchAgentPlist(p ServiceParams) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")
	fmt.Fprintf(&b, "  <key>Label</key>\n  <string>%s</string>\n", xmlText(p.Label()))
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, arg := range p.Args() {
		fmt.Fprintf(&b, "    <string>%s</string>\n", xmlText(arg))
	}
	b.WriteString("  </array>\n")
	if p.WorkingDir != "" {
		fmt.Fprintf(&b, "  <key>WorkingDirectory</key>\n  <string>%s</string>\n", xmlText(p.WorkingDir))
	}
	fmt.Fprintf(&b, "  <key>StandardOutPath</key>\n  <string>%s</string>\n", xmlText(p.LogPath))
	fmt.Fprintf(&b, "  <key>StandardErrorPath</key>\n  <string>%s</string>\n", xmlText(p.LogPath))
	b.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	b.WriteString("  <key>KeepAlive</key>\n  <true/>\n")
	b.WriteString("  <key>ProcessType</key>\n  <string>Interactive</string>\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// SystemdUnit renders the Linux `systemd --user` service.
func SystemdUnit(p ServiceParams) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=brw browser daemon for profile " + p.Profile + "\n")
	b.WriteString("After=graphical-session.target\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("ExecStart=" + strings.Join(quoteAll(p.Args()), " ") + "\n")
	if p.WorkingDir != "" {
		b.WriteString("WorkingDirectory=" + p.WorkingDir + "\n")
	}
	b.WriteString("StandardOutput=append:" + p.LogPath + "\n")
	b.WriteString("StandardError=append:" + p.LogPath + "\n")
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=5\n\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// WindowsLauncherScript renders the .cmd the scheduled task runs.
func WindowsLauncherScript(p ServiceParams) string {
	var b strings.Builder
	b.WriteString("@echo off\r\n")
	b.WriteString("setlocal\r\n")
	fmt.Fprintf(&b, "if not exist \"%s\" mkdir \"%s\"\r\n", filepath.Dir(p.LogPath), filepath.Dir(p.LogPath))
	fmt.Fprintf(&b, "%s >> \"%s\" 2>&1\r\n", strings.Join(quoteAll(p.Args()), " "), p.LogPath)
	return b.String()
}

// WindowsTaskArgs is the schtasks invocation that runs the launcher at logon.
func WindowsTaskArgs(p ServiceParams) []string {
	return []string{
		"schtasks", "/Create", "/F",
		"/SC", "ONLOGON",
		"/TN", p.Label(),
		"/TR", `"` + p.UnitPath() + `"`,
	}
}

// SystemdAvailable reports whether a `systemd --user` instance is actually running for this user.
func SystemdAvailable(runtimeDir string) bool {
	if runtimeDir == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(runtimeDir, "systemd"))
	return err == nil && info.IsDir()
}

// ServiceUnit is one already-installed background unit found on disk, reduced to the fields conflict detection needs.
type ServiceUnit struct {
	Path   string
	Label  string
	Tokens []string
}

// Conflict is a pre-existing unit that already drives the same brwd profile or binds the same loopback ports under a label setup does not own.
type Conflict struct {
	Path   string
	Label  string
	Reason string
}

var (
	plistStringPattern = regexp.MustCompile(`(?s)<string>(.*?)</string>`)
	plistLabelPattern  = regexp.MustCompile(`(?s)<key>Label</key>\s*<string>(.*?)</string>`)
)

// ScanServiceUnits reads every per-user unit in dir that launches a brwd.
func ScanServiceUnits(dir, goos string) ([]ServiceUnit, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	suffix := ".plist"
	if goos != "darwin" {
		suffix = ".service"
	}
	var units []ServiceUnit
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := string(data)
		if !mentionsBRWD(text) {
			continue
		}
		unit := ServiceUnit{Path: path}
		if goos == "darwin" {
			if match := plistLabelPattern.FindStringSubmatch(text); len(match) == 2 {
				unit.Label = strings.TrimSpace(match[1])
			}
			for _, match := range plistStringPattern.FindAllStringSubmatch(text, -1) {
				unit.Tokens = append(unit.Tokens, strings.TrimSpace(match[1]))
			}
		} else {
			unit.Label = strings.TrimSuffix(entry.Name(), suffix)
			for _, line := range strings.Split(text, "\n") {
				if value, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart="); ok {
					unit.Tokens = append(unit.Tokens, strings.Fields(value)...)
				}
			}
		}
		units = append(units, unit)
	}
	return units, nil
}

// Conflicts returns the units that already own this profile or these ports under a different label.
func Conflicts(units []ServiceUnit, p ServiceParams) []Conflict {
	ourLabel := p.Label()
	ourPath := p.UnitPath()
	var out []Conflict
	for _, unit := range units {
		if unit.Label == ourLabel || unit.Path == ourPath {
			continue
		}
		var reasons []string
		if p.Profile != "" && slices.Contains(unit.Tokens, p.Profile) {
			reasons = append(reasons, "drives profile "+p.Profile)
		}
		if p.HTTPAddr != "" && slices.Contains(unit.Tokens, p.HTTPAddr) {
			reasons = append(reasons, "binds HTTP "+p.HTTPAddr)
		}
		if p.BridgeAddr != "" && slices.Contains(unit.Tokens, p.BridgeAddr) {
			reasons = append(reasons, "binds bridge "+p.BridgeAddr)
		}
		if len(reasons) == 0 {
			continue
		}
		label := unit.Label
		if label == "" {
			label = "(unreadable label)"
		}
		out = append(out, Conflict{Path: unit.Path, Label: label, Reason: strings.Join(reasons, ", ")})
	}
	return out
}

func mentionsBRWD(text string) bool {
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return r == '<' || r == '>' || r == ' ' || r == '\n' || r == '\r' || r == '\t' || r == '"' || r == '\''
	}) {
		if filepath.Base(field) == "brwd" || filepath.Base(field) == "brwd.exe" {
			return true
		}
	}
	return false
}

func sanitiseLabel(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "default"
	}
	return out
}

// Command renders an argv the way a shell would accept it, quoting only the arguments that need it.
func Command(args []string) string {
	return strings.Join(quoteAll(args), " ")
}

func quoteAll(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if strings.ContainsAny(arg, " \t\"'") {
			out = append(out, `"`+strings.ReplaceAll(arg, `"`, `\"`)+`"`)
			continue
		}
		out = append(out, arg)
	}
	return out
}

func xmlText(s string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return replacer.Replace(s)
}
