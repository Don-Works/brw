//go:build unix

package harness

import (
	"os"
	"os/exec"
	"testing"
)

const burnEnv = "BRW_HARNESS_BURN_CPU"

// TestMain lets this test binary re-exec itself as a short-lived child, which
// is the only honest way to check that child accounting works: getrusage
// reports a child only once it has exited and been reaped.
func TestMain(m *testing.M) {
	if os.Getenv(burnEnv) != "" {
		burnCPU()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func burnCPU() {
	total := 0
	for i := 0; i < 40_000_000; i++ {
		total += i % 7
	}
	// Keep the compiler from deleting the loop.
	if total < 0 {
		os.Exit(1)
	}
}

func TestNormalizeMaxRSSUsesEachKernelsUnit(t *testing.T) {
	cases := []struct {
		goos   string
		maxrss int64
		want   int64
	}{
		{goos: "linux", maxrss: 2048, want: 2048 * 1024},
		{goos: "darwin", maxrss: 2048, want: 2048},
		{goos: "freebsd", maxrss: 2048, want: 2048},
		{goos: "openbsd", maxrss: 2048, want: 2048},
	}
	for _, testCase := range cases {
		t.Run(testCase.goos, func(t *testing.T) {
			if got := normalizeMaxRSS(testCase.maxrss, testCase.goos); got != testCase.want {
				t.Fatalf("normalizeMaxRSS(%d, %q) = %d, want %d", testCase.maxrss, testCase.goos, got, testCase.want)
			}
		})
	}
}

// TestChildCPUIsAccountedAfterReaping is the claim the benchmark's "browser
// tree" row rests on. If RUSAGE_CHILDREN did not pick a reaped child up, the
// record would report a browser that cost nothing.
func TestChildCPUIsAccountedAfterReaping(t *testing.T) {
	before := ReadResourceUsage()
	if !before.Supported {
		t.Skip("no resource accounting on this platform")
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestChildCPUIsAccountedAfterReaping")
	cmd.Env = append(os.Environ(), burnEnv+"=1")
	if err := cmd.Run(); err != nil {
		t.Fatalf("burn child: %v", err)
	}

	after := ReadResourceUsage()
	delta := after.Children.CPUDelta(before.Children)
	if delta.UserCPUMS+delta.SysCPUMS <= 0 {
		t.Fatalf("a child that burned CPU added %dms user and %dms sys to the children reading",
			delta.UserCPUMS, delta.SysCPUMS)
	}
	if after.Children.PeakRSSBytes <= 0 {
		t.Fatalf("children peak RSS = %d, want a positive high-water mark", after.Children.PeakRSSBytes)
	}
	if after.Self.PeakRSSBytes <= 0 {
		t.Fatalf("self peak RSS = %d, want a positive high-water mark", after.Self.PeakRSSBytes)
	}
}

// TestCPUDeltaKeepsThePeakAndDifferencesTheTime guards the one piece of
// arithmetic that is easy to get backwards: a high-water mark must not be
// subtracted, or every record would report the browser's memory as near zero.
func TestCPUDeltaKeepsThePeakAndDifferencesTheTime(t *testing.T) {
	earlier := Usage{PeakRSSBytes: 100, UserCPUMS: 10, SysCPUMS: 4}
	later := Usage{PeakRSSBytes: 900, UserCPUMS: 35, SysCPUMS: 9}
	delta := later.CPUDelta(earlier)
	if delta.PeakRSSBytes != 900 {
		t.Errorf("peak RSS = %d, want the later high-water mark 900", delta.PeakRSSBytes)
	}
	if delta.UserCPUMS != 25 || delta.SysCPUMS != 5 {
		t.Errorf("cpu delta = %dms user, %dms sys; want 25 and 5", delta.UserCPUMS, delta.SysCPUMS)
	}
}
