package main

import (
	"os"
	"path/filepath"
	"testing"
)

const testBridgeExtensionID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestChromeExtensionInstalled(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Preferences"), []byte(`{
		"extensions": {
			"settings": {
				"`+testBridgeExtensionID+`": {"state": 1}
			}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ok, source, err := chromeExtensionInstalled(dir, testBridgeExtensionID)
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
	got := quoteRemote("~/Library/Application Support/brw/bin/brwd")
	want := `"$HOME/Library/Application Support/brw/bin/brwd"`
	if got != want {
		t.Fatalf("quoteRemote = %q, want %q", got, want)
	}
}
