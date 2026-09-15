package snapshot

import "strings"

const (
	// DefaultSnapshotMode is the bounded, scored visible/actionable frontier used
	// when a caller does not specify a mode. It keeps snapshots small on dense
	// pages instead of dumping every matching element.
	DefaultSnapshotMode = "frontier"
	// DefaultSnapshotLimit caps the number of elements returned in frontier mode.
	DefaultSnapshotLimit = 40
)

// NormalizeOptions applies the default snapshot envelope shared by every
// transport (MCP and HTTP): an unspecified mode becomes the bounded "frontier"
// (viewport-only, capped at DefaultSnapshotLimit) so dense pages do not emit
// thousands of elements and blow an LLM's context budget; an explicit mode keeps
// a non-negative limit. This is the single source of truth for snapshot
// defaulting so the HTTP and MCP surfaces never drift apart.
func NormalizeOptions(opts SnapshotOptions) SnapshotOptions {
	// IncludeBoxes is internal. It is json-tagged because the option object is
	// marshalled into the walker call, and brw_snapshot decodes its arguments
	// non-strictly — so without this an MCP caller could pass include_boxes:true
	// and get four extra numbers on every element from an option that appears in
	// no tool schema. The frame paths set it AFTER normalizing, which is the only
	// place it means anything.
	opts.IncludeBoxes = false
	opts.Mode = strings.TrimSpace(strings.ToLower(opts.Mode))
	if opts.Mode == "" {
		opts.Mode = DefaultSnapshotMode
	}
	if opts.Mode == DefaultSnapshotMode {
		opts.ViewportOnly = true
		if opts.Limit <= 0 {
			opts.Limit = DefaultSnapshotLimit
		}
	} else if opts.Limit < 0 {
		opts.Limit = 0
	}
	return opts
}
