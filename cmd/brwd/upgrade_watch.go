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

	upgradeSettlePolls = 2
)

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
