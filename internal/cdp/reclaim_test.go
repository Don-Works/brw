package cdp

import "testing"

func TestIsOrphanedAutomationChrome(t *testing.T) {
	const dir = "/opt/brw test/profiles/headless"
	chrome := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome --remote-debugging-address=127.0.0.1 --remote-debugging-port=51234 "
	cases := []struct {
		name    string
		ppid    int
		cmdline string
		want    bool
	}{
		{"orphaned automation chrome on this dir", 1, chrome + "--user-data-dir=" + dir + " --headless=new about:blank", true},
		{"dir as the last argument", 1, chrome + "--user-data-dir=" + dir, true},
		{"space-separated dir flag", 1, chrome + "--user-data-dir " + dir + " about:blank", true},
		{"a live daemon still owns it", 4242, chrome + "--user-data-dir=" + dir, false},
		{"a human's browser has no remote debugging", 1, "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome --user-data-dir=" + dir, false},
		{"another profile directory", 1, chrome + "--user-data-dir=" + dir + "-other", false},
		{"a parent directory of this one", 1, chrome + "--user-data-dir=/opt/brw test/profiles", false},
	}
	for _, tc := range cases {
		if got := isOrphanedAutomationChrome(tc.ppid, tc.cmdline, dir); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
