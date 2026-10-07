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

	read, err := proxy.Find(context.Background(), snapshot.FindOptions{Query: "Add"})
	if err != nil {
		t.Fatalf("read-only find: %v", err)
	}
	if len(read.Elements) != 1 {
		t.Fatalf("read-only find returned %d elements, want the cached one", len(read.Elements))
	}
}

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

func TestLocateAndActOverTheProxyRefusesAnUnmarkedAnswer(t *testing.T) {

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

	_, actErr := browser.RunFindAct(context.Background(), proxy, browser.FindAct{
		Query: "Add", Role: "button", Action: "click",
	})
	if !errors.Is(actErr, browser.ErrFinderNotLive) {
		t.Fatalf("RunFindAct() = %v, want ErrFinderNotLive", actErr)
	}
}
