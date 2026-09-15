// Package browsertest holds the helpers shared by every test that hands a
// directory to a real browser.
//
// It exists so the profile reclaim below has one implementation. It had two
// hand-written copies and five helpers with none, and those five are how ten
// tests across four packages went red on Linux CI while the macOS gate stayed
// green.
package browsertest

import (
	"os"
	"sync"
	"testing"
	"time"
)

// A profile is reclaimed by removing it until it stays gone for quietWindow,
// giving up at reclaimBudget.
//
// The budget is generous on purpose: losing this race makes a CI run red, while
// a slow reclaim only makes it slower, and a helper that is genuinely wedged
// still fails the deadline instead of being swallowed.
const (
	reclaimBudget = 15 * time.Second
	quietWindow   = 200 * time.Millisecond
	reclaimPoll   = 25 * time.Millisecond
)

// Profile is a throwaway Chrome --user-data-dir for one test.
//
// Passing t.TempDir() straight to Chrome is the defect this type exists to
// stop. testing's TempDir cleanup is one strict RemoveAll whose error fails the
// test, and it runs the moment the test function returns. On Linux that is
// while Chrome's last helper is still writing Default/, so the unlink fails
// with "directory not empty" and the test goes red for a reason unrelated to
// anything it asserts. macOS tolerates the same race, so only CI sees it.
//
// Waiting for the browser is not enough by itself either: Manager.Close waits
// for Chrome's ROOT process, and a helper reparented away from that root can
// outlive it by a few milliseconds.
//
// NewProfile creates the directory and registers the reclaim in one step, and
// that single step is what orders the shutdown correctly: t.TempDir registered
// its cleanup first, so this one, registered second, runs first. The browser
// shutdown given to StopWith and the wait for the directory to stay gone both
// complete before testing's RemoveAll ever looks at it.
type Profile struct {
	t     *testing.T
	dir   string
	mu    sync.Mutex
	stops []func()
}

// NewProfile returns a disposable profile directory for t.
func NewProfile(t *testing.T) *Profile {
	t.Helper()
	p := &Profile{t: t, dir: t.TempDir()}
	t.Cleanup(p.reclaim)
	return p
}

// Dir is the path to give Chrome as --user-data-dir.
func (p *Profile) Dir() string { return p.dir }

// StopWith records how the browser using this profile is shut down. Stops run
// most-recently-registered first, before the directory is reclaimed, so a test
// registers one per thing that has to be torn down in order (cancel the
// chromedp context, then close the manager, say) rather than one closure that
// has to remember the order itself.
//
// A test that starts a browser on this profile and registers nothing has
// nothing sequencing Chrome's exit ahead of the reclaim; the reclaim says so by
// name when it then times out.
func (p *Profile) StopWith(stop func()) {
	if stop == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stops = append(p.stops, stop)
}

func (p *Profile) takeStops() []func() {
	p.mu.Lock()
	defer p.mu.Unlock()
	stops := p.stops
	p.stops = nil
	reversed := make([]func(), 0, len(stops))
	for i := len(stops) - 1; i >= 0; i-- {
		reversed = append(reversed, stops[i])
	}
	return reversed
}

// reclaim stops the browser and then holds the directory gone, so testing's own
// TempDir cleanup has nothing left to race.
func (p *Profile) reclaim() {
	p.t.Helper()
	stops := p.takeStops()
	for _, stop := range stops {
		stop()
	}

	deadline := time.Now().Add(reclaimBudget)
	var (
		lastErr      error
		missingSince time.Time
	)
	for {
		lastErr = os.RemoveAll(p.dir)
		_, statErr := os.Stat(p.dir)
		if lastErr == nil && os.IsNotExist(statErr) {
			if missingSince.IsZero() {
				missingSince = time.Now()
			}
			// Gone once is not gone: a straggling helper can recreate the
			// directory after the unlink. Only an interval with nothing written
			// into it says the browser has actually let go.
			if time.Since(missingSince) >= quietWindow {
				return
			}
		} else {
			missingSince = time.Time{}
			if lastErr == nil {
				lastErr = statErr
			}
		}
		if time.Now().After(deadline) {
			unsequenced := ""
			if len(stops) == 0 {
				unsequenced = "; nothing was registered with StopWith, so no browser shutdown ran before this wait"
			}
			p.t.Errorf("the throwaway Chrome profile %s was still being written %s after shutdown%s: %v",
				p.dir, reclaimBudget, unsequenced, lastErr)
			return
		}
		time.Sleep(reclaimPoll)
	}
}
