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

// The capability table is the gate every lane's tool catalogue is derived from,
// so a transport constant that is not in it is a lane whose tools are
// unclassified. Declaring the constant is the step nobody forgets; adding the
// row is the step that gets missed, and nothing else in the build notices.
//
// The domain is enumerated from the source rather than from a list kept here,
// because a list kept here has the same problem it is meant to catch.
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

// The three lanes differ in exactly the properties the tool catalogue reads. A
// lane that accidentally copied another's row would advertise the wrong
// catalogue and nothing else would say so.
func TestTransportCapabilitiesDistinguishTheLanes(t *testing.T) {
	for _, tc := range []struct {
		transport string
		want      TransportCapabilities
	}{
		{TransportDirectCDP, TransportCapabilities{CDPSession: true, BrowserTarget: true, RuntimeDownloadRouting: true}},
		{TransportChromeOptIn, TransportCapabilities{CDPSession: true, BrowserTarget: true, SignedInProfile: true}},
		{TransportExtensionBridge, TransportCapabilities{ExtensionAPIs: true, SignedInProfile: true}},
	} {
		got, ok := Capabilities(tc.transport)
		if !ok {
			t.Fatalf("%s is not classified", tc.transport)
		}
		if got != tc.want {
			t.Fatalf("%s capabilities = %+v, want %+v", tc.transport, got, tc.want)
		}
	}
	if _, ok := Capabilities("some-future-lane"); ok {
		t.Fatal("an unclassified transport reported capabilities")
	}
}
