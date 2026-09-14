package snapshot

import (
	"fmt"
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

// The cap is named in the error that refuses a body-keyed HAR replay of a
// capture whose request bodies were clipped. Reported from Go while the clipping
// happens in the injected script, a drift between the two would send a caller
// looking for a 2 KiB body that is really some other size.
func TestNetworkCaptureScriptCarriesTheExportedBodyCap(t *testing.T) {
	want := fmt.Sprintf("var BODY_CAP = %d;", BodyCapBytes)
	if !strings.Contains(NetworkCaptureInstallScript, want) {
		t.Fatalf("the in-page capture no longer clips at %d characters; every message that names the cap is now wrong", BodyCapBytes)
	}
}
