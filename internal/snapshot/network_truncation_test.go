package snapshot

import (
	"strings"
	"testing"
)

// The HAR replay identifies a clipped recording by this suffix, and the suffix
// is written by the injected script rather than by Go. Nothing else connects the
// two, so a change to either spelling would silently stop every truncated body
// from being reported and start serving half a JSON document as a whole one.
func TestNetworkCaptureScriptCarriesTheExportedTruncationMarker(t *testing.T) {
	if !strings.Contains(NetworkCaptureInstallScript, "'"+BodyTruncationMarker+"'") {
		t.Fatalf("the in-page capture no longer appends %q; the HAR replay can no longer tell a clipped body from a whole one", BodyTruncationMarker)
	}
}
