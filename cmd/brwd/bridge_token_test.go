package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/extensionbridge"
)

func TestBridgeRequireTokenDefaults(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want bool
	}{
		{"unset requires the token", "", true},
		{"explicit opt-out allows tokenless", "1", false},
		{"true opts out", "true", false},
		{"unrecognised value still requires", "maybe", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BRW_BRIDGE_ALLOW_TOKENLESS", tc.env)
			if got := bridgeRequireToken(); got != tc.want {
				t.Fatalf("bridgeRequireToken() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBridgeConstructorRequiresToken(t *testing.T) {
	b := extensionbridge.New("", 0, "")
	if !b.RequireToken() {
		t.Fatal("a freshly constructed bridge must require a handshake token")
	}
}

func TestBridgeTokenIsNotWrittenByDefault(t *testing.T) {
	const fixtureToken = "fixture-bridge-token-value-one"

	tests := []struct {
		name      string
		optIn     bool
		preExists bool
		wantFile  string
	}{
		{name: "default writes nothing", wantFile: ""},
		{name: "default removes a file an older daemon left", preExists: true, wantFile: ""},
		{name: "an explicit opt-in still writes", optIn: true, wantFile: fixtureToken},
		{name: "an explicit opt-in overwrites a stale file", optIn: true, preExists: true, wantFile: fixtureToken},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			path := filepath.Join(home, ".brw", "bridge-token")
			if tc.optIn {
				t.Setenv("BRW_BRIDGE_TOKEN_FILE", path)
			} else {
				t.Setenv("BRW_BRIDGE_TOKEN_FILE", "")
			}
			if tc.preExists {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("fixture-bridge-token-value-two"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			target := bridgeTokenFile("")
			if target.Path != path {
				t.Fatalf("token path = %q, want %q", target.Path, path)
			}
			if err := bridgeTokenAtLaunch(target, fixtureToken); err != nil {
				t.Fatalf("bridgeTokenAtLaunch: %v", err)
			}

			data, err := os.ReadFile(path)
			switch {
			case tc.wantFile == "" && err == nil:
				t.Fatalf("%s still holds the handshake token: %q", path, data)
			case tc.wantFile == "" && !os.IsNotExist(err):
				t.Fatalf("reading %s: %v", path, err)
			case tc.wantFile != "" && err != nil:
				t.Fatalf("opted-in token file: %v", err)
			case tc.wantFile != "" && string(data) != tc.wantFile:
				t.Fatalf("token file = %q, want %q", data, tc.wantFile)
			}
		})
	}
}

func TestBridgeTokenFileIsPerWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BRW_BRIDGE_TOKEN_FILE", "")
	first := bridgeTokenFile("work/space")
	second := bridgeTokenFile("other")
	if first.Path == second.Path {
		t.Fatalf("two workspaces share one token file: %s", first.Path)
	}
	if strings.ContainsAny(filepath.Base(first.Path), `/\:`) {
		t.Fatalf("workspace separators leaked into the file name: %s", first.Path)
	}
}

func TestBridgeTokenSweepClearsEveryCopyAnOlderDaemonLeft(t *testing.T) {
	const fixtureToken = "fixture-bridge-token-value-three"
	const stale = "fixture-bridge-token-value-four"

	tests := []struct {
		name      string
		workspace string

		optIn    string
		existing []string
		wantKept []string
	}{
		{
			name:      "a workspace-bound daemon still clears the default path",
			workspace: "work",
			existing:  []string{"bridge-token", "bridge-token-work"},
		},
		{
			name:     "the default daemon clears every workspace copy",
			existing: []string{"bridge-token", "bridge-token-work", "bridge-token-other"},
		},
		{
			name:     "an opt-in somewhere else does not excuse the default path",
			optIn:    "elsewhere",
			existing: []string{"bridge-token", "bridge-token-work"},
		},
		{
			name:      "an opt-in inside the directory keeps only that file",
			workspace: "work",
			optIn:     "~/bridge-token-work",
			existing:  []string{"bridge-token", "bridge-token-work", "bridge-token-other"},
			wantKept:  []string{"bridge-token-work"},
		},
		{
			name:     "a file the daemon never wrote is left alone",
			existing: []string{"bridge-token", "notes.txt", "bridge-tokens-backup"},
			wantKept: []string{"notes.txt", "bridge-tokens-backup"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			dir := filepath.Join(home, ".brw")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range tc.existing {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(stale), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			optIn := ""
			switch {
			case strings.HasPrefix(tc.optIn, "~/"):
				optIn = filepath.Join(dir, strings.TrimPrefix(tc.optIn, "~/"))
			case tc.optIn != "":
				optIn = filepath.Join(home, tc.optIn, "bridge-token")
			}
			t.Setenv("BRW_BRIDGE_TOKEN_FILE", optIn)

			if err := bridgeTokenAtLaunch(bridgeTokenFile(tc.workspace), fixtureToken); err != nil {
				t.Fatalf("bridgeTokenAtLaunch: %v", err)
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			var left []string
			for _, entry := range entries {
				left = append(left, entry.Name())
			}
			sort.Strings(left)
			want := append([]string(nil), tc.wantKept...)
			sort.Strings(want)
			if strings.Join(left, ",") != strings.Join(want, ",") {
				t.Fatalf("~/.brw holds %v, want %v", left, want)
			}

			if optIn == "" {
				return
			}
			data, err := os.ReadFile(optIn)
			if err != nil {
				t.Fatalf("the opted-in token file: %v", err)
			}
			if string(data) != fixtureToken {
				t.Fatalf("opted-in token file = %q, want %q", data, fixtureToken)
			}
		})
	}
}

func TestBridgeTokenSweepRunsOnALaunchThatMintsNoToken(t *testing.T) {
	tests := []struct {
		name  string
		optIn bool
	}{
		{name: "a launch with no opt-in"},
		{name: "a launch whose opt-in has no token to honour", optIn: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			dir := filepath.Join(home, ".brw")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"bridge-token", "bridge-token-work"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture-token-an-older-daemon-left"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			optIn := ""
			if tc.optIn {
				optIn = filepath.Join(dir, "bridge-token")
			}
			t.Setenv("BRW_BRIDGE_TOKEN_FILE", optIn)

			if err := bridgeTokenAtLaunch(bridgeTokenFile(""), ""); err != nil {
				t.Fatalf("bridgeTokenAtLaunch: %v", err)
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			var left []string
			for _, entry := range entries {
				left = append(left, entry.Name())
			}
			if len(left) != 0 {
				t.Fatalf("~/.brw still holds %v after a launch that minted no token", left)
			}
		})
	}
}

func TestEveryLaunchSweepsTheBridgeTokenFile(t *testing.T) {
	const call = "bridgeTokenAtLaunch"
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "main" && fn.Recv == nil {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("cmd/brwd/main.go has no func main")
	}

	calls := func(node ast.Node) int {
		found := 0
		ast.Inspect(node, func(n ast.Node) bool {
			if expr, ok := n.(*ast.CallExpr); ok {
				if ident, ok := expr.Fun.(*ast.Ident); ok && ident.Name == call {
					found++
				}
			}
			return true
		})
		return found
	}

	unconditional := 0
	for _, stmt := range body.List {
		switch typed := stmt.(type) {
		case *ast.IfStmt:
			if typed.Init != nil {
				unconditional += calls(typed.Init)
			}
			if typed.Cond != nil {
				unconditional += calls(typed.Cond)
			}
		case *ast.ExprStmt:
			unconditional += calls(typed)
		case *ast.AssignStmt:
			unconditional += calls(typed)
		}
	}
	total := calls(parsed)
	switch {
	case unconditional == 0:
		t.Fatalf("no %s call runs on every path through main: a launch mode that skips it leaves the handshake token on disk", call)
	case total != unconditional:
		t.Fatalf("%s is called %d time(s) in cmd/brwd/main.go, %d of them on every path; the rest sit inside a branch some launch modes do not take",
			call, total, unconditional)
	}
}

func TestBridgeTokenReplacementIsPrivateAndDoesNotFollowSymlinks(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		path := filepath.Join(home, "token")
		original := filepath.Join(home, "original")
		if err := os.WriteFile(original, []byte("keep original"), 0o644); err != nil {
			t.Fatal(err)
		}
		if symlink {
			if err := os.Symlink(original, path); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(path, []byte("old token"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := bridgeTokenAtLaunch(bridgeTokenTarget{Path: path, OptedIn: true}, "new token"); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Errorf("replacement token is not a private regular file: %v %v", info, err)
		}
		body, err := os.ReadFile(original)
		if err != nil || string(body) != "keep original" {
			t.Errorf("symlink destination overwritten: %q %v", body, err)
		}
	}
}
