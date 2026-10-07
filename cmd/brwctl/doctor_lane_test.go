package main

import (
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/setup"
)

func TestDoctorNamesTheLaneTheDaemonReports(t *testing.T) {
	for _, transport := range brwidentity.Transports() {
		t.Run(transport, func(t *testing.T) {
			run := &doctorRun{
				resolved: true,

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
