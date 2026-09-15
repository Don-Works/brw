package browsertest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The eleventh test to hand t.TempDir() straight to a browser would go red on
// Linux CI and nowhere else, so nothing anyone runs before pushing would catch
// it. This catches it here instead, on every platform, in a scan with no
// browser in it.
//
// The scan is over the syntax tree rather than over lines, because the defect is
// about where a VALUE ends up and a line only shows one spelling of that. The
// first version matched two literal spellings —
//
//	UserDataDir: t.TempDir()
//	chromedp.UserDataDir(t.TempDir())
//
// — and a test that wrote `dir := t.TempDir()` on one line and
// `"--user-data-dir="+dir` on another was invisible to both. That is the site
// that reached CI: internal/chromeoptin's opted-in-Chrome test, whose Chrome
// wrote Default/ into the directory testing was about to RemoveAll. So the walk
// now follows the value: every identifier that came from t.TempDir(), directly
// or through filepath.Join or a concatenation, and every place such a value is
// handed to something that starts a browser.
//
// Three sinks name a browser's profile directory:
//
//   - chromedp's UserDataDir option;
//   - the UserDataDir field of a config that LAUNCHES (browser.Config,
//     cdp.LaunchConfig) — chromeoptin.Options and the other readers take the
//     same field name to go LOOKING in a directory, and nothing writes there;
//   - --user-data-dir on a browser command line.
//
// A directory reached some other way is not this test's business — the reclaim
// is what makes it safe, not the spelling.
func TestNoTestHandsATempDirStraightToABrowser(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			// .claude holds this repo's git worktrees, each a complete second
			// copy of the tree. Walking into one reports every finding once per
			// worktree and keeps reporting a violation already fixed here.
			case ".git", ".claude", "node_modules", "bin", "store-assets":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			relative = path
		}
		sinks, parseErr := profileSinksIn(path)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", relative, parseErr)
		}
		for _, sink := range sinks {
			found = append(found, fmt.Sprintf("%s:%d (%s)", relative, sink.line, sink.spelling))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	if len(found) > 0 {
		t.Errorf("%s hand t.TempDir() to a browser as its profile directory.\n"+
			"testing's TempDir cleanup is a single strict RemoveAll, and on Linux it races the last Chrome helper still writing the profile — the test then fails on the cleanup rather than on anything it asserts.\n"+
			"Take the directory from browsertest.NewProfile(t) and register the browser's shutdown with StopWith.",
			strings.Join(found, ", "))
	}
}

// profileSink is one place a t.TempDir()-derived path reaches a browser.
type profileSink struct {
	line     int
	spelling string
}

// launchConfigTypes are the config types whose UserDataDir field is the
// directory a browser brw STARTS will write into. The same field name on
// chromeoptin.Options, cdp.AutoConnectOptions and the policy types names a
// directory to read, which no browser is writing because of the test.
var launchConfigTypes = map[string]bool{"Config": true, "LaunchConfig": true}

// browserOnlyFlags say that an argument list is a browser's command line. One of
// these next to --user-data-dir is what tells a Chrome invocation apart from a
// brwd invocation that merely carries the same flag through to its config.
var browserOnlyFlags = []string{
	"--headless",
	"--remote-debugging-port",
	"--remote-debugging-pipe",
	"--no-first-run",
	"--no-default-browser-check",
	"--disable-gpu",
	"--site-per-process",
	"--load-extension",
	"--enable-unsafe-extension-debugging",
}

func profileSinksIn(path string) ([]profileSink, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return profileSinksInSource(path, source)
}

func profileSinksInSource(name string, source []byte) ([]profileSink, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, source, 0)
	if err != nil {
		return nil, err
	}
	temp := tempRootedNames(file)
	rooted := func(expr ast.Expr) bool { return tempRooted(expr, temp) }

	var sinks []profileSink
	add := func(pos token.Pos, spelling string) {
		sinks = append(sinks, profileSink{line: fset.Position(pos).Line, spelling: spelling})
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.CallExpr:
			if selector, ok := typed.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "UserDataDir" && len(typed.Args) == 1 {
				if rooted(typed.Args[0]) {
					add(typed.Pos(), "chromedp.UserDataDir")
					return true
				}
			}
			if value, ok := userDataDirArgument(typed.Args); ok && rooted(value) {
				if isExecCommand(typed.Fun) || hasBrowserOnlyFlag(typed.Args) {
					add(typed.Pos(), "--user-data-dir on a browser command line")
				}
			}
		case *ast.CompositeLit:
			for _, element := range typed.Elts {
				keyed, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				name, ok := keyed.Key.(*ast.Ident)
				if !ok || name.Name != "UserDataDir" {
					continue
				}
				// An inline t.TempDir() is unambiguous whatever the type: nothing
				// else could have meant to write it there.
				if containsTempDir(keyed.Value) {
					add(keyed.Pos(), "UserDataDir: t.TempDir()")
					continue
				}
				if !rooted(keyed.Value) || !launchConfigType(typed.Type) {
					continue
				}
				// AttachOnly is the lane that refuses to start a browser at all —
				// attach_only_test asserts the directory is never even created —
				// so the profile it names is written to by nothing.
				if attachOnly(typed) {
					continue
				}
				add(keyed.Pos(), "UserDataDir on a launch config")
			}
			if value, ok := userDataDirArgument(typed.Elts); ok && rooted(value) && hasBrowserOnlyFlag(typed.Elts) {
				add(typed.Pos(), "--user-data-dir on a browser command line")
			}
		}
		return true
	})
	return sinks, nil
}

// tempRootedNames collects every identifier in the file assigned a value that
// came from t.TempDir(), which is what lets the scan see a directory that
// reaches the browser one line later under another name.
func tempRootedNames(file *ast.File) map[string]bool {
	names := map[string]bool{}
	// Two passes: a name can be assigned after the line that uses it (a helper
	// defined below its caller), and the scan is over the file, not over a
	// control flow.
	for pass := 0; pass < 2; pass++ {
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.AssignStmt:
				if len(typed.Lhs) != len(typed.Rhs) {
					return true
				}
				for i, left := range typed.Lhs {
					if ident, ok := left.(*ast.Ident); ok && tempRooted(typed.Rhs[i], names) {
						names[ident.Name] = true
					}
				}
			case *ast.ValueSpec:
				if len(typed.Names) != len(typed.Values) {
					return true
				}
				for i, name := range typed.Names {
					if tempRooted(typed.Values[i], names) {
						names[name.Name] = true
					}
				}
			}
			return true
		})
	}
	return names
}

// tempRooted reports whether an expression's value came from t.TempDir() —
// directly, through one of the names collected above, or through a join or a
// concatenation of either.
func tempRooted(expr ast.Expr, names map[string]bool) bool {
	rooted := false
	ast.Inspect(expr, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.CallExpr:
			if selector, ok := typed.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "TempDir" {
				rooted = true
			}
		case *ast.Ident:
			if names[typed.Name] {
				rooted = true
			}
		}
		return !rooted
	})
	return rooted
}

func containsTempDir(expr ast.Expr) bool { return tempRooted(expr, nil) }

// userDataDirArgument finds the value bound to --user-data-dir in an argument
// list, in each of the spellings a command line uses: "--user-data-dir="+dir,
// "--user-data-dir", dir, and a formatted string carrying the flag.
func userDataDirArgument(elements []ast.Expr) (ast.Expr, bool) {
	const flag = "--user-data-dir"
	for i, element := range elements {
		switch typed := element.(type) {
		case *ast.BinaryExpr:
			if typed.Op == token.ADD {
				if text, ok := stringLiteral(typed.X); ok && strings.HasPrefix(text, flag) {
					return typed.Y, true
				}
			}
		case *ast.BasicLit:
			text, ok := stringLiteral(typed)
			if !ok {
				continue
			}
			if text == flag && i+1 < len(elements) {
				return elements[i+1], true
			}
		case *ast.CallExpr:
			for _, argument := range typed.Args {
				if text, ok := stringLiteral(argument); ok && strings.HasPrefix(text, flag) {
					return typed, true
				}
			}
		}
	}
	return nil, false
}

func hasBrowserOnlyFlag(elements []ast.Expr) bool {
	for _, element := range elements {
		found := false
		ast.Inspect(element, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok {
				return true
			}
			text, ok := stringLiteral(literal)
			if !ok {
				return true
			}
			for _, flag := range browserOnlyFlags {
				if strings.HasPrefix(text, flag) {
					found = true
					return false
				}
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

func isExecCommand(fun ast.Expr) bool {
	selector, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "exec" && strings.HasPrefix(selector.Sel.Name, "Command")
}

func launchConfigType(expr ast.Expr) bool {
	switch typed := expr.(type) {
	case *ast.Ident:
		return launchConfigTypes[typed.Name]
	case *ast.SelectorExpr:
		return launchConfigTypes[typed.Sel.Name]
	}
	return false
}

func attachOnly(literal *ast.CompositeLit) bool {
	for _, element := range literal.Elts {
		keyed, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		name, ok := keyed.Key.(*ast.Ident)
		if !ok || name.Name != "AttachOnly" {
			continue
		}
		if value, ok := keyed.Value.(*ast.Ident); ok && value.Name == "true" {
			return true
		}
	}
	return false
}

func stringLiteral(expr ast.Expr) (string, bool) {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	text, err := strconv.Unquote(literal.Value)
	if err != nil {
		return "", false
	}
	return text, true
}
