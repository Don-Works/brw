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
