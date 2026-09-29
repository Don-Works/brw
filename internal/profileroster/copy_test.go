package profileroster

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/usagelog"
)

type fakeDaemon struct {
	mu        sync.Mutex
	identity  brwidentity.Identity
	jar       []browser.Cookie
	calls     []browser.CookieParams
	refuseSet bool
	released  int
	owners    map[string]bool
	srv       *httptest.Server
}

func newFakeDaemon(t *testing.T, id brwidentity.Identity, jar ...browser.Cookie) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{identity: id, jar: jar}
	d.srv = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeDaemon) addr() string { return strings.TrimPrefix(d.srv.URL, "http://") }

func (d *fakeDaemon) writes() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, c := range d.calls {
		if c.Action != browser.CookieActionList {
			n++
		}
	}
	return n
}

func (d *fakeDaemon) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if d.owners == nil {
		d.owners = map[string]bool{}
	}
	d.owners[r.Header.Get(usagelog.HeaderOwnerID)] = true
	switch r.URL.Path {
	case "/api/session/release":
		d.released++
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	case "/health":
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "identity": d.identity})
	case "/api/page/cookies":
		var p browser.CookieParams
		_ = json.NewDecoder(r.Body).Decode(&p)
		d.calls = append(d.calls, p)
		host := ""
		if u, err := url.Parse(p.URL); err == nil {
			host = u.Hostname()
		}
		switch p.Action {
		case browser.CookieActionList:
			var out []browser.Cookie
			for _, c := range d.jar {
				cd := strings.TrimPrefix(c.Domain, ".")
				if host == cd || strings.HasSuffix(host, "."+cd) {
					out = append(out, c)
				}
			}
			_ = json.NewEncoder(w).Encode(browser.CookieResult{Action: p.Action, Cookies: out, Count: len(out)})
		case browser.CookieActionSet:
			if d.refuseSet {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "site consent: https://" + host + " is not granted act"})
				return
			}
			domain := p.Domain
			if domain == "" {
				domain = host
			}
			c := browser.Cookie{Name: p.Name, Value: p.Value, Domain: domain, Path: p.Path, Secure: p.Secure, HTTPOnly: p.HTTPOnly, SameSite: p.SameSite, Expires: p.Expires}
			d.jar = append(d.jar, c)
			_ = json.NewEncoder(w).Encode(browser.CookieResult{Action: p.Action, Cookie: &c, Cookies: []browser.Cookie{c}})
		case browser.CookieActionDelete:
			kept := d.jar[:0]
			for _, c := range d.jar {
				if c.Name == p.Name && (p.Domain == "" || c.Domain == p.Domain) {
					continue
				}
				kept = append(kept, c)
			}
			d.jar = kept
			_ = json.NewEncoder(w).Encode(browser.CookieResult{Action: p.Action})
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func directID(name, udd string) brwidentity.Identity {
	return brwidentity.Identity{Profile: name, Transport: brwidentity.TransportDirectCDP, UserDataDir: udd}
}

func sessionCookies() []browser.Cookie {
	return []browser.Cookie{
		{Name: "SID", Value: "secret-sid-value", Domain: ".example.com", Path: "/", Secure: true, HTTPOnly: true, Expires: 4102444800},
		{Name: "__Host-t", Value: "secret-host-value", Domain: "example.com", Path: "/", Secure: true, Session: true},
		{Name: "other", Value: "unrelated", Domain: ".other.test", Path: "/"},
	}
}

func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func dailyChromeDir(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, "Library", "Application Support", "Google", "Chrome")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func twoProfilePolicy(src, dst *fakeDaemon, srcUDD, dstUDD string) profilepolicy.Policy {
	return profilepolicy.Policy{Profiles: []profilepolicy.Profile{
		{Name: "source", DirectCDPAllowed: true, UserDataDir: srcUDD, BridgeHTTPAddr: src.addr()},
		{Name: "dest", DirectCDPAllowed: true, UserDataDir: dstUDD, BridgeHTTPAddr: dst.addr()},
	}}
}

func TestCopyMovesOneSiteAndReturnsNoValues(t *testing.T) {
	home := isolatedHome(t)
	srcUDD, dstUDD := filepath.Join(home, ".brw", "profiles", "source"), filepath.Join(home, ".brw", "profiles", "dest")
	src := newFakeDaemon(t, directID("source", srcUDD), sessionCookies()...)
	dst := newFakeDaemon(t, directID("dest", dstUDD))
	policy := twoProfilePolicy(src, dst, srcUDD, dstUDD)

	result, err := CopyDomain(context.Background(), policy, "source", "dest", "www.Example.com", "move")
	if err != nil {
		t.Fatal(err)
	}
	if result.Copied != 2 || result.Removed != 2 || result.Health != HealthCopiedUnverified || result.Domain != "example.com" {
		t.Fatalf("result = %+v", result)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "secret") {
		t.Fatalf("result carries a cookie value: %s", encoded)
	}
	names := map[string]string{}
	for _, c := range dst.jar {
		names[c.Name] = c.Domain
	}
	if names["SID"] != ".example.com" || names["__Host-t"] != "example.com" || names["other"] != "" {
		t.Fatalf("destination jar = %+v", dst.jar)
	}
	if len(src.jar) != 1 || src.jar[0].Name != "other" {
		t.Fatalf("move left the source jar as %+v", src.jar)
	}
	if src.released != 1 || dst.released != 1 {
		t.Fatalf("tab leases not handed back: src %d dst %d", src.released, dst.released)
	}
}

func TestCopyRunsAsItsOwnLeaseOwner(t *testing.T) {
	home := isolatedHome(t)
	t.Setenv("BRW_OWNER_ID", "the-agent-that-ran-brwctl")
	srcUDD, dstUDD := filepath.Join(home, ".brw", "profiles", "source"), filepath.Join(home, ".brw", "profiles", "dest")
	src := newFakeDaemon(t, directID("source", srcUDD), sessionCookies()...)
	dst := newFakeDaemon(t, directID("dest", dstUDD))
	if _, err := CopyDomain(context.Background(), twoProfilePolicy(src, dst, srcUDD, dstUDD), "source", "dest", "example.com", "copy"); err != nil {
		t.Fatal(err)
	}
	agent, _ := httpclient.New("http://127.0.0.1:1", 0)
	for _, d := range []*fakeDaemon{src, dst} {
		if len(d.owners) != 1 {
			t.Fatalf("calls came from several lease owners: %v", d.owners)
		}
		for owner := range d.owners {
			if owner == "" || owner == agent.OwnerID() {
				t.Fatalf("the roster ran under the launching agent's lease owner %q", owner)
			}
		}
	}
}

func TestCopyNeverWritesIntoADailyBrowserProfile(t *testing.T) {
	home := isolatedHome(t)
	daily := dailyChromeDir(t, home)
	srcUDD := filepath.Join(home, ".brw", "profiles", "source")
	tests := []struct {
		name      string
		policyUDD string
		liveUDD   string
	}{
		{"the policy names the daily profile", daily, filepath.Join(home, ".brw", "profiles", "dest")},
		{"the policy names a directory inside it", filepath.Join(daily, "Profile 3"), filepath.Join(home, ".brw", "profiles", "dest")},
		{"the policy names it with a tilde", "~/Library/Application Support/Google/Chrome", filepath.Join(home, ".brw", "profiles", "dest")},
		{"the live daemon drives it", filepath.Join(home, ".brw", "profiles", "dest"), daily},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newFakeDaemon(t, directID("source", srcUDD), sessionCookies()...)
			dst := newFakeDaemon(t, directID("dest", tt.liveUDD))
			policy := twoProfilePolicy(src, dst, srcUDD, profilepolicy.ExpandPath(tt.policyUDD))
			_, err := CopyDomain(context.Background(), policy, "source", "dest", "example.com", "copy")
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if dst.writes() != 0 || len(src.calls) != 0 {
				t.Fatalf("a refused copy touched a daemon: src %+v dst %+v", src.calls, dst.calls)
			}
		})
	}
}

func TestCopyReadsNoDailyBrowserProfile(t *testing.T) {
	home := isolatedHome(t)
	daily := dailyChromeDir(t, home)
	dstUDD := filepath.Join(home, ".brw", "profiles", "dest")
	src := newFakeDaemon(t, directID("source", daily), sessionCookies()...)
	dst := newFakeDaemon(t, directID("dest", dstUDD))
	policy := twoProfilePolicy(src, dst, filepath.Join(home, ".brw", "profiles", "source"), dstUDD)
	if _, err := CopyDomain(context.Background(), policy, "source", "dest", "example.com", "copy"); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if len(src.calls) != 0 {
		t.Fatalf("cookies were listed from a daily profile: %+v", src.calls)
	}
}

func TestCopyIsRefusedOffTheDirectCDPLane(t *testing.T) {
	home := isolatedHome(t)
	srcUDD, dstUDD := filepath.Join(home, ".brw", "profiles", "source"), filepath.Join(home, ".brw", "profiles", "dest")
	for _, transport := range []string{
		brwidentity.TransportExtensionBridge,
		brwidentity.TransportChromeOptIn,
		brwidentity.TransportRemoteCDP,
		brwidentity.TransportOffHostCDP,
		"",
	} {
		for _, end := range []string{"source", "dest"} {
			t.Run(end+"/"+transport, func(t *testing.T) {
				srcID, dstID := directID("source", srcUDD), directID("dest", dstUDD)
				if end == "source" {
					srcID.Transport = transport
				} else {
					dstID.Transport = transport
				}
				src := newFakeDaemon(t, srcID, sessionCookies()...)
				dst := newFakeDaemon(t, dstID)
				policy := twoProfilePolicy(src, dst, srcUDD, dstUDD)
				if _, err := CopyDomain(context.Background(), policy, "source", "dest", "example.com", "move"); !errors.Is(err, ErrRefused) {
					t.Fatalf("err = %v, want a refusal", err)
				}
				if len(src.calls) != 0 || len(dst.calls) != 0 {
					t.Fatalf("a refused copy reached the cookie API: src %+v dst %+v", src.calls, dst.calls)
				}
			})
		}
	}
}

func TestCopyIsRefusedByThePolicy(t *testing.T) {
	home := isolatedHome(t)
	srcUDD, dstUDD := filepath.Join(home, ".brw", "profiles", "source"), filepath.Join(home, ".brw", "profiles", "dest")
	tests := []struct {
		name   string
		mutate func(*profilepolicy.Policy)
	}{
		{"destination is an extension-bridge profile", func(p *profilepolicy.Policy) {
			p.Profiles[1].DirectCDPAllowed = false
			p.Profiles[1].ExtensionBridgeAllowed = true
		}},
		{"source is an extension-bridge profile", func(p *profilepolicy.Policy) {
			p.Profiles[0].DirectCDPAllowed = false
			p.Profiles[0].ExtensionBridgeAllowed = true
		}},
		{"destination daemon is off this machine", func(p *profilepolicy.Policy) {
			p.Profiles[1].BridgeHTTPAddr = "100.64.0.7:17310"
		}},
		{"destination daemon answers as another profile", func(p *profilepolicy.Policy) {
			p.Profiles[1].BridgeHTTPAddr = p.Profiles[0].BridgeHTTPAddr
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newFakeDaemon(t, directID("source", srcUDD), sessionCookies()...)
			dst := newFakeDaemon(t, directID("dest", dstUDD))
			policy := twoProfilePolicy(src, dst, srcUDD, dstUDD)
			tt.mutate(&policy)
			if _, err := CopyDomain(context.Background(), policy, "source", "dest", "example.com", "move"); !errors.Is(err, ErrRefused) {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if src.writes() != 0 || dst.writes() != 0 || len(src.jar) != 3 {
				t.Fatalf("a refused copy wrote: src %+v dst %+v", src.calls, dst.calls)
			}
		})
	}
}

func TestMoveLeavesTheSourceWhenTheDestinationRefuses(t *testing.T) {
	home := isolatedHome(t)
	srcUDD, dstUDD := filepath.Join(home, ".brw", "profiles", "source"), filepath.Join(home, ".brw", "profiles", "dest")
	src := newFakeDaemon(t, directID("source", srcUDD), sessionCookies()...)
	dst := newFakeDaemon(t, directID("dest", dstUDD))
	dst.refuseSet = true
	_, err := CopyDomain(context.Background(), twoProfilePolicy(src, dst, srcUDD, dstUDD), "source", "dest", "example.com", "move")
	if err == nil || !strings.Contains(err.Error(), "not granted") {
		t.Fatalf("err = %v, want the destination's refusal", err)
	}
	if src.writes() != 0 || len(src.jar) != 3 {
		t.Fatalf("the source was modified after a failed copy: %+v", src.calls)
	}
}

func TestCopyValidatesItsArguments(t *testing.T) {
	policy := profilepolicy.Policy{Profiles: []profilepolicy.Profile{{Name: "a", DirectCDPAllowed: true}}}
	for _, tt := range []struct{ from, to, domain, mode string }{
		{"a", "a", "example.com", "copy"},
		{"a", "b", "", "copy"},
		{"a", "b", "https://example.com/x", "copy"},
		{"a", "b", "example.com", "swap"},
	} {
		if _, err := CopyDomain(context.Background(), policy, tt.from, tt.to, tt.domain, tt.mode); err == nil {
			t.Errorf("CopyDomain(%+v) succeeded", tt)
		}
	}
}
