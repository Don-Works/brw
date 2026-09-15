package chromeoptin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Both inputs come from outside brw's control: the file sits in a directory
// another program owns, and the port it names may have been taken by anything.
// Neither read may be open-ended.
func TestDiscoveryReadsAreBounded(t *testing.T) {
	t.Run("a version document that never ends", func(t *testing.T) {
		// The handler streams a string that is never closed, so a decoder with
		// no cap consumes the whole 64 MiB. The assertion is on what the server
		// managed to send: once the client stops reading at the cap the writes
		// fail, and only a socket buffer's worth gets out.
		const chunkBytes = 64 << 10
		const chunks = 1024
		written := &atomic.Int64{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			chunk := []byte(strings.Repeat("a", chunkBytes))
			if _, err := w.Write([]byte(`{"Browser":"`)); err != nil {
				return
			}
			for i := 0; i < chunks; i++ {
				n, err := w.Write(chunk)
				written.Add(int64(n))
				if err != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}))
		defer srv.Close()

		dir := t.TempDir()
		writeActivePort(t, dir, strconv.Itoa(serverPort(t, srv))+"\n/devtools/browser/fake\n")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := Discover(ctx, Options{UserDataDir: dir}); !errors.Is(err, ErrOptInOff) {
			t.Fatalf("Discover = %v, want ErrOptInOff", err)
		}
		// Generous against socket buffering, and two orders of magnitude below
		// what an uncapped decode drains.
		const tolerated = 8 << 20
		if got := written.Load(); got > tolerated {
			t.Fatalf("the fixture streamed %d bytes into discovery; the %d-byte cap should have stopped it", got, maxVersionBytes)
		}
	})

	t.Run("an active-port file that is not a regular file", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.WriteFile(target, []byte("9222\n/devtools/browser/x\n"), 0o600); err != nil {
			t.Fatalf("write target: %v", err)
		}
		if err := os.Symlink(target, filepath.Join(dir, activePortFile)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		_, err := Discover(context.Background(), Options{UserDataDir: dir})
		if !errors.Is(err, ErrOptInOff) {
			t.Fatalf("Discover = %v, want ErrOptInOff", err)
		}
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("refusal did not name what was wrong: %v", err)
		}
	})

	t.Run("an active-port file larger than the cap still parses its first line", func(t *testing.T) {
		dir := t.TempDir()
		writeActivePort(t, dir, "65535\n"+strings.Repeat("x", 1<<20))
		// Nothing is listening on 65535 here, so this refuses at the probe. The
		// point is that it reaches the probe at all: a capped read still sees
		// the line that carries the port.
		_, err := Discover(context.Background(), Options{UserDataDir: dir})
		if !errors.Is(err, ErrOptInOff) {
			t.Fatalf("Discover = %v, want ErrOptInOff", err)
		}
		if strings.Contains(err.Error(), "does not start with a port number") {
			t.Fatalf("the capped read lost the port on the first line: %v", err)
		}
	})
}
