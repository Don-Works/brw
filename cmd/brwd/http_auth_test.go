package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadBearerTokenFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(dir, "open")
	if err := os.WriteFile(open, []byte("0123456789abcdef0123456789abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"owner-only file", good, false},
		{"group-readable file", open, true},
		{"short token", short, true},
		{"symlink", link, true},
		{"relative path", "token", true},
		{"missing file", filepath.Join(dir, "missing"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readBearerTokenFile("http-token-file", tc.path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != "0123456789abcdef0123456789abcdef" {
				t.Fatalf("token not trimmed: %d bytes", len(got))
			}
		})
	}
}

func TestResolveUpstreamToken(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     string
		want    string
		wantErr bool
	}{
		{"none", "", "", false},
		{"env value", "0123456789abcdef0123456789abcdef", "0123456789abcdef0123456789abcdef", false},
		{"short env value", "abc", "", true},
		{"env with space", "0123456789abcdef 0123456789abcdef", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveUpstreamToken("", tc.env)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}

func TestHTTPBindIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:17310":   true,
		"localhost:17310":   true,
		"[::1]:17310":       true,
		"100.64.0.10:17712": false,
		":17310":            false,
		"0.0.0.0:17310":     false,
	} {
		if got := httpBindIsLoopback(addr); got != want {
			t.Errorf("%s: got %v, want %v", addr, got, want)
		}
	}
}
