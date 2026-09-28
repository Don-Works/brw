package main

import "testing"

func TestProfileBrowserExecutableLeavesAnUnknownKindToTheLauncher(t *testing.T) {
	for _, kind := range []string{"", "netscape"} {
		if got := profileBrowserExecutable(kind); got != "" {
			t.Fatalf("kind %q resolved to %q; an unknown kind must fall through to discovery", kind, got)
		}
	}
}
