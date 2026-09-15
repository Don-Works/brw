//go:build unix

package harness

import (
	"runtime"
	"syscall"
)

// ReadResourceUsage samples the kernel's accounting for this process and for
// the children it has reaped.
func ReadResourceUsage() ResourceUsage {
	usage := ResourceUsage{Supported: true}
	var self, children syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &self); err != nil {
		return ResourceUsage{}
	}
	if err := syscall.Getrusage(syscall.RUSAGE_CHILDREN, &children); err != nil {
		return ResourceUsage{}
	}
	usage.Self = convertRusage(self)
	usage.Children = convertRusage(children)
	return usage
}

func convertRusage(r syscall.Rusage) Usage {
	return Usage{
		PeakRSSBytes: normalizeMaxRSS(int64(r.Maxrss), runtime.GOOS),
		UserCPUMS:    r.Utime.Sec*1000 + int64(r.Utime.Usec)/1000,
		SysCPUMS:     r.Stime.Sec*1000 + int64(r.Stime.Usec)/1000,
	}
}

// normalizeMaxRSS converts ru_maxrss to bytes. The kernels disagree about its
// unit: Linux reports kilobytes, Darwin and the BSDs report bytes. Reporting
// one as the other is a 1024x error in a memory figure, which is exactly the
// kind of number nobody re-checks.
func normalizeMaxRSS(maxrss int64, goos string) int64 {
	if goos == "linux" {
		return maxrss * 1024
	}
	return maxrss
}
