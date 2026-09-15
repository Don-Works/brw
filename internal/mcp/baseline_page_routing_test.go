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

// providerRecipeOrigin is the one site the test provider's recipe declares.
// Nothing else is a page that recipe visits, which is what makes a capture
// taken anywhere else under its digest a refusal.
const providerRecipeOrigin = "https://billing.example.test"

// TestBaselineOfAPageTheProviderReachesIsRefusedNotWrittenLocally is the
// property the digest alone cannot give, in both directions.
//
// recipe_digest is a caller argument. An agent sitting on a signed-in page a
// private recipe reached can pass any 64-hex string the provider does not own,
// and routing on that argument alone writes the screenshot and the page's
// accessible names — which carry its text — to the local root. The same agent
// can pass one the provider DOES own, and routing on the argument alone then
// POSTs that capture to the provider, off the machine, under the key of a
// recipe that never goes there. The page is therefore half the question either
// way, and a pair that disagrees is a refusal rather than a destination.
func TestBaselineOfAPageTheProviderReachesIsRefusedNotWrittenLocally(t *testing.T) {
	const privateOrigin = providerRecipeOrigin

	tests := []struct {
		name string
		// pageURL is what the tab is showing when the call arrives.
		pageURL string
		digest  string
		// wantRefusal is the phrase the refusal has to carry, or empty when the
		// capture is meant to be stored.
		wantRefusal string
		wantLocal   int
		// wantProvider is how many captures reached the provider, because "not
		// written locally" is only half of a refusal: the other destination is
		// off this machine entirely.
		wantProvider int
	}{
		{
			name:    "an invented digest on a page the provider's recipes reach",
			pageURL: privateOrigin + "/invoices?month=3", digest: unownedDigest,
			wantRefusal: "not one of its recipes",
		},
		{
			name:    "the same invented digest on a page no recipe reaches",
			pageURL: "https://fixtures.example.test/report", digest: unownedDigest,
			wantLocal: 1,
		},
		{
			name:    "the provider's own recipe on its own page",
			pageURL: privateOrigin + "/invoices", digest: fixtureBaselineDigest,
			wantProvider: 1,
		},
		{
			// The mirror of the first row, and the one an owned digest opens:
			// the provider holds this recipe, so routing on the digest alone
			// sends a screenshot and the accessible names of an unrelated
			// signed-in page to the provider's /v1/baselines/put — off this
			// machine — under the key of a recipe that never goes there.
			name:    "the provider's own recipe on a page that recipe never visits",
			pageURL: "https://mail.unrelated.test/inbox/secret-thread", digest: fixtureBaselineDigest,
			wantRefusal: "does not visit the page",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			controller := &pageController{label: "Pay invoice", encoding: "jpeg", url: tc.pageURL}
			s := baselineServer(t, controller)
			provider := newRecordingBaselines(fixtureBaselineDigest)
			provider.origins[privateOrigin] = true
			provider.visits[privateOrigin] = true
			s.SetRecipeBaselines(provider)
			localRoot := s.baselines.Root()

			args := fmt.Sprintf(`{"action":"update","recipe_digest":%q,"step_index":1}`, tc.digest)
			result, rpcErr := s.callTool(context.Background(), baselineToolName, []byte(args))
			if rpcErr != nil {
				t.Fatalf("callTool: %+v", rpcErr)
			}
			failed, message := isToolError(t, result)
			if failed != (tc.wantRefusal != "") {
				t.Fatalf("refused = %v (%s), want %v", failed, message, tc.wantRefusal != "")
			}
			if tc.wantRefusal != "" && !strings.Contains(message, tc.wantRefusal) {
				t.Fatalf("refusal = %q, which does not say why the digest and the page disagree (want %q)", message, tc.wantRefusal)
			}
			if got := localBaselineFiles(t, localRoot); got != tc.wantLocal {
				t.Fatalf("%d baselines under the local root, want %d", got, tc.wantLocal)
			}
			if len(provider.records) != tc.wantProvider {
				t.Fatalf("%d captures reached the provider, want %d", len(provider.records), tc.wantProvider)
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
	provider.origins[providerRecipeOrigin] = true
	provider.visits[providerRecipeOrigin] = true
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
	page := "https://billing.example.test/invoices"
	tests := []struct {
		name        string
		route       recipe.BaselineRoute
		wantRefusal string
		wantLocal   int
	}{
		{
			name: "a recipe the upstream provider owns, on a page it visits",
			route: recipe.NewBaselineRoute(recipe.BaselineOwnership{
				PageURL: page, OwnsRecipe: true, RecipeVisitsPage: true, OwnsPageOrigin: true,
			}),
			wantRefusal: "--upstream-http",
		},
		{
			name: "a page the upstream provider's recipes reach",
			route: recipe.NewBaselineRoute(recipe.BaselineOwnership{
				PageURL: page, OwnsPageOrigin: true,
			}),
			wantRefusal: "not one of its recipes",
		},
		{
			name: "a recipe the upstream provider owns, on a page it never visits",
			route: recipe.NewBaselineRoute(recipe.BaselineOwnership{
				PageURL: "https://mail.unrelated.test/inbox/secret-thread", OwnsRecipe: true,
			}),
			wantRefusal: "does not visit the page",
		},
		{
			name:      "a public fixture nothing upstream claims",
			route:     recipe.NewBaselineRoute(recipe.BaselineOwnership{PageURL: page}),
			wantLocal: 1,
		},
		{
			// The zero value: a router that never classified the capture. Read
			// as "nobody claimed it" this writes a private page's screenshot to
			// the proxy's disk, so it is a refusal instead.
			name:        "an answer this daemon does not classify",
			route:       recipe.BaselineRoute{},
			wantRefusal: "without a destination",
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

// TestBaselineOfATabThatReportsNoURLIsRefused is the third way to reach the
// destination on the digest alone.
//
// An absent page is how list and delete say "this action captures nothing", so
// the routing rule lets an owned digest through without one. A tab that exists
// and reports no URL is a different thing entirely, and reading it as the first
// would hand an agent back exactly what binding the page took away: name an
// owned digest, capture whatever is on screen, and it is POSTed to the
// provider. Both transports can produce that tab.
func TestBaselineOfATabThatReportsNoURLIsRefused(t *testing.T) {
	for _, action := range []string{"check", "update"} {
		t.Run(action, func(t *testing.T) {
			controller := &pageController{label: "Pay invoice", encoding: "jpeg", blankURL: true}
			s := baselineServer(t, controller)
			provider := newRecordingBaselines(fixtureBaselineDigest)
			provider.origins[providerRecipeOrigin] = true
			provider.visits[providerRecipeOrigin] = true
			s.SetRecipeBaselines(provider)
			localRoot := s.baselines.Root()

			args := fmt.Sprintf(`{"action":%q,"recipe_digest":%q,"step_index":0}`, action, fixtureBaselineDigest)
			result, rpcErr := s.callTool(context.Background(), baselineToolName, []byte(args))
			if rpcErr != nil {
				t.Fatalf("callTool: %+v", rpcErr)
			}
			failed, message := isToolError(t, result)
			if !failed || !strings.Contains(message, "reports no URL") {
				t.Fatalf("capturing a tab with no URL under an owned digest = %v / %q, want a refusal naming the missing page", failed, message)
			}
			if len(provider.records) != 0 {
				t.Fatalf("%d captures of a page brw could not place reached the provider", len(provider.records))
			}
			if got := localBaselineFiles(t, localRoot); got != 0 {
				t.Fatalf("%d captures of a page brw could not place were written to the local root", got)
			}
			// The provider was never asked either: a routing question carrying no
			// page is the question list and delete ask, and this is not that.
			if len(provider.askedAbout) != 0 {
				t.Fatalf("the provider was asked to place a capture with no page: %q", provider.askedAbout)
			}
		})
	}
}
