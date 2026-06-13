package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/revitt/agent-browser/internal/profilepolicy"
)

func TestChromeExtensionInstalled(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Preferences"), []byte(`{
		"extensions": {
			"settings": {
				"`+profilepolicy.DefaultBridgeExtensionID+`": {"state": 1}
			}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ok, source, err := chromeExtensionInstalled(dir, profilepolicy.DefaultBridgeExtensionID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected extension to be detected")
	}
	if source == "" {
		t.Fatal("expected source path")
	}
}

func TestQuoteRemoteHomePathWithSpaces(t *testing.T) {
	got := quoteRemote("~/Library/Application Support/agent-browser/bin/agent-browserd")
	want := `"$HOME/Library/Application Support/agent-browser/bin/agent-browserd"`
	if got != want {
		t.Fatalf("quoteRemote = %q, want %q", got, want)
	}
}
