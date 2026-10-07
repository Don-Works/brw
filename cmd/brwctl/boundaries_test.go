package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteQuotingTreatsHomePathSuffixAsData(t *testing.T) {
	for _, raw := range []string{"~/path/$BRW_QUOTE_DATA", "~/path/$(printf changed)", "~/path/`printf changed`", "~/path/quote\"and'space"} {
		cmd := exec.Command("sh", "-c", "printf '%s' "+quoteRemote(raw))
		got, err := cmd.Output()
		want := os.Getenv("HOME") + "/" + strings.TrimPrefix(raw, "~/")
		if err != nil || string(got) != want {
			t.Errorf("quoted path interpreted: got %q err %v want %q", got, err, want)
		}
	}
}

func TestShellJoinQuotesAssignmentShapedArguments(t *testing.T) {
	raw := "BRW_ARG=$(printf changed)"
	got, err := exec.Command("sh", "-c", "printf '%s' "+shellJoin([]string{raw})).Output()
	if err != nil || string(got) != raw {
		t.Fatalf("assignment-shaped argument interpreted: %q %v", got, err)
	}
}

func TestFetchJSONRejectsTrailingOrOversizedDocuments(t *testing.T) {
	for _, body := range []string{`{"ok":true} {"ok":false}`, `{"ok":true} invalid`, `{"ok":true}` + strings.Repeat(" ", 1<<20)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		var result daemonHealth
		err := fetchJSON(server.Client(), server.URL, &result)
		server.Close()
		if err == nil {
			t.Errorf("ambiguous or oversized document accepted: %d bytes", len(body))
		}
	}
}

type brokenOutput struct{}

func (brokenOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestDoctorPropagatesJSONOutputFailure(t *testing.T) {
	if err := reportDoctor(brokenOutput{}, doctorResult{OK: true}, true); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("output failure lost: %v", err)
	}
}

func TestExtractTarRejectsOversizedEntryBeforeCreatingIt(t *testing.T) {
	var raw bytes.Buffer
	zip := gzip.NewWriter(&raw)
	archive := tar.NewWriter(zip)
	if err := archive.WriteHeader(&tar.Header{Name: "oversized.bin", Mode: 0o600, Size: maxArchiveBytes + 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path := filepath.Join(root, "large.tar.gz")
	if err := os.WriteFile(path, raw.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "unpack")
	if err := extractTarGz(path, dest); err == nil {
		t.Fatal("oversized entry accepted")
	}
	if _, err := os.Stat(filepath.Join(dest, "oversized.bin")); !os.IsNotExist(err) {
		t.Fatalf("oversized entry opened before refusal: %v", err)
	}
}
