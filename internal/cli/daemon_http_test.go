package cli

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
	httpapi "github.com/Don-Works/brw/internal/http"
	"github.com/Don-Works/brw/internal/readability"
)

// The fake daemon elsewhere in this package answers anything sent to it, which
// is what a wire-contract test wants and what a request-shape test cannot use:
// only internal/http's own handler rejects a query or body it does not accept.

// daemonController is the browser side of the real handler, cut down to the
// methods the routes under test reach. The embedded interface is nil, so any
// other call panics rather than quietly answering.
type daemonController struct {
	browser.Controller
	read readability.PageRead
}

func (c *daemonController) Read(context.Context) (readability.PageRead, error) {
	return c.read, nil
}

// The daemon opens a working tab for a session that named none, which every
// lease-scoped route goes through before it reaches the handler.
func (c *daemonController) OpenInGroup(context.Context, string, browser.TabGroupOptions) (browser.OpenResult, error) {
	return browser.OpenResult{Tab: browser.Tab{ID: "tab-1"}, Ready: true}, nil
}

type artifactStore struct {
	artifact.API
	id     string
	offset int64
	max    int
}

func (s *artifactStore) ReadArtifact(_ context.Context, id string, offset int64, maxBytes int) (artifact.Chunk, error) {
	s.id, s.offset, s.max = id, offset, maxBytes
	return artifact.Chunk{ArtifactID: id, Offset: offset, SizeBytes: 9, TotalBytes: 9, Text: "page text", Encoding: "utf-8"}, nil
}

func realDaemon(t *testing.T, ctrl browser.Controller, artifacts artifact.API) *httptest.Server {
	t.Helper()
	daemon := httpapi.New("", ctrl)
	if artifacts != nil {
		daemon.SetArtifactAPI(artifacts)
	}
	server := httptest.NewServer(daemon.Handler())
	t.Cleanup(server.Close)
	return server
}

// `brw read --offset N` bounds where the prose starts and nothing else. The
// route reads any bound parameter as "this read is bounded" and then applies its
// own 20000-char default to the rest, so the request has to say -1 for what the
// caller did not bound — which only the real handler can prove.
func TestReadFromAnOffsetIsNotCappedByTheRealHandler(t *testing.T) {
	const total = 60000
	ctrl := &daemonController{read: readability.PageRead{
		URL:   "https://example.test/",
		Title: "Long",
		Main:  strings.Repeat("q", total),
	}}
	server := realDaemon(t, ctrl, nil)
	t.Setenv("BRW_URL", server.URL)

	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"read", "--offset", "5000"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit=%d (stderr=%q)", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "truncated") {
		t.Fatalf("the daemon truncated a read that asked only for an offset: %q", lastLine(stdout.String()))
	}
	if got := strings.Count(stdout.String(), "q"); got != total-5000 {
		t.Fatalf("prose = %d chars, want the whole %d from the offset", got, total-5000)
	}
}

// The same page with an explicit window still comes back windowed, so the -1
// the offset case sends is not just disabling bounding everywhere.
func TestReadWithAnExplicitWindowStillBounds(t *testing.T) {
	ctrl := &daemonController{read: readability.PageRead{Main: strings.Repeat("q", 60000)}}
	server := realDaemon(t, ctrl, nil)
	t.Setenv("BRW_URL", server.URL)

	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"read", "--max-chars", "100", "--offset", "40"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit=%d (stderr=%q)", code, stderr.String())
	}
	if got := strings.Count(stdout.String(), "q"); got != 100 {
		t.Fatalf("prose = %d chars, want the 100 asked for", got)
	}
	if !strings.Contains(stdout.String(), "continue with --offset 140") {
		t.Fatalf("stdout = %q, want the paging hint", lastLine(stdout.String()))
	}
}

// The artifact routes decode a fixed schema with DisallowUnknownFields. The CLI
// has to send exactly that schema — nothing folded in from the context — or the
// daemon answers 400.
func TestArtifactReadIsAcceptedByTheRealHandler(t *testing.T) {
	store := &artifactStore{}
	server := realDaemon(t, &daemonController{}, store)
	t.Setenv("BRW_URL", server.URL)
	const id = "art_0123456789abcdef0123456789abcdef"

	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"artifact", "read", id, "--offset", "16", "--max-bytes", "64"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit=%d (stderr=%q)", code, stderr.String())
	}
	if store.id != id || store.offset != 16 || store.max != 64 {
		t.Fatalf("daemon read id=%q offset=%d max=%d, want the window asked for", store.id, store.offset, store.max)
	}
	if !strings.Contains(stdout.String(), "page text") {
		t.Fatalf("stdout = %q, want the artifact text", stdout.String())
	}
}

// --tab has no meaning on a host-local artifact handle, and the route rejects a
// tab_id it does not declare. Say so rather than sending a request that 400s.
func TestArtifactReadRejectsTab(t *testing.T) {
	store := &artifactStore{}
	server := realDaemon(t, &daemonController{}, store)
	t.Setenv("BRW_URL", server.URL)

	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--tab", "77", "artifact", "read", "art_0123456789abcdef0123456789abcdef"}, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("exit=%d, want %d (stderr=%q)", code, ExitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--tab does not apply") {
		t.Fatalf("stderr = %q, want it to name the flag", stderr.String())
	}
	if store.id != "" {
		t.Fatalf("the daemon was asked for %q; a rejected flag must not send a request", store.id)
	}
}

func lastLine(out string) string {
	trimmed := strings.Split(strings.TrimRight(out, "\n"), "\n")
	return trimmed[len(trimmed)-1]
}
