package plugin

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

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
	for name := range refused {
		if !strings.Contains(text, name) {
			t.Errorf("docs/plugins.md does not mention the refused capability %q", name)
		}
	}

	for _, phrase := range []string{"Can reach", "Cannot reach", "Never granted", "does not sandbox"} {
		if !strings.Contains(text, phrase) {
			t.Errorf("docs/plugins.md does not state %q", phrase)
		}
	}
}

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
