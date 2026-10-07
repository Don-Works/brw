package extensionbridge

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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
