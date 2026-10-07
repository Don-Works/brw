package browsertest

import "testing"

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

		"the flag, through a reverse-ordered chain": `package x
func TestA(t *testing.T) {
	first := second
	second := third
	third := t.TempDir()
	_ = exec.Command(chromePath, "--headless=new", "--user-data-dir="+first)
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

		"a reclaimed profile": `package x
func TestA(t *testing.T) {
	profile := browsertest.NewProfile(t)
	_ = exec.Command(chromePath, "--headless=new", "--user-data-dir="+profile.Dir())
}`,

		"a directory testing does not own": `package x
func TestA(t *testing.T) {
	dir, _ := os.MkdirTemp("", "brw-")
	_ = exec.Command(chromePath, "--headless=new", "--user-data-dir="+dir)
}`,

		"a config that only reads the directory": `package x
func TestA(t *testing.T) {
	dir := t.TempDir()
	_, _ = Discover(ctx, Options{UserDataDir: dir})
}`,

		"the lane that starts no browser": `package x
func TestA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "would-be-profile")
	_, _ = New(ctx, Config{AttachOnly: true, UserDataDir: dir})
}`,

		"the daemon's own command line": `package x
func TestA(t *testing.T) {
	home := t.TempDir()
	_ = runBrwd(t, []string{"--remote", "auto", "--http", "off", "--user-data-dir", home})
}`,

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
