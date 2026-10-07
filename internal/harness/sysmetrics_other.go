//go:build !unix

package harness

// ReadResourceUsage reports that this platform has no getrusage equivalent brw reads.
func ReadResourceUsage() ResourceUsage {
	return ResourceUsage{}
}
