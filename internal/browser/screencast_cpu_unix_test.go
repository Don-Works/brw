//go:build unix

package browser

import (
	"syscall"
	"time"
)

// processCPU is the CPU this process has burned, user plus system. The
// screencast/screenshot comparison is about work brw does per frame — the JSON
// envelope, the base64 decode, the round trip — so the daemon's own CPU is the
// number that moves when the transport changes.
func processCPU() (time.Duration, bool) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, false
	}
	user := time.Duration(usage.Utime.Sec)*time.Second + time.Duration(usage.Utime.Usec)*time.Microsecond
	system := time.Duration(usage.Stime.Sec)*time.Second + time.Duration(usage.Stime.Usec)*time.Microsecond
	return user + system, true
}
