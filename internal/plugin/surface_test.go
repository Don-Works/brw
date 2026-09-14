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

// A granted plugin's ENTIRE runtime surface is the credential resolver. That is
// why the capability table can be short: there is no second door to gate.
//
// This is a lock on the registry's exported method set. A Registry.Cookies, a
// Registry.NavigationPolicy or a Registry.Run would each be a way for a plugin
// to reach something the capability list says it cannot, and each would fail
// here before it could be wired to anything.
func TestAGrantedPluginsRuntimeSurfaceIsOnlyTheResolver(t *testing.T) {
	registryType := reflect.TypeOf((*Registry)(nil))
	var methods []string
	for index := 0; index < registryType.NumMethod(); index++ {
		methods = append(methods, registryType.Method(index).Name)
	}
	slices.Sort(methods)
	want := []string{"Plugins", "ProbeProvider", "Resolve", "Revoke"}
	if !slices.Equal(methods, want) {
		t.Fatalf("Registry exposes %v, want exactly %v; a new method is a new way to reach a plugin, so say why here", methods, want)
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
