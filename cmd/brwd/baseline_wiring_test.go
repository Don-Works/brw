package main

import (
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/recipe"
)

// TestProxyControllerCanAnswerBaselineRouting pins the assertion the proxy
// branch of main makes.
//
// brw_baseline has no HTTP route: it runs on whichever daemon the agent talks
// to, so on a --upstream-http proxy it runs there while the private recipe
// provider is on the browser host. If the proxy controller stopped satisfying
// recipe.BaselineRouter, that daemon would route every capture to its own
// --baseline-root — including captures of pages the provider's recipes reach,
// with the tool description saying it cannot happen. main refuses to start in
// that case; this is what keeps the refusal unreachable.
func TestProxyControllerCanAnswerBaselineRouting(t *testing.T) {
	upstream, err := httpclient.New("http://127.0.0.1:1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Through browser.Controller, which is the static type main holds.
	var controller browser.Controller = upstream
	if _, ok := controller.(recipe.BaselineRouter); !ok {
		t.Fatalf("the upstream controller (%T) cannot answer baseline routing", controller)
	}
}

// TestBaselineStartupLinesNameTheDestination: where a private page's screenshot
// lands differs per deployment, and an operator finds out from this line or
// from the file appearing somewhere they did not expect.
func TestBaselineStartupLinesNameTheDestination(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
	}{
		{
			name: "a provider that holds no baselines",
			line: recipeBaselineStatusLine(nil),
			want: []string{"holds no baselines", "--baseline-root"},
		},
		{
			name: "a proxying daemon",
			line: proxyBaselineStatusLine(),
			want: []string{"asks the browser host", "refused here"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, want := range tc.want {
				if !strings.Contains(tc.line, want) {
					t.Fatalf("startup line %q does not mention %q", tc.line, want)
				}
			}
		})
	}
}

// TestRecipeBaselinesForRejectsAProviderWithoutTheCapability: a provider that
// cannot hold baselines must come back nil rather than as a store whose first
// write fails, because nil is what makes the daemon fall back to the local root
// deliberately instead of at the first capture.
func TestRecipeBaselinesForRejectsAProviderWithoutTheCapability(t *testing.T) {
	if got := recipeBaselinesFor(providerWithoutBaselines{}); got != nil {
		t.Fatalf("a provider with no baseline capability was used as a store: %T", got)
	}
}

// providerWithoutBaselines is a recipe.Provider and nothing more, which is what
// a custom provider is allowed to be.
type providerWithoutBaselines struct{ recipe.Provider }
