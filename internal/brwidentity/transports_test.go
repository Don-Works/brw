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

// The lanes differ in exactly the properties the tool catalogue reads. A lane
// that accidentally copied another's row would advertise the wrong catalogue
// and nothing else would say so — that is how `--remote` came to be reported as
// direct CDP, which claims brw owns the browser and may therefore stage its
// downloads in a directory brw later deletes.
//
// Every transport has to appear, so a lane added without a row here fails
// rather than passing unexamined.
func TestTransportCapabilitiesDistinguishTheLanes(t *testing.T) {
	want := map[string]TransportCapabilities{
		TransportDirectCDP:       {CDPSession: true, BrowserTarget: true, RuntimeDownloadRouting: true},
		TransportRemoteCDP:       {CDPSession: true, BrowserTarget: true},
		TransportChromeOptIn:     {CDPSession: true, BrowserTarget: true, SignedInProfile: true},
		TransportExtensionBridge: {ExtensionAPIs: true, SignedInProfile: true},
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
	// And no two lanes are the same row: a copy would carry the wrong
	// catalogue while satisfying every other test in this file.
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
