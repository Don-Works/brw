package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/cli"
)

// Environment is the fingerprint every record carries.
//
// A latency number without one is not a measurement, because nothing says
// whether two runs describe the same machine and the same browser. Everything
// here is hardware, build or fixture identity; nothing identifies the operator
// or the machine's owner.
type Environment struct {
	CapturedAt    time.Time `json:"captured_at"`
	BrwVersion    string    `json:"brw_version"`
	GoVersion     string    `json:"go_version"`
	OS            string    `json:"os"`
	Arch          string    `json:"arch"`
	CPUModel      string    `json:"cpu_model"`
	CPUs          int       `json:"cpus"`
	Browser       string    `json:"browser"`
	CDPProtocol   string    `json:"cdp_protocol,omitempty"`
	Headless      bool      `json:"headless"`
	FixtureDigest string    `json:"fixture_digest"`
}

// Fingerprint is the one-line form, for a human deciding at a glance whether
// two runs are comparable.
func (e Environment) Fingerprint() string {
	digest := e.FixtureDigest
	if len(digest) > 12 {
		digest = digest[:12]
	}
	return fmt.Sprintf("%s/%s %s x%d | %s | brw %s | %s | fixtures %s",
		e.OS, e.Arch, e.CPUModel, e.CPUs, e.Browser, e.BrwVersion, e.GoVersion, digest)
}

// ComparedFields names the parts of the fingerprint that decide whether two
// records can be held against each other. It is exported so a test can check
// the list against the struct rather than against itself: a field added to
// Environment and left out of here would silently stop mattering.
var ComparedFields = []string{"os", "arch", "cpu_model", "cpus", "browser", "headless", "fixture_digest"}

// Comparable reports whether two records were taken somewhere the numbers can
// be held against each other, and names the first thing that differs when they
// cannot.
func (e Environment) Comparable(other Environment) (bool, string) {
	for _, field := range []struct {
		name string
		a, b any
	}{
		{"os", e.OS, other.OS},
		{"arch", e.Arch, other.Arch},
		{"cpu_model", e.CPUModel, other.CPUModel},
		{"cpus", e.CPUs, other.CPUs},
		{"browser", e.Browser, other.Browser},
		{"headless", e.Headless, other.Headless},
		{"fixture_digest", e.FixtureDigest, other.FixtureDigest},
	} {
		if field.a != field.b {
			return false, fmt.Sprintf("%s differs: %v vs %v", field.name, field.a, field.b)
		}
	}
	return true, ""
}

// ChromeVersion is what Chrome reports about itself over its debugging port.
type ChromeVersion struct {
	Browser  string `json:"Browser"`
	Protocol string `json:"Protocol-Version"`
}

// ReadChromeVersion asks the running browser what build it is.
func ReadChromeVersion(endpoint string) (ChromeVersion, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(strings.TrimRight(endpoint, "/") + "/json/version")
	if err != nil {
		return ChromeVersion{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ChromeVersion{}, fmt.Errorf("chrome /json/version returned %s", resp.Status)
	}
	var version ChromeVersion
	if err := json.NewDecoder(resp.Body).Decode(&version); err != nil {
		return ChromeVersion{}, err
	}
	return version, nil
}

// DescribeEnvironment fills in everything that does not need a browser.
func DescribeEnvironment() Environment {
	return Environment{
		CapturedAt: time.Now().UTC(),
		BrwVersion: cli.Version,
		GoVersion:  runtime.Version(),
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		CPUModel:   CPUModel(),
		CPUs:       runtime.NumCPU(),
	}
}

// CPUModel names the processor. It is part of the fingerprint because the same
// harness on a different CPU produces different numbers, and a record that does
// not say which CPU invites exactly that comparison.
func CPUModel() string {
	switch runtime.GOOS {
	case "darwin":
		if model := sysctl("machdep.cpu.brand_string"); model != "" {
			return model
		}
	case "linux":
		if model := procCPUModel(); model != "" {
			return model
		}
	}
	return "unknown"
}

func sysctl(key string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sysctl", "-n", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func procCPUModel() string {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "model name", "Model", "Hardware":
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}
