package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	upgradePollInterval = 5 * time.Second
	// upgradeSettlePolls is how many consecutive polls must see the same new
	// file before it is trusted: an install copies the binary and then re-signs
	// it, and exiting between the two would start the unsigned copy.
	upgradeSettlePolls = 2
)

// exitOnUpgradeEnabled decides whether this daemon exits when an install
// replaces its executable. "auto" turns it on only under a service manager that
// starts it again: launchd (the parent is pid 1 on macOS) or systemd, which
// sets INVOCATION_ID for every unit it runs. A daemon started from a terminal
// would stay down.
func exitOnUpgradeEnabled(mode, goos string, ppid int, getenv func(string) string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "on":
		return true, nil
	case "off":
		return false, nil
	case "", "auto":
		if getenv("INVOCATION_ID") != "" {
			return true, nil
		}
		return goos == "darwin" && ppid == 1, nil
	default:
		return false, fmt.Errorf("--exit-on-upgrade must be auto, on or off, not %q", mode)
	}
}

// upgradeWatch polls the daemon's own executable and reports when it has been
// replaced by a settled, executable file and the daemon has nothing in flight.
type upgradeWatch struct {
	path     string
	interval time.Duration
	settle   int
	busy     func() bool
}

func (w upgradeWatch) run(ctx context.Context) bool {
	started, err := os.Stat(w.path)
	if err != nil {
		return false
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	var last os.FileInfo
	stable := 0
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
		current, err := os.Stat(w.path)
		if err != nil || current.Mode()&0o111 == 0 || sameBinary(started, current) {
			last, stable = nil, 0
			continue
		}
		if last != nil && sameBinary(last, current) {
			stable++
		} else {
			stable = 0
		}
		last = current
		if stable >= w.settle && !w.busy() {
			return true
		}
	}
}

func sameBinary(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// watchForUpgrade stops the daemon once its binary has been replaced, so the
// service manager restarts it on the new build. Without it an install updates
// the file and the old code keeps serving until the next reboot.
func watchForUpgrade(ctx context.Context, stop func(), busy func() bool) {
	executable, err := os.Executable()
	if err != nil {
		log.Printf("exit-on-upgrade disabled: cannot resolve this executable: %v", err)
		return
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	watch := upgradeWatch{path: executable, interval: upgradePollInterval, settle: upgradeSettlePolls, busy: busy}
	if watch.run(ctx) {
		log.Printf("%s was replaced by a new build; exiting so the service manager starts it", executable)
		stop()
	}
}
