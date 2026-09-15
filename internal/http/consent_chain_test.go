package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/siteconsent"
)

// forwardingController is a controller that is itself a client of another brw
// daemon, which is what an --upstream-http proxy's controller is.
type forwardingController struct {
	browser.Controller
	posture siteconsent.Posture
	err     error
	asked   int
}

func (c *forwardingController) UpstreamConsentPosture(context.Context) (siteconsent.Posture, error) {
	c.asked++
	return c.posture, c.err
}

// directOnlyController drives a browser directly, so there is no second daemon to
// ask and its own flags are the whole answer.
type directOnlyController struct {
	browser.Controller
}

func healthConsent(t *testing.T, server *Server) map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/health answered %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Consent map[string]any `json:"consent"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("/health is not JSON: %v\n%s", err, recorder.Body.String())
	}
	if payload.Consent == nil {
		t.Fatalf("/health carries no consent block: %s", recorder.Body.String())
	}
	return payload.Consent
}

// The consent posture at /health is a property of the chain a request travels,
// not of the process that answers. A proxy applies its own guard and then hands
// the work to a daemon that applies its own, so whichever of them has a
// prompter is the one an unattended caller would hang on.
func TestHealthReportsTheConsentPostureOfTheWholeChain(t *testing.T) {
	upstream := &forwardingController{posture: siteconsent.Posture{Enabled: true, Interactive: true}}
	server := New("", upstream)

	consent := healthConsent(t, server)
	if interactive, _ := consent["interactive"].(bool); !interactive {
		t.Fatalf("a proxy in front of a prompting daemon reported %v", consent)
	}
	if upstream.asked == 0 {
		t.Fatal("the controller was never asked, so the posture is this process's own flags")
	}
}

// "Could not ask" is not "said no". A hop that cannot be read poisons the
// chain's answer rather than being dropped, because an unattended caller has to
// treat "unknown" the way it treats "yes".
func TestHealthReportsAnUnreadableHopAsUnknown(t *testing.T) {
	server := New("", &forwardingController{err: errors.New("connection refused")})

	consent := healthConsent(t, server)
	if unknown, _ := consent["unknown"].(bool); !unknown {
		t.Fatalf("an unreadable upstream was reported as a definite posture: %v", consent)
	}
	if reason, _ := consent["unknown_reason"].(string); !strings.Contains(reason, "connection refused") {
		t.Errorf("the unknown posture does not say what went wrong: %q", reason)
	}
}

// And a daemon that drives a browser directly still answers definitely. Without
// this, the fix would be a daemon that tells every scheduler it might hang.
func TestHealthOnADirectDaemonIsADefiniteAnswer(t *testing.T) {
	consent := healthConsent(t, New("", &directOnlyController{}))
	if unknown, _ := consent["unknown"].(bool); unknown {
		t.Fatalf("a daemon with no upstream reported an unknown posture: %v", consent)
	}
	for _, field := range []string{"enabled", "interactive", "confirm_actions"} {
		if _, ok := consent[field]; !ok {
			t.Errorf("/health no longer reports %q, which is what `brw run` gates on: %v", field, consent)
		}
	}
}

// The wiring is a type assertion in NewWithIdentity rather than a setter the
// caller has to remember, so a forwarding controller added next year is covered
// without an edit there. What that cannot catch is a forwarding controller that
// never grows the method: the assertion fails silently and the daemon reports
// its own flags as the chain's answer, which is the bypass this closes.
//
// So the types are enumerated out of the source. The marker for "this is a
// client of another brw daemon" is that it can read that daemon's /health:
// anything holding an httpclient.Health has a second daemon to answer for.
func TestEveryControllerThatReadsAnotherDaemonsHealthAlsoReportsItsConsentPosture(t *testing.T) {
	methods := methodsByReceiver(t)

	var forwarding []string
	for receiver, names := range methods {
		if names["readsBrwHealth"] {
			forwarding = append(forwarding, receiver)
		}
	}
	sort.Strings(forwarding)
	if len(forwarding) == 0 {
		t.Fatal("no type in this repository reads another brw daemon's /health, so this test is enumerating nothing")
	}
	for _, receiver := range forwarding {
		if !methods[receiver]["UpstreamConsentPosture"] {
			t.Errorf("%s reads another brw daemon's /health but never reports that daemon's consent posture, so a brwd proxying through it answers an unattended caller with its own flags", receiver)
		}
	}
}

// methodsByReceiver maps "package.Type" to the set of method names declared on
// it, plus the synthetic marker "readsBrwHealth" for a method returning an
// httpclient.Health.
func methodsByReceiver(t *testing.T) map[string]map[string]bool {
	t.Helper()
	root := filepath.Join("..", "..")
	out := map[string]map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			return nil
		}
		for _, decl := range parsed.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Recv == nil || len(function.Recv.List) == 0 {
				continue
			}
			receiver := parsed.Name.Name + "." + receiverTypeName(function.Recv.List[0].Type)
			if out[receiver] == nil {
				out[receiver] = map[string]bool{}
			}
			out[receiver][function.Name.Name] = true
			if returnsBrwHealth(parsed.Name.Name, function.Type.Results) {
				out[receiver]["readsBrwHealth"] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}
	return out
}

// returnsBrwHealth reports whether a result list carries brw's own /health
// payload: httpclient.Health elsewhere, or a bare Health inside the httpclient
// package itself.
func returnsBrwHealth(pkg string, results *ast.FieldList) bool {
	if results == nil {
		return false
	}
	for _, field := range results.List {
		switch typed := field.Type.(type) {
		case *ast.SelectorExpr:
			ident, ok := typed.X.(*ast.Ident)
			if ok && ident.Name == "httpclient" && typed.Sel.Name == "Health" {
				return true
			}
		case *ast.Ident:
			if pkg == "httpclient" && typed.Name == "Health" {
				return true
			}
		}
	}
	return false
}
