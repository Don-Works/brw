package harness

import "fmt"

// ResourceUsage is one reading of what the run has cost the machine.
//
// Self is the harness process. Children is every child process the harness has
// started AND reaped — in a run that means the browser, including the render
// and GPU processes Chrome itself waits for. Both halves are therefore only
// complete after the browser has been shut down.
//
// PeakRSSBytes for Children is a high-water mark of the LARGEST such process,
// not the sum of them: that is what the operating system records, and a sum
// would be a number no kernel counted. CPU time is a true total.
type ResourceUsage struct {
	Supported bool  `json:"supported"`
	Self      Usage `json:"self"`
	Children  Usage `json:"children"`
}

// Usage is the per-scope half of a reading.
type Usage struct {
	PeakRSSBytes int64 `json:"peak_rss_bytes"`
	UserCPUMS    int64 `json:"user_cpu_ms"`
	SysCPUMS     int64 `json:"sys_cpu_ms"`
}

// CPUDelta returns the CPU time spent between two readings. Peak RSS is not
// differenced: it is a high-water mark, so the later reading already is the
// peak for the whole run.
func (u Usage) CPUDelta(earlier Usage) Usage {
	return Usage{
		PeakRSSBytes: u.PeakRSSBytes,
		UserCPUMS:    u.UserCPUMS - earlier.UserCPUMS,
		SysCPUMS:     u.SysCPUMS - earlier.SysCPUMS,
	}
}

// Describe renders a reading for the human summary.
func (u Usage) Describe() string {
	return fmt.Sprintf("peak RSS %.1f MB, CPU %dms user + %dms sys",
		float64(u.PeakRSSBytes)/(1024*1024), u.UserCPUMS, u.SysCPUMS)
}
