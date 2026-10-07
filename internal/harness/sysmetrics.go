package harness

import "fmt"

// ResourceUsage is one reading of what the run has cost the machine.
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

// CPUDelta returns the CPU time spent between two readings.
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
