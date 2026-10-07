package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/chromeoptin"
	"github.com/Don-Works/brw/internal/setup"
)

type doctorChromeOptIn struct {
	Available   bool   `json:"available"`
	Endpoint    string `json:"endpoint,omitempty"`
	Browser     string `json:"browser,omitempty"`
	UserDataDir string `json:"user_data_dir,omitempty"`
	Action      string `json:"action,omitempty"`
}

func (d *doctorRun) inspectPageCommand() string {
	browser := "chrome"
	if d.resolved && d.profile.Kind != "" {
		browser = d.profile.Kind
	}
	goos := d.req.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	note := "then turn on remote debugging; brw cannot turn it on for you"

	if goos == "darwin" {
		return fmt.Sprintf("open -a %q 'chrome://inspect/#remote-debugging'   # %s", setup.BrowserDisplayName(browser), note)
	}
	exe := d.result.BrowserExecutable
	if exe == "" {
		exe = browser
	}
	return exe + " 'chrome://inspect/#remote-debugging'   # " + note
}

func (d *doctorRun) checkChromeOptIn() {
	if d.req.SkipLiveChecks {
		d.add(checkSkip, "chrome_opt_in", "chrome remote-debugging opt-in",
			"live checks skipped, so nothing here says whether the opt-in is on", "")
		return
	}
	browser := "chrome"
	if d.resolved && d.profile.Kind != "" {
		browser = d.profile.Kind
	}
	goos := d.req.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	dir := d.req.ChromeOptInUserDataDir
	if dir == "" {
		dir = chromeoptin.DefaultUserDataDir(goos, browser)
	}
	onThisLane := d.health != nil && d.health.Identity.Transport == brwidentity.TransportChromeOptIn

	if dir == "" {
		d.result.ChromeOptIn = &doctorChromeOptIn{Action: chromeoptin.UserAction}
		status := checkSkip
		if onThisLane {
			status = checkFail
		}
		d.add(status, "chrome_opt_in", "chrome remote-debugging opt-in",
			fmt.Sprintf("brw does not know where %s keeps profiles on %s, so it cannot look for the opt-in endpoint", browser, goos),
			"brwd --chrome-opt-in --chrome-opt-in-user-data-dir <path>")
		return
	}

	endpoint, err := chromeoptin.Discover(context.Background(), chromeoptin.Options{
		UserDataDir: dir,
		Client:      d.client,
	})
	if err == nil {
		d.result.ChromeOptIn = &doctorChromeOptIn{
			Available:   true,
			Endpoint:    endpoint.HTTPURL,
			Browser:     endpoint.Browser,
			UserDataDir: endpoint.UserDataDir,
		}
		detail := fmt.Sprintf("on: %s exposes browser-target CDP at %s", endpoint.BrowserLabel(), endpoint.HTTPURL)
		if !onThisLane {
			detail += "; run brwd --chrome-opt-in to drive it (incognito and HttpOnly cookies against your signed-in profile; brw_state and brw_set_download_path stay refused there). Chrome 144+ asks you to approve each debugging connection in the browser window"
		}
		d.add(checkOK, "chrome_opt_in", "chrome remote-debugging opt-in", detail, "")
		return
	}

	d.result.ChromeOptIn = &doctorChromeOptIn{UserDataDir: dir, Action: chromeoptin.UserAction}
	switch {
	case errors.Is(err, chromeoptin.ErrChromeTooOld):

		d.add(checkSkip, "chrome_opt_in", "chrome remote-debugging opt-in",
			err.Error(), "")
	case onThisLane:
		d.add(checkFail, "chrome_opt_in", "chrome remote-debugging opt-in",
			"this daemon runs on the opt-in lane, but the opt-in is off: "+err.Error(),
			d.inspectPageCommand())
	default:
		d.add(checkSkip, "chrome_opt_in", "chrome remote-debugging opt-in",
			"off, which is the default. Turning it on adds a third lane: your signed-in Chrome with full browser-target CDP, no extension",
			d.inspectPageCommand())
	}
}
