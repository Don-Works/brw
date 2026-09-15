package extensionbridge

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The bridge recognises an old extension by the text its service worker sends
// back for a message type it does not implement, and falls back instead of
// failing. That text is written in JavaScript, so the Go constant is a second
// copy and nothing but this test keeps the two in step.
//
// A reword on the JavaScript side has no loud failure mode: every capability
// probe simply stops being recognised as "your extension is older than this
// daemon" and starts surfacing as a hard error.
func TestTheExtensionStillSendsTheUnknownMessageTypeError(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	worker := filepath.Join(root, "extension", "service_worker.js")
	source, err := os.ReadFile(worker)
	if err != nil {
		t.Fatalf("read %s: %v", worker, err)
	}
	if !strings.Contains(string(source), unknownMessageType) {
		t.Fatalf("%s no longer sends %q; the bridge matches on that text to fall back for an older extension, so update both or the fallback stops firing",
			worker, unknownMessageType)
	}
}
