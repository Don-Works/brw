package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/baseline"
	"github.com/Don-Works/brw/internal/recipe"
)

const unownedDigest = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

const providerRecipeOrigin = "https://billing.example.test"

func TestBaselineOfAPageTheProviderReachesIsRefusedNotWrittenLocally(t *testing.T) {
	const privateOrigin = providerRecipeOrigin

	tests := []struct {
		name string

		pageURL string
		digest  string

		wantRefusal string
		wantLocal   int

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

			if len(provider.askedAbout) == 0 || provider.askedAbout[0] != tc.pageURL {
				t.Fatalf("the provider was asked about %v, want the page the capture is of", provider.askedAbout)
			}
		})
	}
}

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

type upstreamRouter struct {
	route recipe.BaselineRoute
	err   error
}

func (u upstreamRouter) RouteBaseline(context.Context, string, string) (recipe.BaselineRoute, error) {
	return u.route, u.err
}

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

			if len(provider.askedAbout) != 0 {
				t.Fatalf("the provider was asked to place a capture with no page: %q", provider.askedAbout)
			}
		})
	}
}
