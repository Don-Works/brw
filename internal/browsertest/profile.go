// Package browsertest holds the helpers shared by every test that hands a directory to a real browser.
package browsertest

import (
	"os"
	"slices"
	"sync"
	"testing"
	"time"
)

const (
	reclaimBudget = 15 * time.Second
	quietWindow   = 200 * time.Millisecond
	reclaimPoll   = 25 * time.Millisecond
)

// Profile is a throwaway Chrome --user-data-dir for one test.
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

// StopWith runs registered shutdowns in reverse order before reclaiming the profile.
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
	slices.Reverse(stops)
	return stops
}

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
