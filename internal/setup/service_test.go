package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func darwinService() ServiceParams {
	return ServiceParams{
		GOOS:       "darwin",
		Workspace:  "brw-chrome-profile",
		Profile:    "chrome-profile",
		PolicyPath: "/Users/someone/.config/brw/browser-profiles.json",
		BRWDPath:   "/Users/someone/Library/Application Support/brw/bin/brwd",
		HTTPAddr:   "127.0.0.1:17310",
		BridgeAddr: "127.0.0.1:17311",
		LogPath:    "/Users/someone/Library/Logs/brw/brwd-chrome-profile.log",
		WorkingDir: "/Users/someone/Library/Application Support/brw",
		Home:       "/Users/someone",
	}
}

func TestServiceArgsPerLane(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*ServiceParams)
		want    []string
		notWant []string
	}{
		{
			name: "bridge lane binds both loopback addresses",
			want: []string{"--bridge", "--http", "127.0.0.1:17310", "--bridge-addr", "127.0.0.1:17311"},
		},
		{
			name:    "direct cdp lane has no bridge listener",
			mutate:  func(p *ServiceParams) { p.BridgeAddr = "" },
			want:    []string{"--http", "127.0.0.1:17310"},
			notWant: []string{"--bridge", "--bridge-addr"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := darwinService()
			if tc.mutate != nil {
				tc.mutate(&params)
			}
			joined := strings.Join(params.Args(), " ")
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Fatalf("args missing %q: %s", want, joined)
				}
			}
			for _, unwanted := range tc.notWant {
				if strings.Contains(joined, unwanted) {
					t.Fatalf("args unexpectedly contain %q: %s", unwanted, joined)
				}
			}
			if params.Args()[0] != params.BRWDPath {
				t.Fatalf("args must start with the resolved brwd, got %q", params.Args()[0])
			}
		})
	}
}

func TestLaunchAgentPlist(t *testing.T) {
	params := darwinService()
	plist := LaunchAgentPlist(params)
	for _, want := range []string{
		"<string>co.donworks.brwd.chrome-profile</string>",
		"<string>--bridge</string>",
		"<string>127.0.0.1:17311</string>",
		"<string>/Users/someone/Library/Logs/brw/brwd-chrome-profile.log</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
	if params.UnitPath() != "/Users/someone/Library/LaunchAgents/co.donworks.brwd.chrome-profile.plist" {
		t.Fatalf("unit path = %q", params.UnitPath())
	}
}

func TestSystemdUnit(t *testing.T) {
	params := darwinService()
	params.GOOS = "linux"
	params.Home = "/home/someone"
	params.LogPath = "/home/someone/.local/state/brw/brwd-chrome-profile.log"
	unit := SystemdUnit(params)
	for _, want := range []string{
		"ExecStart=",
		"--bridge-addr 127.0.0.1:17311",
		"StandardOutput=append:/home/someone/.local/state/brw/brwd-chrome-profile.log",
		"Restart=always",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
	if params.UnitPath() != "/home/someone/.config/systemd/user/brwd-chrome-profile.service" {
		t.Fatalf("unit path = %q", params.UnitPath())
	}
}

const revittStyleAgent = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>co.revitt.brw.chromium</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/someone/Library/Application Support/brw/bin/brwd</string>
    <string>--profile</string>
    <string>chrome-profile</string>
    <string>--bridge</string>
    <string>--http</string>
    <string>127.0.0.1:17310</string>
  </array>
</dict>
</plist>
`

// TestConflictsRefusesPreExistingAgent covers the case that must never regress:
// a machine already carrying a hand-made LaunchAgent for the same profile or
// the same ports. Setup reports it and writes nothing.
func TestConflictsRefusesPreExistingAgent(t *testing.T) {
	cases := []struct {
		name       string
		file       string
		content    string
		wantReason string
	}{
		{
			name:       "hand made agent for the same profile",
			file:       "co.revitt.brw.chromium.plist",
			content:    revittStyleAgent,
			wantReason: "drives profile chrome-profile",
		},
		{
			name: "another label on the same bridge port",
			file: "org.someone.other.plist",
			content: `<?xml version="1.0"?><plist><dict>
  <key>Label</key><string>org.someone.other</string>
  <key>ProgramArguments</key><array>
    <string>/usr/local/bin/brwd</string>
    <string>--profile</string><string>unrelated</string>
    <string>--bridge-addr</string><string>127.0.0.1:17311</string>
  </array></dict></plist>`,
			wantReason: "binds bridge 127.0.0.1:17311",
		},
		{
			name: "an unrelated brwd on other ports is not a conflict",
			file: "org.someone.elsewhere.plist",
			content: `<?xml version="1.0"?><plist><dict>
  <key>Label</key><string>org.someone.elsewhere</string>
  <key>ProgramArguments</key><array>
    <string>/usr/local/bin/brwd</string>
    <string>--profile</string><string>other-profile</string>
    <string>--http</string><string>127.0.0.1:18888</string>
  </array></dict></plist>`,
		},
		{
			name: "a non-brwd agent is ignored entirely",
			file: "com.example.unrelated.plist",
			content: `<?xml version="1.0"?><plist><dict>
  <key>Label</key><string>com.example.unrelated</string>
  <key>ProgramArguments</key><array>
    <string>/usr/bin/true</string>
    <string>--profile</string><string>chrome-profile</string>
  </array></dict></plist>`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			units, err := ScanServiceUnits(dir, "darwin")
			if err != nil {
				t.Fatal(err)
			}
			conflicts := Conflicts(units, darwinService())
			if tc.wantReason == "" {
				if len(conflicts) != 0 {
					t.Fatalf("expected no conflict, got %+v", conflicts)
				}
				return
			}
			if len(conflicts) != 1 {
				t.Fatalf("conflicts = %+v, want exactly one", conflicts)
			}
			if !strings.Contains(conflicts[0].Reason, tc.wantReason) {
				t.Fatalf("reason = %q, want it to mention %q", conflicts[0].Reason, tc.wantReason)
			}
			if conflicts[0].Path != filepath.Join(dir, tc.file) {
				t.Fatalf("conflict path = %q", conflicts[0].Path)
			}
		})
	}
}

// TestConflictsIgnoresOurOwnAgent keeps a re-run from refusing to update the
// agent setup itself wrote on the previous run.
func TestConflictsIgnoresOurOwnAgent(t *testing.T) {
	dir := t.TempDir()
	params := darwinService()
	params.Home = dir
	agentDir := filepath.Join(dir, "Library", "LaunchAgents")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(params.UnitPath(), []byte(LaunchAgentPlist(params)), 0o644); err != nil {
		t.Fatal(err)
	}
	units, err := ScanServiceUnits(agentDir, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 1 {
		t.Fatalf("scan found %d units, want 1", len(units))
	}
	if conflicts := Conflicts(units, params); len(conflicts) != 0 {
		t.Fatalf("setup's own agent was reported as a conflict: %+v", conflicts)
	}
}

func TestScanServiceUnitsSystemd(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "brwd-other.service"), []byte(
		"[Service]\nExecStart=/usr/local/bin/brwd --profile chrome-profile --http 127.0.0.1:17310\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unrelated.service"), []byte(
		"[Service]\nExecStart=/usr/bin/sleep 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	units, err := ScanServiceUnits(dir, "linux")
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 1 || units[0].Label != "brwd-other" {
		t.Fatalf("units = %+v, want only the brwd one", units)
	}
	params := darwinService()
	params.GOOS = "linux"
	params.Home = "/home/someone"
	conflicts := Conflicts(units, params)
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want one", conflicts)
	}
}

func TestScanServiceUnitsToleratesMissingDirectory(t *testing.T) {
	units, err := ScanServiceUnits(filepath.Join(t.TempDir(), "nope"), "darwin")
	if err != nil || units != nil {
		t.Fatalf("units=%+v err=%v, want a quiet empty result", units, err)
	}
}

func TestSanitiseLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"chrome-profile", "chrome-profile"},
		{"Work Profile", "work-profile"},
		{"profile.1", "profile-1"},
		{"", "default"},
		{"--", "default"},
	}
	for _, tc := range cases {
		if got := sanitiseLabel(tc.in); got != tc.want {
			t.Fatalf("sanitiseLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCommandQuotesOnlyWhatNeedsIt(t *testing.T) {
	got := Command([]string{"/Applications/x y/brwd", "--http", "127.0.0.1:17310"})
	want := `"/Applications/x y/brwd" --http 127.0.0.1:17310`
	if got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
}
