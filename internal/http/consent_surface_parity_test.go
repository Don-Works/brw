package httpapi

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/mcp"
	"github.com/Don-Works/brw/internal/siteconsent"
	"github.com/Don-Works/brw/internal/snapshot"
)

// This file is about the hole that keeps reopening: a rule enforced on the lane
// it was reported against, and not on the other one.
//
// include_frames attaches a CDP session to every embedded third party's target
// and runs brw's walker in its document. The MCP surface asked consent about each
// of those origins. The HTTP surface installed the fetch check and stopped, so
// `GET /api/page/snapshot?include_frames=true` read the payment form with nothing
// decided about it — and that is the route a proxied call arrives on, because a
// daemon running against an upstream one forwards include_frames over it and a Go
// func cannot cross that boundary. Same daemon, same controller, same tool name;
// different lane.
//
// So the assertions below are about the PROPERTY ("every dispatching surface
// answers every runtime consent question"), enumerated three ways: the hooks come
// from the browser.ConsentEnforcer interface, the surfaces are checked against
// the source tree, and each surface is then driven through its own real entry
// point and asked what the controller was handed.

// runtimeConsentHookProbes maps each method of browser.ConsentEnforcer to the way
// a dispatched context is asked whether that hook reached it.
//
// It is keyed by method name so the interface itself is the list: add a hook to
// ConsentEnforcer and this test fails until someone says how to observe it, which
// is the step that was skipped when the frame-read check was added.
var runtimeConsentHookProbes = map[string]func(context.Context) bool{
	"CheckFetchDestination": func(ctx context.Context) bool { return browser.FetchCheckFromContext(ctx) != nil },
	"CheckFrameRead":        func(ctx context.Context) bool { return browser.FrameReadCheckFromContext(ctx) != nil },
}

// consentSurfaces are the surfaces that turn an agent's call into a controller
// call. Each dispatches one gated read of a page carrying a cross-origin iframe,
// through its own real entry point, and returns the context the controller was
// handed.
//
// The key is the package directory, which is what ties this table to the source
// scan below: a surface added next year installs the hooks from its own package,
// and TestEveryConsentInstallerIsAnEnumeratedSurface fails until it is listed
// here and driven.
var consentSurfaces = map[string]func(t *testing.T, guard *siteconsent.Guard, ctrl browser.Controller) context.Context{
	"internal/http": func(t *testing.T, guard *siteconsent.Guard, ctrl browser.Controller) context.Context {
		t.Helper()
		server := New("", ctrl)
		server.SetSiteConsent(guard)
		rec := doJSON(t, server, "GET", "/api/page/snapshot?include_frames=true", "")
		if rec.Code != 200 {
			t.Fatalf("snapshot over HTTP: %d %s", rec.Code, rec.Body.String())
		}
		return dispatchedContext(t, ctrl)
	},
	"internal/mcp": func(t *testing.T, guard *siteconsent.Guard, ctrl browser.Controller) context.Context {
		t.Helper()
		server := mcp.New(ctrl)
		server.SetSiteConsent(guard)
		input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brw_snapshot","arguments":{"include_frames":true}}}` + "\n"
		var out bytes.Buffer
		if err := server.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
			t.Fatalf("serve brw_snapshot: %v", err)
		}
		if strings.Contains(out.String(), `"error"`) {
			t.Fatalf("brw_snapshot over MCP failed: %s", out.String())
		}
		return dispatchedContext(t, ctrl)
	},
}

// surfaceProbeController answers the consent gate with a live tab and keeps the
// context its Snapshot was called with, which is the only place the runtime hooks
// can be observed from.
type surfaceProbeController struct {
	consentController
	mu  sync.Mutex
	got context.Context
}

func (c *surfaceProbeController) Snapshot(ctx context.Context, opts snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	c.mu.Lock()
	c.got = ctx
	c.mu.Unlock()
	return c.consentController.Snapshot(ctx, opts)
}

func dispatchedContext(t *testing.T, ctrl browser.Controller) context.Context {
	t.Helper()
	probe, ok := ctrl.(*surfaceProbeController)
	if !ok {
		t.Fatalf("controller %T does not record the dispatched context", ctrl)
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.got == nil {
		t.Fatal("the surface never reached the controller, so there is no dispatched context to inspect")
	}
	return probe.got
}

func newParityGuard(t *testing.T) *siteconsent.Guard {
	t.Helper()
	store, err := siteconsent.NewStoreWithKey(filepath.Join(t.TempDir(), "site-grants.json"), fixtureConsentKey)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := siteconsent.NewGuard(store, siteconsent.AdminConfig{})
	if err != nil {
		t.Fatal(err)
	}
	guard.SetGrantor("fixture-user")
	// The page itself is granted. That is precisely the grant that must NOT carry
	// into the documents it embeds.
	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://shop.test", Scope: siteconsent.ScopeRead, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	return guard
}

// TestRuntimeConsentHookProbesCoverTheEnforcer keeps the probe table level with
// the interface, so a hook added to browser.ConsentEnforcer cannot be shipped
// with nothing checking that the surfaces install it.
func TestRuntimeConsentHookProbesCoverTheEnforcer(t *testing.T) {
	iface := reflect.TypeOf((*browser.ConsentEnforcer)(nil)).Elem()
	for i := 0; i < iface.NumMethod(); i++ {
		name := iface.Method(i).Name
		if _, ok := runtimeConsentHookProbes[name]; !ok {
			t.Errorf("browser.ConsentEnforcer.%s is a runtime consent hook with no probe: say how a dispatched context is asked whether it arrived, so every surface can be held to it", name)
		}
	}
	for name := range runtimeConsentHookProbes {
		if _, ok := iface.MethodByName(name); !ok {
			t.Errorf("a probe names %s, which browser.ConsentEnforcer no longer declares", name)
		}
	}
}

// TestEveryConsentSurfaceInstallsEveryRuntimeHook drives each surface and asks
// the controller what it was handed.
//
// The hooks are not merely present: each is called with an origin nobody granted
// and has to refuse it, and with the granted one and has to allow it. A surface
// that installed a stub, or wired a hook to a guard that is not the daemon's,
// passes a presence check and fails this one.
func TestEveryConsentSurfaceInstallsEveryRuntimeHook(t *testing.T) {
	for surface, dispatch := range consentSurfaces {
		t.Run(surface, func(t *testing.T) {
			guard := newParityGuard(t)
			ctrl := &surfaceProbeController{consentController: consentController{tabURL: "https://shop.test/cart"}}
			ctx := dispatch(t, guard, ctrl)

			for name, installed := range runtimeConsentHookProbes {
				if !installed(ctx) {
					t.Errorf("%s dispatched without %s installed, so that question is never asked on this surface while it is on the other — the same call, gated by choice of lane", surface, name)
				}
			}

			frameRead := browser.FrameReadCheckFromContext(ctx)
			if frameRead == nil {
				t.Fatalf("%s installs no cross-origin frame read check, so include_frames walks every embedded third party's document unasked", surface)
			}
			if err := frameRead("https://payments.test"); err == nil {
				t.Errorf("%s allowed a read of https://payments.test, which nobody granted; the embedder's grant is not a grant for what it embeds", surface)
			} else if !strings.Contains(err.Error(), "https://payments.test") {
				t.Errorf("%s refused an embedded origin with %v, which does not name it", surface, err)
			}
			if err := frameRead("https://shop.test"); err != nil {
				t.Errorf("%s refused the granted origin %v, so the hook is not wired to the daemon's own grants", surface, err)
			}

			fetch := browser.FetchCheckFromContext(ctx)
			if fetch == nil {
				t.Fatalf("%s installs no daemon-side fetch check", surface)
			}
			if err := fetch("https://payments.test/receipt"); err == nil {
				t.Errorf("%s allowed a daemon-side fetch of an un-granted origin", surface)
			}
			if err := fetch("https://shop.test/cart"); err != nil {
				t.Errorf("%s refused a daemon-side fetch of the granted origin with %v, so the hook is not wired to the daemon's own grants", surface, err)
			}
		})
	}
}

// TestEveryConsentInstallerIsAnEnumeratedSurface is the lane enumeration.
//
// A surface is found by what it DOES, not by what it installs: a package that
// puts a consent.CheckTool call in front of a dispatch is gating an agent's call,
// and that is the whole definition. Keying the enumeration on the installers
// would miss the case this file exists for — a surface that installs nothing at
// all is not an installer, so such a scan would report the tree clean.
//
// Three things fail it. A gating surface missing from consentSurfaces, driven by
// nothing and free to install a subset exactly as this one did. A gating surface
// that installs no runtime hooks. And production code reaching for an individual
// installer instead of browser.WithRuntimeConsent, which is how a surface gets to
// install two hooks out of three in the first place.
func TestEveryConsentInstallerIsAnEnumeratedSurface(t *testing.T) {
	root := filepath.Join("..", "..")
	gates := map[string]bool{}
	installers := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// .claude holds this repo's git worktrees, each a complete second
			// copy of the tree; walking into one reports another checkout's
			// surfaces as though they were this one's.
			if name := d.Name(); name == ".git" || name == ".claude" || name == "node_modules" || name == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		source := string(body)
		pkg := filepath.ToSlash(filepath.Dir(strings.TrimPrefix(filepath.ToSlash(path), filepath.ToSlash(root)+"/")))
		for _, partial := range []string{"browser.WithFetchCheck(", "browser.WithFrameReadCheck("} {
			if strings.Contains(source, partial) {
				t.Errorf("%s installs %s by hand; go through browser.WithRuntimeConsent so the next hook is a compile error here rather than a surface that gates two questions out of three", path, strings.TrimSuffix(partial, "("))
			}
		}
		if strings.Contains(source, "consent.CheckTool(") {
			gates[pkg] = true
		}
		if strings.Contains(source, "browser.WithRuntimeConsent(") {
			installers[pkg] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	if len(gates) == 0 || len(installers) == 0 {
		t.Fatalf("the scan found %d gating surfaces and %d installing ones; one of its patterns has stopped matching, so it is proving nothing", len(gates), len(installers))
	}
	for pkg := range gates {
		if !installers[pkg] {
			t.Errorf("%s gates a dispatched call but installs no runtime consent hooks, so every question that can only be answered mid-call goes unasked on that surface", pkg)
		}
		if _, enumerated := consentSurfaces[pkg]; !enumerated {
			t.Errorf("%s gates an agent's calls but is not one of the surfaces this file drives; add it to consentSurfaces so it is held to the whole set", pkg)
		}
	}
	for pkg := range installers {
		if _, enumerated := consentSurfaces[pkg]; !enumerated {
			t.Errorf("%s installs the runtime consent hooks but is not one of the surfaces this file drives; add it to consentSurfaces so it is held to the whole set", pkg)
		}
	}
	for pkg := range consentSurfaces {
		if !installers[pkg] {
			t.Errorf("consentSurfaces names %s, which no longer installs the runtime consent hooks", pkg)
		}
		if !gates[pkg] {
			t.Errorf("consentSurfaces names %s, which no longer gates the calls it dispatches", pkg)
		}
	}
}
