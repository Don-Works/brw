package snapshot

import "testing"

// TestNormalizeOptionsRefusesCallerSuppliedIncludeBoxes keeps an internal option
// off the tool surface.
//
// include_boxes is json-tagged so the option object can be marshalled into the
// walker call, and brw_snapshot decodes its arguments non-strictly — so a caller
// could set a field that appears in no schema and get x/y/w/h on every element.
// The frame paths set it after normalizing, which is where it means something.
func TestNormalizeOptionsRefusesCallerSuppliedIncludeBoxes(t *testing.T) {
	for _, mode := range []string{"", "frontier", "all", "form_lens"} {
		got := NormalizeOptions(SnapshotOptions{Mode: mode, IncludeBoxes: true})
		if got.IncludeBoxes {
			t.Errorf("mode %q kept a caller-supplied include_boxes; it is not part of the tool schema", mode)
		}
	}
}
