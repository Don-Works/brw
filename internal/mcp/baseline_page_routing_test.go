package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/baseline"
	"github.com/Don-Works/brw/internal/recipe"
)

// unownedDigest is well-formed and belongs to no provider. It is what an agent
// invents when it wants a capture written somewhere the routing would not have
// sent it.
const unownedDigest = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// TestBaselineOfAPageTheProviderReachesIsRefusedNotWrittenLocally is the
// property the digest alone cannot give.
//
// recipe_digest is a caller argument. An agent sitting on a signed-in page a
// private recipe reached can pass any 64-hex string the provider does not own,
// and routing on that argument alone writes the screenshot and the page's
// accessible names — which carry its text — to the local root. The page is
// therefore half the question, and a provider that has a recipe for the page's
// origin but not for this digest is a refusal rather than a destination.
func TestBaselineOfAPageTheProviderReachesIsRefusedNotWrittenLocally(t *testing.T) {
	const privateOrigin = "https://billing.example.test"

	tests := []struct {
		name string
		// pageURL is what the tab is showing when the call arrives.
		pageURL string
		digest  string
		// wantRefused is whether brw must refuse rather than pick a store.
		wantRefused bool
		wantLocal   int
	}{
		{
			name:    "an invented digest on a page the provider's recipes reach",
			pageURL: privateOrigin + "/invoices?month=3", digest: unownedDigest,
			wantRefused: true,
		},
		{
			name:    "the same invented digest on a page no recipe reaches",
			pageURL: "https://fixtures.example.test/report", digest: unownedDigest,
			wantLocal: 1,
		},
		{
			name:    "the provider's own recipe on its own page",
			pageURL: privateOrigin + "/invoices", digest: fixtureBaselineDigest,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			controller := &pageController{label: "Pay invoice", encoding: "jpeg", url: tc.pageURL}
			s := baselineServer(t, controller)
			provider := newRecordingBaselines(fixtureBaselineDigest)
			provider.origins[privateOrigin] = true
			s.SetRecipeBaselines(provider)
			localRoot := s.baselines.Root()

			args := fmt.Sprintf(`{"action":"update","recipe_digest":%q,"step_index":1}`, tc.digest)
			result, rpcErr := s.callTool(context.Background(), baselineToolName, []byte(args))
			if rpcErr != nil {
				t.Fatalf("callTool: %+v", rpcErr)
			}
			failed, message := isToolError(t, result)
			if failed != tc.wantRefused {
				t.Fatalf("refused = %v (%s), want %v", failed, message, tc.wantRefused)
			}
			if tc.wantRefused && !strings.Contains(message, "not one of its recipes") {
				t.Fatalf("refusal = %q, which does not say why the digest and the page disagree", message)
			}
			if got := localBaselineFiles(t, localRoot); got != tc.wantLocal {
				t.Fatalf("%d baselines under the local root, want %d", got, tc.wantLocal)
			}
			// The page reached the routing question at all: without it the
			// refusal above could only ever have come from the digest.
			if len(provider.askedAbout) == 0 || provider.askedAbout[0] != tc.pageURL {
				t.Fatalf("the provider was asked about %v, want the page the capture is of", provider.askedAbout)
			}
		})
	}
}

// TestBaselineListDoesNotRequireAPage: list reads no page and writes nothing,
// so requiring a live tab for it would refuse a call that has nothing to leak.
func TestBaselineListDoesNotRequireAPage(t *testing.T) {
	controller := &pageController{label: "Pay invoice", encoding: "jpeg", url: "https://billing.example.test/invoices"}
	s := baselineServer(t, controller)
	provider := newRecordingBaselines(fixtureBaselineDigest)
	provider.origins["https://billing.example.test"] = true
	s.SetRecipeBaselines(provider)

	result := callBaselineTool(t, s, fmt.Sprintf(`{"action":"list","recipe_digest":%q,"step_index":1}`, unownedDigest))
	if count, _ := result["count"].(float64); count != 0 {
		t.Fatalf("list = %+v, want an empty answer from the local root", result)
	}
	for _, asked := range provider.askedAbout {
		if asked != "" {
			t.Fatalf("list sent the page %q to the provider; it captures nothing", asked)
		}
	}
}

// upstreamRouter answers the routing question and cannot store the answer.
// That is a daemon started with --upstream-http: brw_baseline runs here, and
// the private recipe provider is on the browser host this one proxies.
type upstreamRouter struct {
	route recipe.BaselineRoute
	err   error
}

func (u upstreamRouter) RouteBaseline(context.Context, string, string) (recipe.BaselineRoute, error) {
	return u.route, u.err
}

// TestBaselineOnAProxyingDaemonRefusesWhatBelongsWithTheProvider covers the
// deployment README.md and docs/mcp-client-config.md document: an MCP daemon
// whose recipe API is proxied upstream.
//
// brw_baseline has no HTTP route, so it runs on the proxy while the provider
// that owns the recipe is upstream. Answering that from the proxy's own
// --baseline-root would put a private page's screenshot on this machine's disk
// with the tool description saying it could not happen. The refusal is named
// and narrow: a public fixture still gates here.
func TestBaselineOnAProxyingDaemonRefusesWhatBelongsWithTheProvider(t *testing.T) {
	tests := []struct {
		name        string
		route       recipe.BaselineRoute
		wantRefusal string
		wantLocal   int
	}{
		{
			name:        "a recipe the upstream provider owns",
			route:       recipe.BaselineRoute{OwnsRecipe: true},
			wantRefusal: "--upstream-http",
		},
		{
			name:        "a page the upstream provider's recipes reach",
			route:       recipe.BaselineRoute{OwnsOrigin: true},
			wantRefusal: "not one of its recipes",
		},
		{
			name:      "a public fixture nothing upstream claims",
			route:     recipe.BaselineRoute{},
			wantLocal: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			controller := &pageController{label: "Pay invoice", encoding: "jpeg"}
			s := baselineServer(t, controller)
			s.SetBaselineRouter(upstreamRouter{route: tc.route})
			localRoot := s.baselines.Root()

			args := fmt.Sprintf(`{"action":"update","recipe_digest":%q,"step_index":0}`, fixtureBaselineDigest)
			result, rpcErr := s.callTool(context.Background(), baselineToolName, []byte(args))
			if rpcErr != nil {
				t.Fatalf("callTool: %+v", rpcErr)
			}
			failed, message := isToolError(t, result)
			if failed != (tc.wantRefusal != "") {
				t.Fatalf("refused = %v (%s), want %v", failed, message, tc.wantRefusal != "")
			}
			if tc.wantRefusal != "" && !strings.Contains(message, tc.wantRefusal) {
				t.Fatalf("refusal = %q, want it to name %q", message, tc.wantRefusal)
			}
			if got := localBaselineFiles(t, localRoot); got != tc.wantLocal {
				t.Fatalf("%d baselines under this proxy's local root, want %d", got, tc.wantLocal)
			}
		})
	}
}

// TestBaselineOnAProxyingDaemonFailsWhenTheRouteCannotBeAsked: an unanswerable
// routing question is exactly the case where using the local root is wrong.
func TestBaselineOnAProxyingDaemonFailsWhenTheRouteCannotBeAsked(t *testing.T) {
	controller := &pageController{label: "Pay invoice", encoding: "jpeg"}
	s := baselineServer(t, controller)
	s.SetBaselineRouter(upstreamRouter{err: fmt.Errorf("the browser host is unreachable")})
	localRoot := s.baselines.Root()

	args := fmt.Sprintf(`{"action":"update","recipe_digest":%q,"step_index":0}`, fixtureBaselineDigest)
	result, rpcErr := s.callTool(context.Background(), baselineToolName, []byte(args))
	if rpcErr != nil {
		t.Fatalf("callTool: %+v", rpcErr)
	}
	failed, message := isToolError(t, result)
	if !failed || !strings.Contains(message, "unreachable") {
		t.Fatalf("result = %v / %q, want a refusal naming the unreachable host", failed, message)
	}
	if got := localBaselineFiles(t, localRoot); got != 0 {
		t.Fatalf("%d captures fell back to the proxy's local root", got)
	}
}

// TestBaselineStorageIsStillLocalWithNoRouter: a daemon with no private
// provider at all routes everything to the local root, which is the behaviour
// that existed before there was anywhere else to put a baseline.
func TestBaselineStorageIsStillLocalWithNoRouter(t *testing.T) {
	controller := &pageController{label: "Pay invoice", encoding: "jpeg", url: "https://billing.example.test/invoices"}
	s := baselineServer(t, controller)

	result := callBaselineTool(t, s, fmt.Sprintf(`{"action":"update","recipe_digest":%q,"step_index":0}`, unownedDigest))
	if status, _ := result["status"].(string); status != baseline.StatusRecorded {
		t.Fatalf("update with no provider = %+v", result)
	}
	if got := localBaselineFiles(t, s.baselines.Root()); got != 1 {
		t.Fatalf("%d baselines under the local root, want the one just recorded", got)
	}
}
