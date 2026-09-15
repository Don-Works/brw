package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	cdplaunch "github.com/Don-Works/brw/internal/cdp"
)

// A launch that asks for a plugin-supplied browser AND for something only a
// browser on this machine can do has to fail by name. Each of these resolves
// the same way if it is not refused: the flag is ignored and the run goes ahead
// on a fresh, unauthenticated cloud browser.
func TestEveryLaunchThatConflictsWithAProviderIsRefusedByName(t *testing.T) {
	for name, test := range map[string]struct {
		launch  providerLaunch
		wantErr string
	}{
		"nothing conflicting": {providerLaunch{}, ""},
		"bridge":              {providerLaunch{Bridge: true}, "extension bridge"},
		"upstream http":       {providerLaunch{UpstreamHTTP: "http://127.0.0.1:17410"}, "browser-host daemon"},
		"remote endpoint":     {providerLaunch{Config: browser.Config{RemoteURL: "http://127.0.0.1:9222"}}, "which one you meant"},
		"login":               {providerLaunch{Login: true}, "profile reuse"},
		"profile":             {providerLaunch{Profile: "client-a-chrome"}, "profile reuse"},
		"workspace":           {providerLaunch{Workspace: "client-a"}, "profile reuse"},
		"headless":            {providerLaunch{Headless: true}, "decided over there"},
		"extension":           {providerLaunch{Config: browser.Config{Extensions: []string{"/tmp/fixture-ext"}}}, "this machine's filesystem"},
		"chrome arg":          {providerLaunch{Config: browser.Config{ChromeArgs: []string{"--mute-audio"}}}, "not launching Chrome"},
		"proxy": {
			providerLaunch{Config: browser.Config{Network: cdplaunch.NetworkEnvironment{ProxyServer: "http://127.0.0.1:8080"}}},
			"launch switches",
		},
		"certificate policy": {
			providerLaunch{Config: browser.Config{Network: cdplaunch.NetworkEnvironment{IgnoreHTTPSErrors: true}}},
			"launch switches",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := refuseWithProvider(test.launch)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("refuseWithProvider = %v, want the launch accepted", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("refuseWithProvider accepted a launch that conflicts with a provider: %+v", test.launch)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("refuseWithProvider = %q, want a reason containing %q", err, test.wantErr)
			}
			if !strings.Contains(err.Error(), "browser.provider") {
				t.Errorf("refuseWithProvider = %q, want it to name the capability that caused the conflict", err)
			}
		})
	}
}

// Three of the refusals are the shared capability table's, so the message an
// operator reads here is the same one an agent reads from a tool error. A
// second spelling of "why not" is how the two drift.
func TestProviderRefusalsCarryTheSharedCapabilityClass(t *testing.T) {
	for _, launch := range []providerLaunch{{Bridge: true}, {Login: true}, {Profile: "p"}, {Workspace: "w"}} {
		err := refuseWithProvider(launch)
		if !errors.Is(err, browser.ErrRemoteTargetUnsupported) {
			t.Errorf("refuseWithProvider(%+v) = %v, which is not the remote-capability class", launch, err)
		}
	}
}

// Every field of the launch struct has to be something the table looks at, or
// it is a conflict this daemon collects and never checks.
func TestEveryProviderLaunchFieldIsCheckedBySomething(t *testing.T) {
	launchType := reflect.TypeOf(providerLaunch{})
	// Config is checked field by field above rather than as a whole.
	checked := map[string]providerLaunch{
		"Bridge":       {Bridge: true},
		"UpstreamHTTP": {UpstreamHTTP: "http://127.0.0.1:17410"},
		"ChromeOptIn":  {ChromeOptIn: true},
		"Login":        {Login: true},
		"Headless":     {Headless: true},
		"Profile":      {Profile: "p"},
		"Workspace":    {Workspace: "w"},
		"Config":       {Config: browser.Config{RemoteURL: "http://127.0.0.1:9222"}},
	}
	for index := 0; index < launchType.NumField(); index++ {
		field := launchType.Field(index).Name
		launch, ok := checked[field]
		if !ok {
			t.Errorf("providerLaunch.%s is collected and never checked; add it to refuseWithProvider or drop the field", field)
			continue
		}
		if err := refuseWithProvider(launch); err == nil {
			t.Errorf("providerLaunch.%s set produced no refusal", field)
		}
	}
}
