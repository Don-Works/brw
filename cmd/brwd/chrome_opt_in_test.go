package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/chromeoptin"
)

func TestEveryChromeOptInFlagIsRefused(t *testing.T) {
	typ := reflect.TypeOf(chromeOptInFlags{})
	if typ.NumField() == 0 {
		t.Fatal("chromeOptInFlags has no fields; the scan is broken, not the table")
	}
	for i := range typ.NumField() {
		field := typ.Field(i)
		t.Run(field.Name, func(t *testing.T) {
			value := reflect.New(typ).Elem()
			set := value.Field(i)
			switch set.Kind() {
			case reflect.Bool:
				set.SetBool(true)
			case reflect.String:
				set.SetString("set-by-the-test")
			case reflect.Int:
				set.SetInt(9222)
			default:
				t.Fatalf("field %s is a %s, which this test does not know how to set", field.Name, set.Kind())
			}
			flags := value.Interface().(chromeOptInFlags)
			name := flags.conflict()
			if name == "" {
				t.Fatalf("setting %s alone did not conflict with --chrome-opt-in; it would combine silently", field.Name)
			}
			err := checkChromeOptInFlags(flags)
			if err == nil {
				t.Fatalf("checkChromeOptInFlags accepted %s", field.Name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("refusal %q does not name the flag to drop (%s)", err, name)
			}
		})
	}
}

func TestChromeOptInAloneIsAllowed(t *testing.T) {
	if err := checkChromeOptInFlags(chromeOptInFlags{}); err != nil {
		t.Fatalf("--chrome-opt-in on its own was refused: %v", err)
	}
}

func TestChromeOptInRefusalsNameFlagsBrwdRegisters(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	registered := map[string]bool{}
	for _, m := range regexp.MustCompile(`flag\.\w+\(&?[^,]+, "([a-z0-9-]+)"`).FindAllStringSubmatch(string(source), -1) {
		registered[m[1]] = true
	}
	if len(registered) < 10 {
		t.Fatalf("found only %d flags in main.go; the scan is broken", len(registered))
	}
	typ := reflect.TypeOf(chromeOptInFlags{})
	for i := range typ.NumField() {
		value := reflect.New(typ).Elem()
		set := value.Field(i)
		switch set.Kind() {
		case reflect.Bool:
			set.SetBool(true)
		case reflect.String:
			set.SetString("x")
		case reflect.Int:
			set.SetInt(1)
		}
		for _, named := range strings.Split(value.Interface().(chromeOptInFlags).conflict(), "/") {
			named = strings.TrimPrefix(strings.TrimSpace(named), "--")
			if named == "" {
				continue
			}
			if !registered[named] {
				t.Errorf("the refusal for %s names --%s, which brwd does not register", typ.Field(i).Name, named)
			}
		}
	}
}

func fakeOptInChrome(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	hits := &atomic.Int64{}
	var doc map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/json/version" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	_, portText, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("port %q: %v", portText, err)
	}
	doc = map[string]any{
		"Browser":              "Chrome/144.0.7000.0",
		"webSocketDebuggerUrl": "ws://127.0.0.1:" + portText + "/devtools/browser/fake",
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte(strconv.Itoa(port)+"\n/devtools/browser/fake\n"), 0o600); err != nil {
		t.Fatalf("write DevToolsActivePort: %v", err)
	}
	return dir, hits
}

func TestConfigureChromeOptInAttachesToTheDiscoveredEndpoint(t *testing.T) {
	dir, _ := fakeOptInChrome(t)
	cfg, endpoint, err := configureChromeOptIn(context.Background(), browser.Config{}, chromeOptInRequest{UserDataDir: dir})
	if err != nil {
		t.Fatalf("configureChromeOptIn: %v", err)
	}
	if cfg.RemoteURL != endpoint.HTTPURL || cfg.RemoteURL == "" {
		t.Fatalf("RemoteURL = %q, endpoint = %q", cfg.RemoteURL, endpoint.HTTPURL)
	}
	if !cfg.AttachOnly {
		t.Fatal("AttachOnly is not set, so a later edit that lost the endpoint would launch Chrome with a debugging flag")
	}
	if !cfg.SignedInProfile {
		t.Fatal("SignedInProfile is not set, so brw_state would seal the cookies of the browser the user is signed into")
	}

	if cfg.UserDataDir != "" {
		t.Fatalf("UserDataDir = %q; brw only reads the browser's profile directory, and the Manager writes under whatever it is given", cfg.UserDataDir)
	}
	if endpoint.UserDataDir != dir {
		t.Fatalf("endpoint.UserDataDir = %q, want %q so the daemon can report which profile it attached to", endpoint.UserDataDir, dir)
	}
}

func TestConfigureChromeOptInRefusesWithoutLaunching(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  string
	}{
		{name: "opt-in off", dir: t.TempDir()},
		{name: "no directory known for this browser", dir: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, err := configureChromeOptIn(context.Background(), browser.Config{}, chromeOptInRequest{UserDataDir: tc.dir})
			if err == nil {
				t.Fatal("configureChromeOptIn succeeded with no endpoint")
			}
			if cfg.RemoteURL != "" {
				t.Fatalf("RemoteURL = %q on the failure path", cfg.RemoteURL)
			}
			if !cfg.AttachOnly {
				t.Fatal("the failure path returned a config that browser.New would launch a browser from")
			}
			if tc.dir != "" && !errors.Is(err, chromeoptin.ErrOptInOff) {
				t.Fatalf("error %v does not wrap ErrOptInOff", err)
			}
		})
	}
}
