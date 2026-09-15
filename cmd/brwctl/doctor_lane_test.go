package main

import (
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/setup"
)

// doctor prints the lane a user is on and the capabilities that go with it. The
// profile policy can only ever select two of the lanes, so on any of the others
// a doctor that reported the policy's answer named a lane the user is not on —
// and printed somebody else's capability list beside it.
//
// Enumerated over the declared transports rather than written for the three new
// ones, so a lane added later fails here instead of being described wrongly.
func TestDoctorNamesTheLaneTheDaemonReports(t *testing.T) {
	for _, transport := range brwidentity.Transports() {
		t.Run(transport, func(t *testing.T) {
			run := &doctorRun{
				resolved: true,
				// A policy that would resolve to direct-cdp on its own, so a
				// report of anything else can only have come from the daemon.
				profile: profilepolicy.Profile{Name: "fixture", DirectCDPAllowed: true},
				health:  &daemonHealth{Identity: brwidentity.Identity{Transport: transport}},
			}
			run.checkTransport()
			if run.result.Transport != transport {
				t.Fatalf("doctor reported transport %q for a daemon on %q", run.result.Transport, transport)
			}
			if run.result.Capabilities == nil {
				t.Fatalf("doctor described no capabilities for %q", transport)
			}
			if strings.HasPrefix(run.result.Capabilities.Has, "unknown") {
				t.Errorf("doctor has no capability description for %q, so it tells the user to treat every capability as unverified", transport)
			}
		})
	}
}

// And with no daemon answering there is still the policy's lane, so an install
// that has not started yet is described rather than left blank.
func TestDoctorFallsBackToThePolicyLane(t *testing.T) {
	run := &doctorRun{
		resolved: true,
		profile:  profilepolicy.Profile{Name: "fixture", DirectCDPAllowed: true},
	}
	run.checkTransport()
	if run.result.Transport != setup.ResolvedDirectCDP {
		t.Fatalf("with no health response doctor reported %q, want the policy's %q", run.result.Transport, setup.ResolvedDirectCDP)
	}
}
