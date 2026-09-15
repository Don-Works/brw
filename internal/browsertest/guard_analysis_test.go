package browsertest

import "testing"

// The guard above is worth only what it catches, and what it catches is now an
// analysis rather than a pair of regexps. These are the spellings it has to see
// and the ones it has to leave alone, written out so a later widening cannot
// quietly stop matching one of them — which is exactly how the site that reached
// CI got past the two spellings the first version knew.
func TestTheGuardSeesEverySpellingOfTheDefect(t *testing.T) {
	caught := map[string]string{
		"the field, inline": `package x
func TestA(t *testing.T) { _ = Config{UserDataDir: t.TempDir()} }`,

		"chromedp's option, inline": `package x
func TestA(t *testing.T) { _ = chromedp.UserDataDir(t.TempDir()) }`,

		"chromedp's option, through a name": `package x
func TestA(t *testing.T) {
	dir := t.TempDir()
	_ = chromedp.UserDataDir(dir)
}`,

		// The site that went red on CI: the value reaches the browser as a
		// command-line flag, a line away from the t.TempDir() that made it.
		"the flag, concatenated": `package x
func TestA(t *testing.T) {
	dir := t.TempDir()
	_ = exec.Command(chromePath, "--headless=new", "--user-data-dir="+dir, "about:blank")
}`,

		"the flag, as two arguments": `package x
func TestA(t *testing.T) {
	dir := t.TempDir()
	_ = exec.Command(chromePath, "--headless=new", "--user-data-dir", dir)
}`,

		"the flag, in a slice built elsewhere": `package x
func TestA(t *testing.T) {
	dir := t.TempDir()
	args := []string{"--remote-debugging-pipe", "--user-data-dir=" + dir, "--no-first-run"}
	_ = args
}`,

		"the flag, through a join": `package x
func TestA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	_ = exec.Command(chromePath, "--headless=new", "--user-data-dir="+dir)
}`,

		"the field on a launch config, through a name": `package x
func TestA(t *testing.T) {
	dir := t.TempDir()
	_, _ = New(ctx, Config{UserDataDir: dir})
}`,
	}
	for name, source := range caught {
		t.Run(name, func(t *testing.T) {
			sinks, err := profileSinksInSource("caught_test.go", []byte(source))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(sinks) == 0 {
				t.Error("the guard does not see this spelling; a test written this way reaches CI before anyone finds out")
			}
		})
	}

	allowed := map[string]string{
		// browsertest.NewProfile IS the fix, so its directory must not be read
		// as the defect.
		"a reclaimed profile": `package x
func TestA(t *testing.T) {
	profile := browsertest.NewProfile(t)
	_ = exec.Command(chromePath, "--headless=new", "--user-data-dir="+profile.Dir())
}`,

		// os.MkdirTemp has no strict cleanup to race: the removal is the test's
		// own and its error is nobody's failure.
		"a directory testing does not own": `package x
func TestA(t *testing.T) {
	dir, _ := os.MkdirTemp("", "brw-")
	_ = exec.Command(chromePath, "--headless=new", "--user-data-dir="+dir)
}`,

		// The same field name on a reader: Discover goes LOOKING in the
		// directory, and no browser is writing it because of the test.
		"a config that only reads the directory": `package x
func TestA(t *testing.T) {
	dir := t.TempDir()
	_, _ = Discover(ctx, Options{UserDataDir: dir})
}`,

		// The attach-only lane refuses to start a browser at all, which is what
		// the test using this spelling asserts.
		"the lane that starts no browser": `package x
func TestA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "would-be-profile")
	_, _ = New(ctx, Config{AttachOnly: true, UserDataDir: dir})
}`,

		// brwd carries the flag through to a config; the browser on that lane is
		// somebody else's, already running, somewhere else.
		"the daemon's own command line": `package x
func TestA(t *testing.T) {
	home := t.TempDir()
	_ = runBrwd(t, []string{"--remote", "auto", "--http", "off", "--user-data-dir", home})
}`,

		// An unpacked extension is read, not written.
		"a directory handed to the browser to read": `package x
func TestA(t *testing.T) {
	profile := browsertest.NewProfile(t)
	extension := t.TempDir()
	_ = exec.Command(chromePath, "--headless=new", "--load-extension="+extension, "--user-data-dir="+profile.Dir())
}`,
	}
	for name, source := range allowed {
		t.Run(name, func(t *testing.T) {
			sinks, err := profileSinksInSource("allowed_test.go", []byte(source))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(sinks) > 0 {
				t.Errorf("the guard reports %d finding(s) here; a guard that cries wolf gets worked around rather than fixed", len(sinks))
			}
		})
	}
}
