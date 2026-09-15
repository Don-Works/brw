package plugin

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The capability table in docs/plugins.md is the operator-facing half of the
// gate: it is where someone decides whether to install a plugin at all. A
// capability the code knows and the doc does not is a grant nobody reviewed, so
// the doc is checked against the code rather than by reading it.
func TestPluginDocsNameEveryCapabilityTheGateKnows(t *testing.T) {
	doc, err := os.ReadFile("../../docs/plugins.md")
	if err != nil {
		t.Fatalf("read docs/plugins.md: %v", err)
	}
	text := string(doc)
	for _, name := range GrantableCapabilities() {
		if !strings.Contains(text, name) {
			t.Errorf("docs/plugins.md does not mention the grantable capability %q", name)
		}
	}
	for name := range reserved {
		if !strings.Contains(text, name) {
			t.Errorf("docs/plugins.md does not mention the reserved capability %q", name)
		}
	}
	for name := range refused {
		if !strings.Contains(text, name) {
			t.Errorf("docs/plugins.md does not mention the refused capability %q", name)
		}
	}
	// The doc has to say the thing the brief asked it to say: what each one can
	// and cannot reach. A table of names with no boundary is not that.
	for _, phrase := range []string{"Can reach", "Cannot reach", "Never granted", "does not sandbox"} {
		if !strings.Contains(text, phrase) {
			t.Errorf("docs/plugins.md does not state %q", phrase)
		}
	}
}

// A capability section that lists what a plugin cannot reach, and leaves the
// sandboxing caveat 40 lines further down under its own heading, reads as a
// property of the plugin process. It is not one: brw does not sandbox an exec
// provider, so the list is what brw HANDS a provider, and an operator deciding
// whether to install one has to see both facts in the same place.
func TestEveryGrantedCapabilitySectionSaysWhatItDoesNotCover(t *testing.T) {
	for _, capability := range GrantableCapabilities() {
		t.Run(capability, func(t *testing.T) {
			section := docSection(t, "### `"+capability+"`")
			for _, phrase := range []string{"does not sandbox", "daemon's user"} {
				if !strings.Contains(section, phrase) {
					t.Errorf("the %s section does not say %q, so its boundary list reads as a property of the plugin process", capability, phrase)
				}
			}
			for _, phrase := range []string{"Can reach", "Cannot reach"} {
				if !strings.Contains(section, phrase) {
					t.Errorf("the %s section does not state %q", capability, phrase)
				}
			}
		})
	}
}

// The browser.provider section is the operator-facing half of the remote
// capability table. A refusal the code enforces and the doc does not name is a
// boundary an operator finds out about from a failed run.
func TestTheBrowserProviderSectionNamesWhatARemoteBrowserCannotDo(t *testing.T) {
	section := docSection(t, "### `"+CapabilityBrowserProvider+"`")
	for _, phrase := range []string{
		"profile reuse", "extension bridge", "print-renderer", "profile_session",
		"local downloads", "local uploads", "clipboard",
		"stdin", "expires_in_ms", "{session}", "userinfo", "remote-cdp",
	} {
		if !strings.Contains(section, phrase) {
			t.Errorf("the browser.provider section does not mention %q", phrase)
		}
	}
}

// docSection returns one heading's body with whitespace collapsed, so a phrase
// that straddles a markdown line wrap still matches.
func docSection(t *testing.T, heading string) string {
	t.Helper()
	doc, err := os.ReadFile("../../docs/plugins.md")
	if err != nil {
		t.Fatalf("read docs/plugins.md: %v", err)
	}
	start := strings.Index(string(doc), heading)
	if start < 0 {
		t.Fatalf("docs/plugins.md has no %s section", heading)
	}
	section := string(doc)[start+len(heading):]
	if end := strings.Index(section, "\n### "); end >= 0 {
		section = section[:end]
	}
	return strings.Join(strings.Fields(section), " ")
}

// A granted plugin's ENTIRE runtime surface is the credential resolver and the
// browser-session opener. That is why the capability table can be short: there
// is no third door to gate.
//
// This is a lock on the registry's exported method set. A Registry.Cookies, a
// Registry.NavigationPolicy or a Registry.Run would each be a way for a plugin
// to reach something the capability list says it cannot, and each would fail
// here before it could be wired to anything.
//
// OpenBrowserSession and ProbeBrowserProvider are the browser.provider half,
// and they mirror Resolve and ProbeProvider exactly: ask, or ask whether asking
// would work. Neither hands the plugin anything of brw's.
func TestAGrantedPluginsRuntimeSurfaceIsOnlyTheResolverAndTheBrowserOpener(t *testing.T) {
	registryType := reflect.TypeOf((*Registry)(nil))
	var methods []string
	for index := 0; index < registryType.NumMethod(); index++ {
		methods = append(methods, registryType.Method(index).Name)
	}
	slices.Sort(methods)
	want := []string{"OpenBrowserSession", "Plugins", "ProbeBrowserProvider", "ProbeProvider", "Resolve", "Revoke"}
	if !slices.Equal(methods, want) {
		t.Fatalf("Registry exposes %v, want exactly %v; a new method is a new way to reach a plugin, so say why here", methods, want)
	}
}

// The browser provider interface is the same shape of promise the credential
// one makes: a plugin is ASKED for something and told to release it. It is
// handed a context and, internally, a resolved credential — never a structure
// it could read brw state out of, and never a way to call back into brw.
func TestTheBrowserProviderInterfaceOffersNoWayToAskBrwForAnything(t *testing.T) {
	providerType := reflect.TypeOf((*browserProvider)(nil)).Elem()
	var methods []string
	for index := 0; index < providerType.NumMethod(); index++ {
		methods = append(methods, providerType.Method(index).Name)
	}
	slices.Sort(methods)
	if !slices.Equal(methods, []string{"open", "release"}) {
		t.Fatalf("browserProvider exposes %v, want exactly [open release]", methods)
	}
	for _, method := range methods {
		m, _ := providerType.MethodByName(method)
		for index := 0; index < m.Type.NumIn(); index++ {
			switch kind := m.Type.In(index).Kind(); kind {
			case reflect.String, reflect.Interface, reflect.Struct:
			default:
				t.Errorf("browserProvider.%s argument %d is a %s; a provider takes a context, a name and a value, never something it could read brw out of", method, index, kind)
			}
		}
	}
}

// The credential provider interface is what a granted plugin implements. It
// takes a reference name and returns a value: a plugin has no way to ask brw
// for a page, a cookie, a policy, or anything else.
func TestTheCredentialProviderInterfaceOffersNoWayToAskBrwForAnything(t *testing.T) {
	providerType := reflect.TypeOf((*credentialProvider)(nil)).Elem()
	if providerType.NumMethod() != 1 {
		t.Fatalf("credentialProvider has %d methods; it must have exactly one", providerType.NumMethod())
	}
	method := providerType.Method(0)
	if method.Name != "resolve" {
		t.Fatalf("credentialProvider method is %q", method.Name)
	}
	if got := method.Type.NumIn(); got != 2 {
		t.Fatalf("resolve takes %d arguments; it must take a context and a reference name only", got)
	}
	if got := method.Type.In(1).Kind(); got != reflect.String {
		t.Fatalf("resolve's second argument is a %s; a provider is handed a name, never a structure it could read brw state out of", got)
	}
}
