package extensionbridge

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/browsertest"
	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/snapshot"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestControlledTransportParity(t *testing.T) {
	if os.Getenv("BRW_MEASURE_TRANSPORT_PARITY") != "1" {
		t.Skip("set BRW_MEASURE_TRANSPORT_PARITY=1 for disposable headed/headless transport parity")
	}
	rounds := 3
	if value := os.Getenv("BRW_PARITY_ROUNDS"); value != "" {
		var err error
		rounds, err = strconv.Atoi(value)
		if err != nil || rounds < 3 || rounds > 100 {
			t.Fatal("BRW_PARITY_ROUNDS must be between 3 and 100")
		}
	}
	var baselineAcrossModes string
	for _, headless := range []bool{true, false} {
		t.Run(fmt.Sprintf("headless_%t", headless), func(t *testing.T) {
			browsers := installedBrowsers()
			if len(browsers) == 0 {
				t.Skip("Chrome/Chromium unavailable")
			}
			if strings.Contains(strings.ToLower(filepath.Base(browsers[0])), "google chrome") || strings.HasPrefix(strings.ToLower(filepath.Base(browsers[0])), "google-chrome") {
				t.Skip("unbranded Chromium required for unpacked extensions")
			}
			b := New("", 30*time.Second, "")
			token, err := NewAuthToken()
			if err != nil {
				t.Fatal(err)
			}
			b.SetAuthToken(token)
			bridgeServer := httptest.NewServer(b.server.Handler)
			defer bridgeServer.Close()
			fixture := httptest.NewServer(http.FileServer(http.Dir("../../tests/fixtures")))
			defer fixture.Close()
			extension := t.TempDir()
			if err := os.CopyFS(extension, os.DirFS("../../extension")); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(extension, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			var manifest map[string]any
			if err := json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
			manifest["background"] = map[string]any{"service_worker": "test_bootstrap.js", "type": "module"}
			raw, err = json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(extension, "manifest.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			bootstrap := fmt.Sprintf(`import './service_worker.js'; chrome.storage.local.set({brwBridgeConfig:{bridgeUrl:%q},brwBrowserControlConsent:{granted:true,version:1,grantedAt:new Date().toISOString()}});`, "ws"+strings.TrimPrefix(bridgeServer.URL, "http")+"/extension")
			if err := os.WriteFile(filepath.Join(extension, "test_bootstrap.js"), []byte(bootstrap), 0600); err != nil {
				t.Fatal(err)
			}
			profile := browsertest.NewProfile(t)
			launcher, err := cdp.Launch(context.Background(), cdp.LaunchConfig{ChromePath: browsers[0], UserDataDir: profile.Dir(), Extensions: []string{extension}, Headless: headless, Args: append(quietLaunchArgs(), "--window-size=1280,1000")})
			if err != nil {
				t.Fatal(err)
			}
			profile.StopWith(func() { _ = launcher.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			readyCtx, readyCancel := context.WithTimeout(ctx, 20*time.Second)
			defer readyCancel()
			if _, err := b.getConn(readyCtx); err != nil {
				t.Fatalf("unpacked extension did not connect: %v", err)
			}

			b.SetPacing(browser.PacingOff)
			m, err := browser.New(ctx, browser.Config{RemoteURL: launcher.Endpoint(), Timeout: 30 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			m.SetPacing(browser.PacingOff)
			t.Logf("ENV browser=%s headless=%t pacing=off", browserVersion(t, browsers[0]), headless)
			for round := 0; round < rounds; round++ {
				for turn := 0; turn < 2; turn++ {
					arm := (round + turn) % 2
					seed := int64(20261001 + round)
					rng := rand.New(rand.NewSource(seed))
					email := fmt.Sprintf("speed-%d@example.test", rng.Intn(1000000))
					name := fmt.Sprintf("Parity 👩🏽‍💻 日本語 é %d", rng.Intn(1000000))
					expected := "Submitted " + email + " " + name + " pro accepted"
					ctrl := []browser.Controller{m, b}[arm]
					transport := []string{"direct", "extension"}[arm]
					totalStart := time.Now()
					stages := map[string]float64{}
					bytes := 0
					record := func(name string, start time.Time, value any) {
						stages[name] = float64(time.Since(start).Microseconds()) / 1000
						encoded, _ := json.Marshal(value)
						bytes += len(encoded)
					}
					start := time.Now()
					opened, err := ctrl.Open(ctx, fixture.URL+"/forms.html")
					if err != nil {
						t.Fatal(err)
					}
					record("open", start, opened)
					tabctx := browser.WithTabID(ctx, opened.Tab.ID)
					if err := ctrl.WaitFor(tabctx, "fn:document.readyState === 'complete' && !!document.getElementById('signup')", 10*time.Second); err != nil {
						t.Fatal(err)
					}
					if _, err := ctrl.EmulateDevice(tabctx, browser.DeviceEmulationOptions{Width: 1280, Height: 1000, DeviceScaleFactor: 1}); err != nil {
						t.Fatal(err)
					}
					viewport, err := ctrl.Evaluate(tabctx, "({width:innerWidth,height:innerHeight,dpr:devicePixelRatio})")
					if err != nil {
						t.Fatal(err)
					}
					start = time.Now()
					snap, err := ctrl.Snapshot(tabctx, snapshot.SnapshotOptions{Mode: "all"})
					if err != nil {
						t.Fatal(err)
					}
					record("snapshot_first", start, snap)
					start = time.Now()
					warm, err := ctrl.Snapshot(tabctx, snapshot.SnapshotOptions{Mode: "all"})
					if err != nil {
						t.Fatal(err)
					}
					record("snapshot_repeat", start, warm)
					start = time.Now()
					read, err := ctrl.Read(tabctx)
					if err != nil {
						t.Fatal(err)
					}
					record("read", start, read)
					refs := map[string]string{}
					normalized := []map[string]any{}
					for _, e := range snap.Elements {
						refs[e.Role+":"+e.Name] = e.Ref
						normalized = append(normalized, map[string]any{"role": e.Role, "name": e.Name, "value": e.Value, "visible": e.Visible, "viewport": e.InViewport, "disabled": e.Disabled})
					}
					norm, _ := json.Marshal(normalized)
					if baselineAcrossModes == "" {
						baselineAcrossModes = string(norm)
					} else if baselineAcrossModes != string(norm) {
						t.Fatalf("semantic observation mismatch transport=%s round=%d baseline=%s got=%s", transport, round, baselineAcrossModes, norm)
					}
					action := func(name string, f func() (browser.ActionResult, error)) {
						s := time.Now()
						r, e := f()
						if e != nil {
							t.Fatalf("%s: %v", name, e)
						}
						if !r.OK {
							t.Fatalf("%s not OK: %+v", name, r)
						}
						record(name, s, r)
					}
					action("fill_email", func() (browser.ActionResult, error) {
						return ctrl.Fill(tabctx, snapshot.FillOptions{Ref: refs["textbox:Email"], Text: email, Replace: true})
					})
					action("fill_name", func() (browser.ActionResult, error) {
						return ctrl.Fill(tabctx, snapshot.FillOptions{Ref: refs["textbox:Full name"], Text: name, Replace: true})
					})
					action("select_plan", func() (browser.ActionResult, error) { return ctrl.Select(tabctx, refs["combobox:Plan"], "pro") })
					action("click_terms", func() (browser.ActionResult, error) { return ctrl.Click(tabctx, refs["checkbox:Accept terms"]) })
					action("click_submit", func() (browser.ActionResult, error) { return ctrl.Click(tabctx, refs["button:Submit request"]) })
					start = time.Now()
					if err := ctrl.AssertText(tabctx, refs["status:"], expected, 10*time.Second); err != nil {
						t.Fatal(err)
					}
					record("assert", start, map[string]bool{"ok": true})
					final, err := ctrl.Evaluate(tabctx, "({email:document.getElementById('email').value,name:document.getElementById('full-name').value,plan:document.getElementById('plan').value,terms:document.getElementById('terms').checked,result:document.getElementById('result').textContent})")
					if err != nil {
						t.Fatal(err)
					}
					encoded, _ := json.Marshal(final)
					if !strings.Contains(string(encoded), expected) {
						t.Fatalf("final state failed: %s", encoded)
					}
					output, _ := json.Marshal(map[string]any{"headless": headless, "transport": transport, "round": round, "seed": seed, "viewport": viewport, "stages_ms": stages, "observation_json_bytes": bytes, "total_verified_ms": float64(time.Since(totalStart).Microseconds()) / 1000, "semantic_elements": len(snap.Elements), "semantic_sha256": fmt.Sprintf("%x", sha256.Sum256(norm)), "final_state": final})
					t.Log(string(output))
					if err := ctrl.CloseTab(tabctx, opened.Tab.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}
