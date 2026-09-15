package httpapi

import (
	"os"
	"regexp"
	"sort"
	"testing"
)

var (
	routePattern = regexp.MustCompile(`mux\.HandleFunc\("[A-Z]+ (/api/[^"]*)"`)
	// usageEntryPattern reads the allowlist keys out of usage.go rather than the
	// map itself, so a key that is present but unreachable is still comparable.
	usageEntryPattern = regexp.MustCompile(`"(/api/[^"]+)":\s*"brw_`)
)

// routesNotRecorded are the /api/ paths that deliberately have no usage
// operation, each with the reason it is not one.
var routesNotRecorded = map[string]string{
	// A long-lived SSE control-plane stream, not a tool call: one event per
	// session rather than per operation, and its duration is the session's.
	"/api/session/stream": "server-sent event stream, not an operation",
	// Served by the usageMiddleware's /api/artifacts/ prefix fallback, which
	// maps the {id} forms onto the four artifact operations.
	"/api/artifacts/{id}":        "mapped by the artifacts prefix fallback",
	"/api/artifacts/{id}/info":   "mapped by the artifacts prefix fallback",
	"/api/artifacts/{id}/read":   "mapped by the artifacts prefix fallback",
	"/api/artifacts/{id}/search": "mapped by the artifacts prefix fallback",
	// Names the tab a proxying daemon's page-tool report has to poll back into.
	// It is one hop inside another operation, which is already recorded on both
	// daemons, so a ledger entry of its own would double-count the agent's call.
	"/api/browser/active_tab": "internal tab-naming hop inside another operation",
	"/api/roster/board":       "operator profile-manager UI, not an agent tool",
	"/api/roster/self":        "operator profile-manager UI, not an agent tool",
	"/api/roster/profiles":    "operator profile-manager UI, not an agent tool",
	"/api/roster/copy":        "operator profile-manager UI, not an agent tool",
	"/api/roster/pin":         "operator profile-manager UI, not an agent tool",
	"/api/roster/open":        "operator profile-manager UI, not an agent tool",
}

// TestEveryAPIRouteHasAUsageOperation pins the route table to the usage
// allowlist. usageOperations is an allowlist by design — an unknown path is
// never copied to the ledger, because a caller could put a secret in one — and
// the cost of that design is that a route added without an entry records
// nothing, silently and forever. Two separate reviews found new routes missing
// from it, which is the argument for checking it here rather than in review.
func TestEveryAPIRouteHasAUsageOperation(t *testing.T) {
	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read the route table: %v", err)
	}
	matches := routePattern.FindAllStringSubmatch(string(source), -1)
	if len(matches) == 0 {
		t.Fatal("found no mux.HandleFunc routes: the registration shape this test reads has changed")
	}

	var missing []string
	seen := map[string]bool{}
	for _, match := range matches {
		path := match[1]
		if seen[path] {
			continue
		}
		seen[path] = true
		if _, exempt := routesNotRecorded[path]; exempt {
			continue
		}
		if usageOperations[path] == "" {
			missing = append(missing, path)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("routes with no usageOperations entry: %v — add one, or list the path in routesNotRecorded with the reason it is not an operation", missing)
	}

	// The other direction: an entry whose route was renamed or removed records
	// nothing and misleads the next reader into thinking it is covered.
	usageSource, err := os.ReadFile("usage.go")
	if err != nil {
		t.Fatalf("read the usage allowlist: %v", err)
	}
	var stale []string
	for _, match := range usageEntryPattern.FindAllStringSubmatch(string(usageSource), -1) {
		if !seen[match[1]] {
			stale = append(stale, match[1])
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("usageOperations entries with no route: %v", stale)
	}
}
