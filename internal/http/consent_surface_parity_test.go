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

var runtimeConsentHookProbes = map[string]func(context.Context) bool{
	"CheckFetchDestination": func(ctx context.Context) bool { return browser.FetchCheckFromContext(ctx) != nil },
	"CheckFrameRead":        func(ctx context.Context) bool { return browser.FrameReadCheckFromContext(ctx) != nil },
}

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

	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://shop.test", Scope: siteconsent.ScopeRead, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	return guard
}

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

func TestEveryConsentInstallerIsAnEnumeratedSurface(t *testing.T) {
	root := filepath.Join("..", "..")
	gates := map[string]bool{}
	installers := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {

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
