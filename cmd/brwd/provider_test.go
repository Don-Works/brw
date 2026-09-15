package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwidentity"
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
		// Refused HERE, not only inside browser.New: checkRemoteConfig runs after
		// the operator's mint program has executed and the provider has billed a
		// session.
		"debugging port": {providerLaunch{Config: browser.Config{Port: 9222}}, "no debugging port"},
		"real profile":   {providerLaunch{Config: browser.Config{AllowRealProfile: true}}, "profile reuse"},
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
	// Config's own fields are checked one by one. It gets more than one case
	// because a single representative proves only that SOME field of Config is
	// read: Port lived inside Config, was satisfied by the RemoteURL case, and
	// was refused nowhere in this table until a reviewer went looking.
	checked := map[string][]providerLaunch{
		"Bridge":       {{Bridge: true}},
		"UpstreamHTTP": {{UpstreamHTTP: "http://127.0.0.1:17410"}},
		"ChromeOptIn":  {{ChromeOptIn: true}},
		"Login":        {{Login: true}},
		"Headless":     {{Headless: true}},
		"Profile":      {{Profile: "p"}},
		"Workspace":    {{Workspace: "w"}},
		"Config": {
			{Config: browser.Config{RemoteURL: "http://127.0.0.1:9222"}},
			{Config: browser.Config{Port: 9222}},
			{Config: browser.Config{AllowRealProfile: true}},
			{Config: browser.Config{Extensions: []string{"/tmp/fixture-ext"}}},
			{Config: browser.Config{ChromeArgs: []string{"--mute-audio"}}},
			{Config: browser.Config{Network: cdplaunch.NetworkEnvironment{ProxyServer: "http://127.0.0.1:8080"}}},
		},
	}
	for index := 0; index < launchType.NumField(); index++ {
		field := launchType.Field(index).Name
		launches, ok := checked[field]
		if !ok {
			t.Errorf("providerLaunch.%s is collected and never checked; add it to refuseWithProvider or drop the field", field)
			continue
		}
		for _, launch := range launches {
			if err := refuseWithProvider(launch); err == nil {
				t.Errorf("providerLaunch.%s set to %+v produced no refusal", field, launch)
			}
		}
	}
}

// configFieldsReadOffTheFlagInstead are the settings the startup table does not
// read from the config, because the config carries a DEFAULT for them that
// describes this machine: refusing on the default would make a provider
// unusable without also passing a flag to unset one. Each names the launch
// shape that IS refused when the operator asked for it explicitly.
var configFieldsReadOffTheFlagInstead = map[string]providerLaunch{
	"UserDataDir":      {Profile: "p"},
	"ProfileDirectory": {Profile: "p"},
	"Headless":         {Headless: true},
	// Both of these are produced by the Chrome opt-in discovery rather than
	// typed: BrowserWSURL is the endpoint it resolved, and SignedInProfile is
	// the marker it stamps on the config. --chrome-opt-in is what an operator
	// actually passes, so that is what startup refuses.
	"BrowserWSURL":    {ChromeOptIn: true},
	"SignedInProfile": {ChromeOptIn: true},
}

// probeConfigField returns a non-zero value for one browser.Config field, so
// the enumeration below can ask the manager's own table about each field on its
// own rather than trusting a hand-written list of the fields that table reads.
func probeConfigField(field reflect.StructField) (reflect.Value, bool) {
	switch field.Type.Kind() {
	case reflect.String:
		return reflect.ValueOf("http://127.0.0.1:9222").Convert(field.Type), true
	case reflect.Bool:
		return reflect.ValueOf(true), true
	case reflect.Int, reflect.Int64:
		return reflect.ValueOf(int64(9222)).Convert(field.Type), true
	case reflect.Slice:
		return reflect.ValueOf([]string{"/tmp/fixture"}).Convert(field.Type), true
	case reflect.Struct:
		return reflect.ValueOf(cdplaunch.NetworkEnvironment{ProxyServer: "http://127.0.0.1:8080"}), true
	}
	return reflect.Value{}, false
}

// Every setting of browser.Config the MANAGER's own table refuses has to be
// refused at STARTUP too. The inner gate runs inside browser.New, after the
// mint program has run and the provider has billed a session, and its failure
// then also has to unwind a session brw already holds.
//
// Enumerated by reflecting over browser.Config and asking
// browser.ProviderConfigProblems — the very function checkRemoteConfig applies —
// about each field one at a time. A hand-written list of cases here would cover
// the fields that table reads TODAY and stay green for a field added to it
// tomorrow and forgotten in refuseWithProvider, which is the "satisfied by one
// representative" defect that hid --remote-debugging-port.
func TestConfigFieldsRefusedByTheManagerAreAlsoRefusedAtStartup(t *testing.T) {
	configType := reflect.TypeOf(browser.Config{})
	enumerated := 0
	for index := 0; index < configType.NumField(); index++ {
		field := configType.Field(index)
		if field.Name == "Remote" {
			// Remote IS the plugin-supplied browser, not a setting that
			// conflicts with one, and there is no RemoteTarget at startup.
			continue
		}
		probe, ok := probeConfigField(field)
		if !ok {
			t.Errorf("browser.Config.%s is a %s this test cannot set, so nothing asks the manager's table about it; add a case to probeConfigField", field.Name, field.Type)
			continue
		}
		cfg := browser.Config{}
		reflect.ValueOf(&cfg).Elem().Field(index).Set(probe)
		if browser.ProviderConfigProblems(cfg) == nil {
			// Not a setting the manager refuses; startup has nothing to mirror.
			continue
		}
		enumerated++
		t.Run(field.Name, func(t *testing.T) {
			launch := providerLaunch{Config: cfg}
			if alternate, ok := configFieldsReadOffTheFlagInstead[field.Name]; ok {
				launch = alternate
			}
			if err := refuseWithProvider(launch); err == nil {
				t.Fatalf("browser.Config.%s is refused by the manager and accepted at startup, so the refusal lands only after the provider has billed a session", field.Name)
			}
		})
	}
	if enumerated == 0 {
		t.Fatal("the probe found no refused field, so this test asserts nothing")
	}
	// A stale exemption is as bad as a missing case: it excuses a field from the
	// config half of the check for a reason that no longer applies.
	for name, launch := range configFieldsReadOffTheFlagInstead {
		field, ok := configType.FieldByName(name)
		if !ok {
			t.Errorf("configFieldsReadOffTheFlagInstead names %q, which is not a field of browser.Config", name)
			continue
		}
		probe, _ := probeConfigField(field)
		cfg := browser.Config{}
		reflect.ValueOf(&cfg).Elem().FieldByName(name).Set(probe)
		if browser.ProviderConfigProblems(cfg) == nil {
			t.Errorf("browser.Config.%s is exempted from the config half of the startup check, but the manager no longer refuses it", name)
		}
		if err := refuseWithProvider(launch); err == nil {
			t.Errorf("the flag that stands in for browser.Config.%s (%+v) is not refused at startup either", name, launch)
		}
	}
}

// The identity a provider-backed launch actually produces. It never runs the
// profile-policy block (--profile and --workspace are refused with a provider),
// so every field naming a profile on this machine is empty — and an identity
// guard pinned to a workspace fails against it.
func TestTheIdentityAProviderLaunchReportsCarriesNoProfile(t *testing.T) {
	identity := resolveIdentity(identityInputs{BrowserProvider: true})
	if identity.Workspace != "" || identity.Profile != "" || identity.UserDataDir != "" || identity.ProfileDirectory != "" {
		t.Fatalf("a provider-backed daemon reported a profile: %+v", identity)
	}
	if identity.Mode != "browser-provider" {
		t.Errorf("mode = %q, want browser-provider", identity.Mode)
	}
	if identity.Transport != brwidentity.TransportOffHostCDP {
		t.Errorf("transport = %q, want %q", identity.Transport, brwidentity.TransportOffHostCDP)
	}
	// The fail-closed half: a proxy pinned to a workspace must not accept it.
	if got := identity.Mismatches(brwidentity.Identity{Workspace: "client-a"}); len(got) != 1 {
		t.Fatalf("a workspace pin against a provider-backed daemon = %v, want it refused", got)
	}
	if got := identity.Mismatches(brwidentity.Identity{Profile: "client-a-chrome"}); len(got) != 1 {
		t.Fatalf("a profile pin against a provider-backed daemon = %v, want it refused", got)
	}
	// And the local lanes still report what they always did.
	for _, test := range []struct {
		name  string
		in    identityInputs
		mode  string
		trans string
	}{
		{"direct", identityInputs{}, "direct", brwidentity.TransportDirectCDP},
		{"bridge", identityInputs{Bridge: true}, "bridge", brwidentity.TransportExtensionBridge},
		{"proxy", identityInputs{UpstreamHTTP: "http://127.0.0.1:17410"}, "upstream-http", ""},
		// --remote and the Chrome opt-in are local lanes too, and both attach to
		// a browser brw did not start. Neither may be reported as the provider's
		// lane: that one refuses this machine's disk, clipboard and session
		// store, and these three do not.
		{"remote", identityInputs{RemoteURL: "http://127.0.0.1:9222"}, "remote", brwidentity.TransportRemoteCDP},
		{"chrome opt-in", identityInputs{ChromeOptIn: true}, "chrome-opt-in", brwidentity.TransportChromeOptIn},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := resolveIdentity(test.in)
			if got.Mode != test.mode || got.Transport != test.trans {
				t.Fatalf("resolveIdentity(%+v) = mode %q transport %q, want %q and %q", test.in, got.Mode, got.Transport, test.mode, test.trans)
			}
		})
	}
}

// A provider-backed daemon must not resolve to the same on-disk scope as a
// local daemon started without a profile. Both have an identity with no profile
// fields, and that scope is where the artifact store and the session-snapshot
// store live — the store brw_state reads.
func TestAProviderBackedDaemonDoesNotShareTheDefaultStoreScope(t *testing.T) {
	local := runtimeScopeDir(brwidentity.Identity{})
	provider := runtimeScopeDir(brwidentity.Identity{Transport: brwidentity.TransportOffHostCDP})
	if local != "default" {
		t.Fatalf("a local daemon with no profile = %q, want the unchanged %q scope", local, "default")
	}
	if provider == local {
		t.Fatalf("a provider-backed daemon resolves to %q, the same store as a local daemon with no profile", provider)
	}
	// The local lanes keep the scope they already have on disk: moving them
	// would orphan every existing artifact and snapshot.
	for _, transport := range []string{"", brwidentity.TransportDirectCDP, brwidentity.TransportExtensionBridge, brwidentity.TransportRemoteCDP, brwidentity.TransportChromeOptIn} {
		if got := runtimeScopeDir(brwidentity.Identity{Transport: transport}); got != "default" {
			t.Errorf("transport %q = %q, want the store it already has", transport, got)
		}
	}
	// And a named profile still scopes by the profile rather than by transport.
	named := brwidentity.Identity{Workspace: "client-a", Profile: "client-a-chrome"}
	if runtimeScopeDir(named) == "default" {
		t.Error("a named profile resolved to the default scope")
	}
	if a, b := runtimeScopeDir(named), runtimeScopeDir(brwidentity.Identity{Workspace: "client-b", Profile: "client-b-chrome"}); a == b {
		t.Error("two different profiles resolved to the same scope")
	}
}
