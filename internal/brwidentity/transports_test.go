package brwidentity

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestEveryTransportConstantIsClassified(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	fset := token.NewFileSet()
	found := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, ident := range value.Names {
					if !strings.HasPrefix(ident.Name, "Transport") || i >= len(value.Values) {
						continue
					}
					lit, ok := value.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					unquoted, err := strconv.Unquote(lit.Value)
					if err != nil {
						continue
					}
					found[ident.Name] = unquoted
				}
			}
		}
	}
	if len(found) == 0 {
		t.Fatal("found no Transport* constants to check; the scan is broken, not the table")
	}
	for name, value := range found {
		if !KnownTransport(value) {
			t.Errorf("constant %s = %q has no row in transportCapabilities, so no tool's availability on that lane is defined", name, value)
		}
	}
	if len(found) != len(Transports()) {
		t.Errorf("%d Transport* constants but %d classified transports: %v vs %v", len(found), len(Transports()), found, Transports())
	}
}

func TestTransportCapabilitiesDistinguishTheLanes(t *testing.T) {
	want := map[string]TransportCapabilities{
		TransportDirectCDP:       {CDPSession: true, BrowserTarget: true, RuntimeDownloadRouting: true, BrowserOnThisHost: true},
		TransportRemoteCDP:       {CDPSession: true, BrowserTarget: true, BrowserOnThisHost: true},
		TransportChromeOptIn:     {CDPSession: true, BrowserTarget: true, SignedInProfile: true, BrowserOnThisHost: true},
		TransportExtensionBridge: {ExtensionAPIs: true, SignedInProfile: true, BrowserOnThisHost: true},

		TransportOffHostCDP: {CDPSession: true, BrowserTarget: true},
	}
	for _, transport := range Transports() {
		expected, stated := want[transport]
		if !stated {
			t.Errorf("transport %q has no expected capability row here; state what that lane can do rather than letting it inherit whatever the table says", transport)
			continue
		}
		got, ok := Capabilities(transport)
		if !ok {
			t.Errorf("%s is not classified", transport)
			continue
		}
		if got != expected {
			t.Errorf("%s capabilities = %+v, want %+v", transport, got, expected)
		}
	}

	seen := map[TransportCapabilities]string{}
	for _, transport := range Transports() {
		caps, _ := Capabilities(transport)
		if other, dup := seen[caps]; dup {
			t.Errorf("%s and %s declare identical capabilities %+v", transport, other, caps)
		}
		seen[caps] = transport
	}
	if _, ok := Capabilities("some-future-lane"); ok {
		t.Fatal("an unclassified transport reported capabilities")
	}
}
