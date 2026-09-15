package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// staleFrameRefClaims are things brw used to say about a ref inside a
// cross-origin iframe and no longer does.
//
// Both were true before direct CDP attached a session to the frame's own target.
// The branch that changed it rewrote three of the places that said so and left
// the rest, which is how a doc comment on the field an agent reads ends up
// telling it the opposite of what the code does.
var staleFrameRefClaims = map[string]string{
	"f<i>:e<j>":                 "the element half is a ref from the FRAME's own walk, so it carries the walker's collision suffixes (e6_edit_2) and is not always e<j>",
	"cannot be resolved by ref": "direct CDP resolves such a ref through a session attached to the frame's target; cx/cy is the fallback for the verbs that do not route",
}

// TestNoSourceRepeatsTheOldFrameRefClaims walks the package rather than naming
// the file that was wrong, because naming files is what let two of these survive
// the rewrite that was supposed to remove them.
func TestNoSourceRepeatsTheOldFrameRefClaims(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/snapshot: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || name == "frame_ref_docs_test.go" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for claim, why := range staleFrameRefClaims {
			if strings.Contains(string(body), claim) {
				t.Errorf("internal/snapshot/%s still says %q: %s", name, claim, why)
			}
		}
	}
}
