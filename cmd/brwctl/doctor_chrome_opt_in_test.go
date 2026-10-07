package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
)

func stageOptIn(t *testing.T, fx *doctorFixture, browser string) {
	t.Helper()
	var doc map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	_, portText, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	doc = map[string]any{
		"Browser":              browser,
		"webSocketDebuggerUrl": "ws://127.0.0.1:" + portText + "/devtools/browser/fake",
	}
	if _, err := strconv.Atoi(portText); err != nil {
		t.Fatalf("port %q: %v", portText, err)
	}
	if err := os.WriteFile(filepath.Join(fx.optInDir, "DevToolsActivePort"),
		[]byte(portText+"\n/devtools/browser/fake\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorReportsTheOptInAsOffAndNamesTheAction(t *testing.T) {
	fx := newDoctorFixture(t)
	report := fx.report()

	got := checkByName(t, report, "chrome_opt_in")
	if got.Status != checkSkip {
		t.Fatalf("chrome_opt_in = %+v, want a skip on a machine with the opt-in off", got)
	}
	if !strings.Contains(got.Fix, "chrome://inspect") {
		t.Fatalf("the fix does not send the user to the switch: %q", got.Fix)
	}
	if !strings.Contains(got.Fix, "brw cannot turn it on for you") {
		t.Fatalf("the fix does not say brw will not do it for them: %q", got.Fix)
	}

	if !report.OK {
		t.Fatalf("a machine with the opt-in off reported failures: %v", report.Failures)
	}
	if report.ChromeOptIn == nil || report.ChromeOptIn.Available {
		t.Fatalf("chrome_opt_in JSON = %+v, want available=false", report.ChromeOptIn)
	}
	if report.ChromeOptIn.Action == "" {
		t.Fatal("the JSON report carries no action for a consumer to show")
	}
}

func TestDoctorReportsTheOptInAsOnWhenItIs(t *testing.T) {
	fx := newDoctorFixture(t)
	stageOptIn(t, fx, "Chrome/144.0.7000.0")
	report := fx.report()

	got := checkByName(t, report, "chrome_opt_in")
	if got.Status != checkOK {
		t.Fatalf("chrome_opt_in = %+v, want OK", got)
	}
	if !strings.Contains(got.Detail, "Chrome/144.0.7000.0") {
		t.Fatalf("the detail does not name the browser: %q", got.Detail)
	}
	if report.ChromeOptIn == nil || !report.ChromeOptIn.Available || report.ChromeOptIn.Endpoint == "" {
		t.Fatalf("chrome_opt_in JSON = %+v", report.ChromeOptIn)
	}
	if !report.OK {
		t.Fatalf("a machine with the opt-in on reported failures: %v", report.Failures)
	}
}

func TestDoctorFailsWhenTheRunningLaneNeedsTheOptIn(t *testing.T) {
	fx := newDoctorFixture(t)
	fx.health.Identity.Transport = brwidentity.TransportChromeOptIn
	report := fx.report()

	got := checkByName(t, report, "chrome_opt_in")
	if got.Status != checkFail {
		t.Fatalf("chrome_opt_in = %+v, want a failure when the running lane needs it", got)
	}
	if !strings.Contains(got.Fix, "chrome://inspect") {
		t.Fatalf("the fix does not send the user to the switch: %q", got.Fix)
	}
	if report.OK {
		t.Fatal("a daemon whose lane cannot work reported no failures")
	}

	stageOptIn(t, fx, "Chrome/144.0.7000.0")
	report = fx.report()
	if got := checkByName(t, report, "chrome_opt_in"); got.Status != checkOK {
		t.Fatalf("chrome_opt_in = %+v after turning the opt-in on, want OK", got)
	}
}

func TestDoctorSeparatesAnOldChromeFromASwitchedOffOptIn(t *testing.T) {
	fx := newDoctorFixture(t)
	stageOptIn(t, fx, "Chrome/143.0.6000.0")
	report := fx.report()

	got := checkByName(t, report, "chrome_opt_in")
	if got.Status != checkSkip {
		t.Fatalf("chrome_opt_in = %+v, want a skip for a Chrome with no opt-in", got)
	}
	if !strings.Contains(got.Detail, "144") {
		t.Fatalf("the detail does not say which Chrome is needed: %q", got.Detail)
	}
	if strings.Contains(got.Fix, "chrome://inspect") {
		t.Fatalf("the fix sends the user to a page with no switch on it: %q", got.Fix)
	}
}
