package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/setup"
)

// TestDoctorNamesTheLiveBridgeEndpoint is the whole point of the check: the
// packaged bridge-defaults.json and the extension's stored config are
// indistinguishable on disk, so a machine with a stale file and a working stored
// config and a machine that is genuinely broken look identical to anything that
// reads the filesystem. Every case below is decided by what the extension
// reported, and the two file-presence cases prove the file neither forces a red
// nor earns a green on its own.
func TestDoctorNamesTheLiveBridgeEndpoint(t *testing.T) {
	tests := []struct {
		name string
		// packagedPort: "dead" writes a bridge-defaults.json naming a port
		// nothing listens on, "live" writes one naming this profile's bridge,
		// "" writes no file at all.
		packagedFile string
		// reported is what the extension said, and where it said it.
		connected     bool
		helloStatus   string // "live" or "dead"
		helloSource   string
		refusedStatus string // "live" or "dead"
		refusedSource string

		wantStatus   string
		wantDetails  []string
		wantReportOK bool
	}{
		{
			name:         "a stale file loses to a working stored config",
			packagedFile: "dead",
			connected:    true,
			helloStatus:  "live",
			helloSource:  "stored",
			wantStatus:   checkOK,
			wantDetails:  []string{"the extension is using", "stored config", "bridge-defaults.json still names", "overridden"},
			wantReportOK: true,
		},
		{
			name:          "a live config pointing at a dead port is red with no file anywhere",
			refusedStatus: "dead",
			refusedSource: "stored",
			wantStatus:    checkFail,
			wantDetails:   []string{"nothing is listening there", "stored config", "refused handshake"},
		},
		{
			name:          "a live config pointing at a dead port names the packaged layer it came from",
			packagedFile:  "dead",
			refusedStatus: "dead",
			refusedSource: "packaged",
			wantStatus:    checkFail,
			wantDetails:   []string{"nothing is listening there", "packaged bridge-defaults.json"},
		},
		{
			name:          "a refused handshake naming this bridge is not an endpoint fault",
			refusedStatus: "live",
			refusedSource: "stored",
			wantStatus:    checkOK,
			wantDetails:   []string{"that is this profile's bridge"},
		},
		{
			name:         "a file naming a dead port is red only because nothing else reported",
			packagedFile: "dead",
			wantStatus:   checkFail,
			wantDetails:  []string{"no extension has reported", "bridge-defaults.json names", "nothing is listening there"},
		},
		{
			name:         "a file naming this bridge is green",
			packagedFile: "live",
			wantStatus:   checkOK,
			wantDetails:  []string{"the installed bridge-defaults.json names this bridge"},
			wantReportOK: false, // bridge_connected is red: nothing is connected.
		},
		{
			name:        "nothing on disk and nothing reported is not a diagnosis",
			wantStatus:  checkSkip,
			wantDetails: []string{"no installed bridge-defaults.json names one"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newDoctorFixture(t)
			deadAddr := freeLoopbackAddr(t)
			liveAddr := strings.TrimPrefix(fx.bridge.URL, "http://")
			resolve := func(which string) string {
				switch which {
				case "live":
					return "http://" + liveAddr + "/status"
				case "dead":
					return "http://" + deadAddr + "/status"
				}
				return ""
			}

			if tc.packagedFile != "" {
				addr := liveAddr
				if tc.packagedFile == "dead" {
					addr = deadAddr
				}
				fx.writeFile(filepath.Join(fx.appDir, "extension", setup.BridgeDefaultsFile),
					`{"bridgeUrl":"ws://`+addr+`/extension"}`)
			}

			fx.status.Connected = tc.connected
			fx.status.Hello.StatusURL = resolve(tc.helloStatus)
			fx.status.Hello.ConfigSource = tc.helloSource
			fx.status.LastHandshake.StatusURL = resolve(tc.refusedStatus)
			fx.status.LastHandshake.ConfigSource = tc.refusedSource

			report := fx.report()
			check := checkByName(t, report, "bridge_config")
			if check.Status != tc.wantStatus {
				t.Fatalf("bridge_config = %s (%s), want %s", check.Status, check.Detail, tc.wantStatus)
			}
			for _, want := range tc.wantDetails {
				if !strings.Contains(check.Detail, want) {
					t.Fatalf("bridge_config detail %q does not mention %q", check.Detail, want)
				}
			}
			if tc.wantStatus == checkFail && check.Fix == "" {
				t.Fatal("a red bridge_config check printed no command to run")
			}
			if tc.wantReportOK && !report.OK {
				t.Fatalf("machine reported failures: %v", report.Failures)
			}
		})
	}
}

// TestDoctorBridgeConfigPrefersTheLiveHelloOverAStaleRefusal: a refused
// handshake is kept until the daemon restarts, so a reload that fixed the
// endpoint would otherwise keep being reported as broken.
func TestDoctorBridgeConfigPrefersTheLiveHelloOverAStaleRefusal(t *testing.T) {
	fx := newDoctorFixture(t)
	dead := "http://" + freeLoopbackAddr(t) + "/status"
	live := "http://" + strings.TrimPrefix(fx.bridge.URL, "http://") + "/status"

	fx.status.Connected = true
	fx.status.Hello.StatusURL = live
	fx.status.Hello.ConfigSource = "stored"
	fx.status.LastHandshake.StatusURL = dead
	fx.status.LastHandshake.ConfigSource = "packaged"

	check := checkByName(t, fx.report(), "bridge_config")
	if check.Status != checkOK {
		t.Fatalf("bridge_config = %s (%s), want ok", check.Status, check.Detail)
	}
	if strings.Contains(check.Detail, dead) {
		t.Fatalf("a connected extension was reported against a stale refusal: %s", check.Detail)
	}
}

// TestBridgeDefaultsAreReadFromEveryInstalledCopy: an upgrade preserves a
// per-profile copy's own file, so the drift can live in any of them.
func TestBridgeDefaultsAreReadFromEveryInstalledCopy(t *testing.T) {
	appDir := t.TempDir()
	write := func(dir, body string) {
		t.Helper()
		target := filepath.Join(appDir, dir)
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, setup.BridgeDefaultsFile), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("extension", `{"bridgeUrl":"ws://127.0.0.1:17311/extension"}`)
	write("extension-work", `{"statusUrl":"http://localhost:19311/status"}`)
	write("extension-port", `{"bridgePort":19312}`)
	write("extension-label", `{"label":"desk"}`)

	found, err := setup.InstalledBridgeDefaults(appDir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, file := range found {
		got[filepath.Base(filepath.Dir(file.Path))] = file.StatusURL
	}
	want := map[string]string{
		"extension":       "http://127.0.0.1:17311/status",
		"extension-work":  "http://localhost:19311/status",
		"extension-port":  "http://127.0.0.1:19312/status",
		"extension-label": "",
	}
	for dir, wantURL := range want {
		if got[dir] != wantURL {
			t.Fatalf("%s status URL = %q, want %q", dir, got[dir], wantURL)
		}
	}
}

// TestSameEndpointIgnoresLoopbackSpelling: the extension accepts localhost and
// 127.0.0.1 interchangeably, so comparing the strings would call a working
// machine misconfigured.
func TestSameEndpointIgnoresLoopbackSpelling(t *testing.T) {
	tests := []struct {
		left, right string
		want        bool
	}{
		{"http://127.0.0.1:17311/status", "http://localhost:17311/status", true},
		{"http://localhost:17311/status", "http://localhost:17311/status", true},
		{"http://127.0.0.1:17311/status", "http://127.0.0.1:19311/status", false},
		{"http://127.0.0.1:17311/status", "", false},
		{"", "", false},
		{"not a url", "http://127.0.0.1:17311/status", false},
	}
	for _, tc := range tests {
		if got := sameEndpoint(tc.left, tc.right); got != tc.want {
			t.Fatalf("sameEndpoint(%q, %q) = %v, want %v", tc.left, tc.right, got, tc.want)
		}
	}
}
