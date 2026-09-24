package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExitOnUpgradeEnabled(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		goos    string
		ppid    int
		env     map[string]string
		want    bool
		wantErr bool
	}{
		{name: "launchd agent", mode: "auto", goos: "darwin", ppid: 1, want: true},
		{name: "macOS terminal", mode: "auto", goos: "darwin", ppid: 4242},
		{name: "systemd unit", mode: "auto", goos: "linux", ppid: 900, env: map[string]string{"INVOCATION_ID": "abc"}, want: true},
		{name: "linux terminal", mode: "", goos: "linux", ppid: 1},
		{name: "forced on", mode: "on", goos: "linux", ppid: 4242, want: true},
		{name: "forced off under launchd", mode: "off", goos: "darwin", ppid: 1},
		{name: "typo", mode: "yes", goos: "darwin", ppid: 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := exitOnUpgradeEnabled(tc.mode, tc.goos, tc.ppid, func(key string) string { return tc.env[key] })
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("enabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUpgradeWatch(t *testing.T) {
	cases := []struct {
		name string
		// replace runs once the watch has recorded the starting binary.
		replace func(t *testing.T, path string)
		busy    bool
		want    bool
	}{
		{
			name:    "untouched binary",
			replace: func(*testing.T, string) {},
		},
		{
			name:    "replaced by a new file",
			replace: func(t *testing.T, path string) { replaceBinary(t, path, "new build", 0o755) },
			want:    true,
		},
		{
			name: "rewritten in place",
			replace: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("rewritten build, longer"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: true,
		},
		{
			name:    "replaced while a request is in flight",
			replace: func(t *testing.T, path string) { replaceBinary(t, path, "new build", 0o755) },
			busy:    true,
		},
		{
			name:    "replaced by a file that is not executable yet",
			replace: func(t *testing.T, path string) { replaceBinary(t, path, "half written", 0o644) },
		},
		{
			name: "removed mid-install",
			replace: func(t *testing.T, path string) {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "brwd")
			if err := os.WriteFile(path, []byte("old build"), 0o755); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
			defer cancel()
			watch := upgradeWatch{path: path, interval: 10 * time.Millisecond, settle: 2, busy: func() bool { return tc.busy }}
			done := make(chan bool, 1)
			started := make(chan struct{})
			go func() {
				close(started)
				done <- watch.run(ctx)
			}()
			<-started
			time.Sleep(30 * time.Millisecond)
			tc.replace(t, path)
			if got := <-done; got != tc.want {
				t.Fatalf("watch reported %v, want %v", got, tc.want)
			}
		})
	}
}

func replaceBinary(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	staged := path + ".new"
	if err := os.WriteFile(staged, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(staged, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, path); err != nil {
		t.Fatal(err)
	}
}
