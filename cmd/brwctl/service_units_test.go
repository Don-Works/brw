package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/setup"
)

const handMadePlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>co.example.brw.chromium</string>
  <key>ProgramArguments</key>
  <array>
    <string>%BRWD%</string>
    <string>--workspace</string>
    <string>brw-chromium</string>
    <string>--profile</string>
    <string>chromium-profile</string>
    <string>--bridge</string>
  </array>
  <key>KeepAlive</key>
  <true/>
</dict>
</plist>
`

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBrwdServiceUnitsFindsUnitsByProgram: the label is whatever the operator
// chose, so a unit is recognised by the brwd it runs. Units for another install
// or another program are not this install's to restart.
func TestBrwdServiceUnitsFindsUnitsByProgram(t *testing.T) {
	cases := []struct {
		name        string
		goos        string
		units       func(appBrwd, otherBrwd string) map[string]string
		wantLabels  []string
		wantProfile string
	}{
		{
			name: "hand-made launchd agent",
			goos: "darwin",
			units: func(appBrwd, otherBrwd string) map[string]string {
				return map[string]string{
					"Library/LaunchAgents/co.example.brw.chromium.plist": strings.ReplaceAll(handMadePlist, "%BRWD%", appBrwd),
					"Library/LaunchAgents/co.example.other.plist":        strings.ReplaceAll(strings.ReplaceAll(handMadePlist, "%BRWD%", otherBrwd), "co.example.brw.chromium", "co.example.other"),
					"Library/LaunchAgents/com.unrelated.plist":           strings.ReplaceAll(handMadePlist, "%BRWD%", "/usr/bin/true"),
				}
			},
			wantLabels:  []string{"co.example.brw.chromium"},
			wantProfile: "chromium-profile",
		},
		{
			name: "systemd unit with a quoted path",
			goos: "linux",
			units: func(appBrwd, otherBrwd string) map[string]string {
				return map[string]string{
					".config/systemd/user/my-brw.service":    "[Service]\nExecStart=\"" + appBrwd + "\" --profile work --bridge\n",
					".config/systemd/user/other-brw.service": "[Service]\nExecStart=" + otherBrwd + " --profile work\n",
				}
			},
			wantLabels:  []string{"my-brw"},
			wantProfile: "work",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			appBrwd := filepath.Join(home, "app", "bin", "brwd")
			otherBrwd := filepath.Join(home, "elsewhere", "bin", "brwd")
			writeTestFile(t, appBrwd, "brwd")
			writeTestFile(t, otherBrwd, "brwd")
			for rel, content := range tc.units(appBrwd, otherBrwd) {
				writeTestFile(t, filepath.Join(home, rel), content)
			}
			units := brwdServiceUnits(tc.goos, home, appBrwd)
			var labels []string
			for _, unit := range units {
				labels = append(labels, unit.Label)
			}
			if !slices.Equal(labels, tc.wantLabels) {
				t.Fatalf("labels = %v, want %v", labels, tc.wantLabels)
			}
			if got := unitProfile(units[0]); got != tc.wantProfile {
				t.Fatalf("profile = %q, want %q", got, tc.wantProfile)
			}
		})
	}
}

// TestBrwdServiceUnitsFollowsTheBinDirSymlink: a unit may name the BINDIR link
// rather than the file in the app directory; both run the same binary.
func TestBrwdServiceUnitsFollowsTheBinDirSymlink(t *testing.T) {
	home := t.TempDir()
	appBrwd := filepath.Join(home, "app", "bin", "brwd")
	writeTestFile(t, appBrwd, "brwd")
	link := filepath.Join(home, ".local", "bin", "brwd")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(appBrwd, link); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(home, ".config/systemd/user/linked.service"), "[Service]\nExecStart="+link+" --bridge\n")
	units := brwdServiceUnits("linux", home, appBrwd)
	if len(units) != 1 || units[0].Label != "linked" {
		t.Fatalf("units = %+v, want the unit that runs brwd through the symlink", units)
	}
}

// TestUpgradeRestartsHandMadeUnits: a machine set up by hand runs its daemons
// under labels setup never wrote. Before this, upgrade restarted nothing there
// and the old build kept serving until someone noticed.
func TestUpgradeRestartsHandMadeUnits(t *testing.T) {
	fx := newUpgradeFixture(t)
	if err := os.Remove(setup.ServiceParams{GOOS: "linux", Profile: fixtureProfile, Home: fx.home}.UnitPath()); err != nil {
		t.Fatal(err)
	}
	brwd := filepath.Join(fx.appDir, "bin", "brwd")
	fx.write(filepath.Join(fx.home, ".config/systemd/user/hand-made-brw.service"),
		"[Service]\nExecStart=\""+brwd+"\" --profile "+fixtureProfile+" --bridge\n")

	result, err := runUpgrade(fx.options())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.RestartedServices, []string{"hand-made-brw"}) {
		t.Fatalf("restarted = %v, want the hand-made unit", result.RestartedServices)
	}
	if !fx.runner.called("systemctl --user restart hand-made-brw.service") {
		t.Fatalf("calls = %v", fx.runner.calls)
	}
	if strings.Contains(strings.Join(result.Manual, "\n"), "No profile has an installed service unit") {
		t.Fatalf("manual notes claim nothing was restarted: %v", result.Manual)
	}
}

// TestUpgradeRestartsEachUnitOnce: a unit setup wrote also matches the program
// scan, and restarting it twice would drop the extension's connection twice.
func TestUpgradeRestartsEachUnitOnce(t *testing.T) {
	fx := newUpgradeFixture(t)
	params := setup.ServiceParams{GOOS: "linux", Profile: fixtureProfile, Home: fx.home}
	fx.write(params.UnitPath(), "[Service]\nExecStart=\""+filepath.Join(fx.appDir, "bin", "brwd")+"\" --profile "+fixtureProfile+"\n")

	result, err := runUpgrade(fx.options())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.RestartedServices, []string{params.Label()}) {
		t.Fatalf("restarted = %v, want %s once", result.RestartedServices, params.Label())
	}
}
