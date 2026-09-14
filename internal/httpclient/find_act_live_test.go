package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	httpapi "github.com/Don-Works/brw/internal/http"
	"github.com/Don-Works/brw/internal/snapshot"
)

// cachingBrowser is the browser host behind the proxy: an extension bridge whose
// plain Find may be served from the tab's snapshot cache, and whose FindLive
// re-reads the page. The two disagree the way they do after a step that wrote a
// DOM property the cache-validity probe cannot see.
type cachingBrowser struct {
	browser.Controller
	cached  []snapshot.Element
	live    []snapshot.Element
	clicked []string
}

func (c *cachingBrowser) Find(context.Context, snapshot.FindOptions) (snapshot.FindResult, error) {
	return snapshot.FindResult{URL: "https://fixture.test/cart", Elements: c.cached}, nil
}

func (c *cachingBrowser) FindLive(context.Context, snapshot.FindOptions) (snapshot.FindResult, error) {
	return snapshot.FindResult{URL: "https://fixture.test/cart", Elements: c.live}, nil
}

func (c *cachingBrowser) Click(_ context.Context, ref string) (browser.ActionResult, error) {
	c.clicked = append(c.clicked, ref)
	return browser.ActionResult{OK: true, Message: "clicked " + ref}, nil
}

// The daemon opens a working tab for a session that named none, which every
// lease-scoped route goes through before it reaches the handler.
func (c *cachingBrowser) OpenInGroup(context.Context, string, browser.TabGroupOptions) (browser.OpenResult, error) {
	return browser.OpenResult{Tab: browser.Tab{ID: "tab-1"}, Ready: true}, nil
}

func element(ref, role, name string) snapshot.Element {
	return snapshot.Element{Ref: ref, Role: role, Name: name, Visible: true, InViewport: true}
}

func proxyTo(t *testing.T, ctrl browser.Controller) *Controller {
	t.Helper()
	daemon := httptest.NewServer(httpapi.New("", ctrl).Handler())
	t.Cleanup(daemon.Close)
	proxy, err := New(daemon.URL, 10*time.Second)
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}
	return proxy
}

// The whole topology the live-resolution rule has to survive: an MCP process
// started with --upstream-http, whose controller is this proxy, in front of a
// daemon driving an extension bridge. A standalone brw_find with an action goes
// through browser.RunFindAct with the proxy as the finder, and the proxy used to
// have no live search at all — so it fell back to the cached GET and resolved
// from the page as it used to be.
func TestLocateAndActOverTheProxyResolvesFromTheLivePage(t *testing.T) {
	host := &cachingBrowser{
		cached: []snapshot.Element{element("e1", "button", "Add to cart")},
		live: []snapshot.Element{
			element("e1", "button", "Add to cart"),
			element("e2", "button", "Add to wishlist"),
		},
	}
	proxy := proxyTo(t, host)

	_, err := browser.RunFindAct(context.Background(), proxy, browser.FindAct{
		Query: "Add", Role: "button", Action: "click",
	})
	if err == nil {
		t.Fatal("a locate-and-act over the proxy resolved a unique match the live page does not have")
	}
	if !strings.Contains(err.Error(), "refusing to guess") {
		t.Fatalf("err = %v, want the ambiguity the live page has", err)
	}
	if len(host.clicked) != 0 {
		t.Fatalf("the proxy clicked %v after the live page said the match was ambiguous", host.clicked)
	}

	// An ordinary read through the proxy is unchanged: it may still be served
	// from the cache, because nothing is being decided from it.
	read, err := proxy.Find(context.Background(), snapshot.FindOptions{Query: "Add"})
	if err != nil {
		t.Fatalf("read-only find: %v", err)
	}
	if len(read.Elements) != 1 {
		t.Fatalf("read-only find returned %d elements, want the cached one", len(read.Elements))
	}
}

// The same topology with a live page that IS unambiguous acts, so the test above
// is measuring liveness rather than a proxy that can no longer locate anything.
func TestLocateAndActOverTheProxyActsOnTheLiveMatch(t *testing.T) {
	host := &cachingBrowser{
		cached: []snapshot.Element{element("e1", "button", "Add to cart"), element("e2", "button", "Add to wishlist")},
		live:   []snapshot.Element{element("e2", "button", "Add to wishlist")},
	}
	proxy := proxyTo(t, host)

	result, err := browser.RunFindAct(context.Background(), proxy, browser.FindAct{
		Query: "Add", Role: "button", Action: "click",
	})
	if err != nil {
		t.Fatalf("RunFindAct() = %v", err)
	}
	if result.Matched.Ref != "e2" {
		t.Fatalf("acted on %q, want the live page's only match e2", result.Matched.Ref)
	}
	if len(host.clicked) != 1 || host.clicked[0] != "e2" {
		t.Fatalf("clicked %v, want [e2]", host.clicked)
	}
}

// A daemon that cannot answer live is refused rather than believed. A cached
// answer and a live one are identical on the wire, so the daemon marks the live
// one and the proxy requires the mark: without it, "this daemon predates the
// parameter" is indistinguishable from "the page really did have one match", and
// the locate-and-act would act on the difference.
func TestLocateAndActOverTheProxyRefusesAnUnmarkedAnswer(t *testing.T) {
	// A daemon too old to know the parameter ignores the unknown query value and
	// answers 200 with the element list it had.
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/page/find" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snapshot.FindResult{
			URL:      "https://fixture.test/cart",
			Elements: []snapshot.Element{element("e1", "button", "Add to cart")},
		})
	}))
	t.Cleanup(daemon.Close)
	proxy, err := New(daemon.URL, 10*time.Second)
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}

	if _, err := proxy.FindLive(context.Background(), snapshot.FindOptions{Query: "Add"}); !errors.Is(err, browser.ErrFinderNotLive) {
		t.Fatalf("FindLive() = %v, want ErrFinderNotLive", err)
	} else if !strings.Contains(err.Error(), daemon.URL) {
		t.Fatalf("err = %v, want it to name the upstream that could not answer", err)
	}
	// And the locate-and-act that would have resolved from it refuses too,
	// rather than falling back to the plain search.
	_, actErr := browser.RunFindAct(context.Background(), proxy, browser.FindAct{
		Query: "Add", Role: "button", Action: "click",
	})
	if !errors.Is(actErr, browser.ErrFinderNotLive) {
		t.Fatalf("RunFindAct() = %v, want ErrFinderNotLive", actErr)
	}
}
