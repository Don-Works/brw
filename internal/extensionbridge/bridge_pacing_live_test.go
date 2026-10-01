package extensionbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/browsertest"
	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/snapshot"
)

func deadlineLiveBridge(t *testing.T, normalTimers bool) (*Bridge, context.Context, string) {
	return deadlineLiveBridgeMode(t, normalTimers, false)
}

func deadlineLiveBridgeMode(t *testing.T, normalTimers, headless bool) (*Bridge, context.Context, string) {
	t.Helper()
	browsers := installedBrowsers()
	if len(browsers) == 0 {
		t.Skip("Chromium unavailable")
	}
	if strings.Contains(strings.ToLower(filepath.Base(browsers[0])), "google chrome") {
		t.Skip("unbranded Chromium required")
	}
	b := New("", 10*time.Second, "")
	b.SetPacing(browser.PacingOff)
	b.SetFollowFocus(false)
	token, err := NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	b.SetAuthToken(token)
	bridgeServer := httptest.NewServer(b.server.Handler)
	t.Cleanup(bridgeServer.Close)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<title>Deadline fixture</title><input aria-label="Name"><button>Continue</button><output id="state" role="status">waiting</output>`)
	}))
	t.Cleanup(fixture.Close)
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
	raw, _ = json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(extension, "manifest.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	bootstrap := fmt.Sprintf(`import './service_worker.js'; chrome.storage.local.set({brwBridgeConfig:{bridgeUrl:%q},brwBrowserControlConsent:{granted:true,version:1,grantedAt:new Date().toISOString()}});`, "ws"+strings.TrimPrefix(bridgeServer.URL, "http")+"/extension")
	if err := os.WriteFile(filepath.Join(extension, "test_bootstrap.js"), []byte(bootstrap), 0600); err != nil {
		t.Fatal(err)
	}
	chromePath := browsers[0]
	if normalTimers {
		chromePath = filepath.Join(t.TempDir(), "chromium-native-timers")
		wrapper := fmt.Sprintf("#!/usr/bin/env python3\nimport os,sys\nargs=[a for a in sys.argv[1:] if a not in ['--disable-background-timer-throttling','--disable-backgrounding-occluded-windows','--disable-renderer-backgrounding']]\nos.execv(%q,[%q]+args)\n", browsers[0], browsers[0])
		if err := os.WriteFile(chromePath, []byte(wrapper), 0700); err != nil {
			t.Fatal(err)
		}
	}
	profile := browsertest.NewProfile(t)
	launcher, err := cdp.Launch(context.Background(), cdp.LaunchConfig{ChromePath: chromePath, UserDataDir: profile.Dir(), Extensions: []string{extension}, Headless: headless, Args: append(quietLaunchArgs(), "--window-size=1280,1000")})
	if err != nil {
		t.Fatal(err)
	}
	profile.StopWith(func() { _ = launcher.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	t.Cleanup(cancel)
	ready, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	if _, err := b.getConn(ready); err != nil {
		t.Fatal(err)
	}
	opened, err := b.Open(ctx, fixture.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx = browser.WithTabID(ctx, opened.Tab.ID)
	t.Cleanup(func() { _ = b.CloseTab(context.Background(), opened.Tab.ID) })
	if err := b.WaitFor(ctx, "ready", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Snapshot(ctx, snapshot.SnapshotOptions{ViewportOnly: true}); err != nil {
		t.Fatal(err)
	}
	state, err := b.Evaluate(ctx, `({hidden:document.hidden,visibility:document.visibilityState,hasFocus:document.hasFocus(),browser:navigator.userAgent})`)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ENV native_timers=%t state=%v pacing=off", normalTimers, state)
	return b, ctx, launcher.Endpoint()
}

func TestBridgePassiveStepPacingMeasurement(t *testing.T) {
	if os.Getenv("BRW_MEASURE_BRIDGE_WAIT_DEADLINE") != "1" {
		t.Skip("set BRW_MEASURE_BRIDGE_WAIT_DEADLINE=1 for paired passive-step pacing measurements")
	}
	b, ctx, _ := deadlineLiveBridge(t, false)
	for _, mode := range []browser.PacingMode{browser.PacingOff, browser.PacingHuman} {
		b.SetPacing(mode)
		for round := 0; round < 12; round++ {
			started := time.Now()
			_, err := b.WaitForOutcome(ctx, "fn:new Promise(()=>{})", 40*time.Millisecond)
			standaloneMS := float64(time.Since(started).Microseconds()) / 1000
			if err == nil {
				t.Fatal("never-resolving standalone wait succeeded")
			}
			started = time.Now()
			result, batchErr := b.ExecuteBatch(ctx, []browser.BatchStep{{Action: "wait", Condition: "fn:new Promise(()=>{})", TimeoutMS: 40}})
			batchMS := float64(time.Since(started).Microseconds()) / 1000
			if batchErr != nil || result.OK || result.StepsCompleted != 1 {
				t.Fatal("passive batch incorrectly completed")
			}
			row, _ := json.Marshal(map[string]any{"pacing": mode, "round": round, "standalone_ms": standaloneMS, "batch_ms": batchMS})
			t.Log(string(row))
		}
	}
}

func TestBridgePassiveFlowMeasurement(t *testing.T) {
	if os.Getenv("BRW_MEASURE_BRIDGE_WAIT_DEADLINE") != "1" {
		t.Skip("set BRW_MEASURE_BRIDGE_WAIT_DEADLINE=1 for paired whole-flow pacing measurements")
	}
	b, ctx, endpoint := deadlineLiveBridge(t, false)
	m, err := browser.New(ctx, browser.Config{RemoteURL: endpoint, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	sourceURL, err := b.Evaluate(ctx, "location.href")
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 8; round++ {
		for turn := 0; turn < 2; turn++ {
			arm := (round + turn) % 2
			ctrl := []browser.Controller{m, b}[arm]
			flowCtx := ctx
			if arm == 0 {
				opened, err := m.Open(ctx, sourceURL.(string))
				if err != nil {
					t.Fatal(err)
				}
				flowCtx = browser.WithTabID(ctx, opened.Tab.ID)
			}
			switch c := ctrl.(type) {
			case *browser.Manager:
				c.SetPacing(browser.PacingHuman)
			case *Bridge:
				c.SetPacing(browser.PacingHuman)
			}
			_, err := ctrl.Evaluate(flowCtx, `window.actionTimes=[];document.querySelector('input').value='';document.querySelector('input').oninput=()=>window.actionTimes.push(performance.now())`)
			if err != nil {
				t.Fatal(err)
			}
			snap, err := ctrl.Snapshot(flowCtx, snapshot.SnapshotOptions{Mode: "all"})
			if err != nil {
				t.Fatal(err)
			}
			ref := ""
			for _, el := range snap.Elements {
				if el.Role == "textbox" {
					ref = el.Ref
				}
			}
			started := time.Now()
			result, err := ctrl.ExecuteBatch(flowCtx, []browser.BatchStep{
				{Action: "fill", Ref: ref, Text: "x"},
				{Action: "wait", Condition: "fn:document.querySelector('input').value === 'x'", TimeoutMS: 1000},
				{Action: "assert_value", Ref: ref, Value: "x", TimeoutMS: 1000},
				{Action: "fill", Ref: ref, Text: "y"},
				{Action: "wait", Condition: "fn:document.querySelector('input').value === 'y'", TimeoutMS: 1000},
				{Action: "assert_value", Ref: ref, Value: "y", TimeoutMS: 1000},
			})
			elapsed := float64(time.Since(started).Microseconds()) / 1000
			if err != nil || !result.OK || result.StepsCompleted != 6 {
				t.Fatalf("whole flow result=%+v err=%v", result, err)
			}
			state, err := ctrl.Evaluate(flowCtx, `({value:document.querySelector('input').value,count:window.actionTimes.length,gap:window.actionTimes[window.actionTimes.length-1]-window.actionTimes[0]})`)
			if err != nil {
				t.Fatal(err)
			}
			values := state.(map[string]any)
			if values["value"] != "y" || values["count"] != float64(2) || values["gap"].(float64) < 275 {
				t.Fatalf("UI action pacing/final state invalid: %v", state)
			}
			row, _ := json.Marshal(map[string]any{"transport": []string{"direct", "extension"}[arm], "round": round, "total_ms": elapsed, "state": state, "steps": result.StepsCompleted})
			t.Log(string(row))
			if arm == 0 {
				if err := m.CloseTab(flowCtx, browser.TabIDFromContext(flowCtx)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}
