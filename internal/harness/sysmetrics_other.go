//go:build !unix

package harness

// ReadResourceUsage reports that this platform has no getrusage equivalent brw
// reads. Supported stays false so a record says the system metrics are absent
// rather than printing zeroes that read as "the run was free".
func ReadResourceUsage() ResourceUsage {
	return ResourceUsage{}
}
